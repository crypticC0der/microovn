# This is a bash shell fragment -*- bash -*-
load "${ABS_TOP_TEST_DIRNAME}test_helper/setup_teardown/$(basename "${BATS_TEST_FILENAME//.bats/.bash}")"

setup() {
    load ${ABS_TOP_TEST_DIRNAME}test_helper/common.bash
    load ${ABS_TOP_TEST_DIRNAME}test_helper/lxd.bash
    load ${ABS_TOP_TEST_DIRNAME}test_helper/microovn.bash
    load ${ABS_TOP_TEST_DIRNAME}test_helper/bgp_utils.bash

    load ${ABS_TOP_TEST_DIRNAME}../.bats/bats-support/load.bash
    load ${ABS_TOP_TEST_DIRNAME}../.bats/bats-assert/load.bash

    assert [ -n "$TEST_CONTAINER" ]
    assert [ -n "$BGP_PEER" ]
}

teardown() {
    print_diagnostics_on_failure "$TEST_CONTAINER $BGP_PEER"

    local chassis="$TEST_CONTAINER"
    local bridge=br-bgp
    local lr=lr-ext
    local ls=ls-ext

    # Reset the FRR peer's running configuration (the test configures it via
    # vtysh, which is additive).
    lxc_exec "$BGP_PEER" "systemctl restart frr"

    # Drop the attach gate and chassis knobs (idempotent; the test may already
    # have removed the gate).
    for key in microovn-bgp microovn-bgp-asn microovn-bgp-router-id microovn-bgp-advertise-table; do
        lxc_exec "$chassis" "microovn.ovs-vsctl remove Open_vSwitch . external-ids $key" || true
    done

    # The bridge and logical router/switch are *externally owned* (the test acts
    # as the owner), so a `disable bgp` would not remove them. Tear them down
    # here so the next test starts clean; deleting the bridge also frees the
    # physical interface that was added to it. The LS is deleted before the LR
    # so the router-type LSP that references the LRP is gone first.
    lxc_exec "$chassis" "microovn.ovs-vsctl --if-exists del-br $bridge" || true
    lxc_exec "$chassis" "microovn.ovs-vsctl remove Open_vSwitch . external-ids ovn-bridge-mappings 2>/dev/null" || true
    lxc_exec "$chassis" "microovn.ovn-nbctl --if-exists ls-del $ls"
    lxc_exec "$chassis" "microovn.ovn-nbctl --if-exists lr-del $lr"

    # Assert the owner topology is fully gone (mirrors the cleanup assertions in
    # bgp_control_plane.bats).
    run lxc_exec "$chassis" "microovn.ovs-vsctl br-exists $bridge"
    assert_failure

    run lxc_exec "$chassis" "microovn.ovn-nbctl show"
    assert_success
    refute_output --partial "$lr"
    refute_output --partial "$ls"
}

# attach_peer_link_local CONTAINER IFACE
#
# Print the IPv6 link-local address of IFACE on CONTAINER.
attach_peer_link_local() {
    local container=$1; shift
    local iface=$1; shift

    lxc_exec "$container" \
        "ip -6 addr show dev $iface | grep -oE 'fe80::[0-9a-f:]+' | head -1"
}

# attach_peer_link_local_is_set CONTAINER IFACE
attach_peer_link_local_is_set() {
    local container=$1; shift
    local iface=$1; shift
    [ -n "$(attach_peer_link_local "$container" "$iface")" ]
}

# attach_vrf_ready CONTAINER TABLE — the OVN-maintained VRF is present.
attach_vrf_ready() {
    local container=$1; shift
    local table=$1; shift
    lxc_exec "$container" "ip link show dev ovnvrf$table"
}

# attach_redirect_set CONTAINER LRP — routing-protocol-redirect is present.
attach_redirect_set() {
    local container=$1; shift
    local lrp=$1; shift
    [ -n "$(lxc_exec "$container" "microovn.ovn-nbctl get Logical_Router_Port $lrp options:routing-protocol-redirect 2>/dev/null")" ]
}

# attach_redirect_gone CONTAINER LRP — routing-protocol-redirect was removed.
attach_redirect_gone() {
    local container=$1; shift
    local lrp=$1; shift
    ! lxc_exec "$container" "microovn.ovn-nbctl get Logical_Router_Port $lrp options:routing-protocol-redirect"
}

# attach_default_route CONTAINER TABLE — default route present in the VRF table.
attach_default_route() {
    local container=$1; shift
    local table=$1; shift
    lxc_exec "$container" "ip route show table $table | grep -q default"
}

# attach_bgp_established CONTAINER — the attach uplink BGP protocol is Established.
attach_bgp_established() {
    local container=$1; shift
    lxc_exec "$container" \
        "microovn.birdc show protocols all uplink_0_peer_0 | grep -qE 'BGP state:[[:space:]]*Established'"
}

# attach_bfd_session_up CONTAINER — a BFD session with the peer is Up. BIRD prints
# "show bfd sessions" rows as "<address> <interface> <state> ...", so the state is
# field 3, not field 2.
attach_bfd_session_up() {
    local container=$1; shift
    lxc_exec "$container" \
        "microovn.birdc show bfd sessions | awk '\$1 ~ /^fe80::/ && \$3 == \"Up\"' | grep -q ."
}

# attach_bfd_session_up_addr CONTAINER ADDR — a BFD session with the exact peer
# address ADDR is Up (numbered peering; ADDR is e.g. 10.0.0.2, not a link-local).
attach_bfd_session_up_addr() {
    local container=$1; shift
    local addr=$1; shift
    lxc_exec "$container" \
        "microovn.birdc show bfd sessions | awk -v a=\"$addr\" '\$1 == a && \$3 == \"Up\"' | grep -q ."
}

# attach_default_route_via CONTAINER TABLE ADDR — the VRF default route exists and
# its next hop is ADDR (a numbered IPv4 neighbour).
attach_default_route_via() {
    local container=$1; shift
    local table=$1; shift
    local addr=$1; shift
    lxc_exec "$container" "ip route show table $table | grep -q \"default via $addr\""
}

# attach_veth_ipv4_count CONTAINER PORT_NAME — prints the number of IPv4 addresses
# on the attachment veth (looked up via dynamic-routing-port-mapping).
attach_veth_ipv4_count() {
    local container=$1; shift
    local port_name=$1; shift
    local iface
    # ovs-vsctl quotes the map value ("att-ext=mpXXXXb"), so strip quotes before
    # splitting on commas and matching the exact entry.
    iface=$(lxc_exec "$container" \
        "microovn.ovs-vsctl get Open_vSwitch . external-ids:dynamic-routing-port-mapping | tr -d '\"' | tr ',' '\n' | grep \"^$port_name=\" | cut -d= -f2")
    lxc_exec "$container" "ip -o addr show dev $iface | grep -c 'inet '"
}

# attach_veth_has_single_ipv4 CONTAINER PORT_NAME — the attachment veth carries
# exactly one IPv4 address (the numbered transit address) and no stale extras.
attach_veth_has_single_ipv4() {
    local container=$1; shift
    local port_name=$1; shift
    [ "$(attach_veth_ipv4_count "$container" "$port_name")" = "1" ]
}

# attach_bird_config_gone CONTAINER — the attach-generated BIRD config was reset.
attach_bird_config_gone() {
    local container=$1; shift
    ! lxc_exec "$container" \
        "grep -q 'Generated by microovn attach mode' /var/snap/microovn/common/data/bird/bird.conf"
}

bgp_attach_register_test_functions() {
    bats_test_function \
        --description "Attach BIRD to an externally owned router port" \
        -- attach_external_router

    bats_test_function \
        --description "Attach BIRD to an externally owned router port (numbered IPv4)" \
        -- attach_external_router_numbered
}

# attach_external_router
#
# Simulates an external topology owner (Neutron/sunbeam/MicroCloud): creates the
# provider bridge and the OVN logical router/switch/ports, stamps the attach
# markers + chassis knobs, then verifies MicroOVN attaches BIRD via
# routing-protocol-redirect, forms an eBGP session with the FRR peer and learns
# the default route. Finally it removes the attach marker (gate left enabled)
# and verifies clean detach through the marker-removal path, then drops the gate.
attach_external_router() {
    local chassis="$TEST_CONTAINER"
    local tor_asn=4200000100
    local host_asn=4210000001
    local router_id=192.0.2.1
    local learn_table=10
    local advert_table=100
    local physnet=extnet
    local bridge=br-bgp
    local lr=lr-ext
    local ls=ls-ext
    local lrp=lrp-ext
    local port_name=att-ext

    # Start the FRR peer on the ToR interface.
    echo "# Starting BGP on $BGP_PEER" >&3
    lxc_exec "$BGP_PEER" "ip link set $BGP_CONTAINER_INT_IFACE up"
    wait_until "attach_peer_link_local_is_set $BGP_PEER $BGP_CONTAINER_INT_IFACE" false 30
    frr_start_bgp_unnumbered "$BGP_PEER" "$tor_asn" "$BGP_CONTAINER_INT_IFACE"

    local peer_ll
    peer_ll=$(attach_peer_link_local "$BGP_PEER" "$BGP_CONTAINER_INT_IFACE")
    echo "# ($BGP_PEER) link-local is $peer_ll" >&3

    echo "# ($chassis) Creating externally owned provider bridge and OVN topology" >&3
    # The "owner" creates the provider bridge and wires the physical port into it.
    lxc_exec "$chassis" "microovn.ovs-vsctl add-br $bridge"
    lxc_exec "$chassis" "microovn.ovs-vsctl add-port $bridge $OVN_CONTAINER_INT_IFACE"
    lxc_exec "$chassis" "microovn.ovs-vsctl set Open_vSwitch . external-ids:ovn-bridge-mappings=$physnet:$bridge"

    # The "owner" creates the logical router/switch/ports and enables dynamic
    # routing so OVN creates the VRF on this chassis. The router is pinned to the
    # local chassis and the LRP carries an explicit MAC and a
    # dynamic-routing-port-name.
    lxc_exec "$chassis" \
        "microovn.ovn-nbctl \
            lr-add $lr \
            -- set Logical_Router $lr options:chassis=$chassis \
            -- set Logical_Router $lr options:dynamic-routing=true \
            -- set Logical_Router $lr options:dynamic-routing-vrf-id=$learn_table \
            -- lrp-add $lr $lrp 02:00:00:00:00:02 \
            -- set Logical_Router_Port $lrp options:dynamic-routing-maintain-vrf=true \
            -- set Logical_Router_Port $lrp options:dynamic-routing-port-name=$port_name \
            -- ls-add $ls \
            -- lsp-add $ls lsp-$lr \
            -- lsp-set-type lsp-$lr router \
            -- lsp-set-options lsp-$lr router-port=$lrp \
            -- lsp-set-addresses lsp-$lr router \
            -- lsp-add $ls lsp-$ls-localnet \
            -- lsp-set-type lsp-$ls-localnet localnet \
            -- lsp-set-options lsp-$ls-localnet network_name=$physnet \
            -- lsp-set-addresses lsp-$ls-localnet unknown"

    echo "# ($chassis) Waiting for OVN to create the VRF ovnvrf$learn_table" >&3
    wait_until "attach_vrf_ready $chassis $learn_table" false 30

    echo "# ($chassis) Stamping attach markers and knobs" >&3
    lxc_exec "$chassis" "microovn.ovs-vsctl set Open_vSwitch . external-ids:microovn-bgp=enabled"
    lxc_exec "$chassis" "microovn.ovs-vsctl set Open_vSwitch . external-ids:microovn-bgp-asn=$host_asn"
    lxc_exec "$chassis" "microovn.ovs-vsctl set Open_vSwitch . external-ids:microovn-bgp-router-id=$router_id"
    lxc_exec "$chassis" "microovn.ovs-vsctl set Open_vSwitch . external-ids:microovn-bgp-advertise-table=$advert_table"
    lxc_exec "$chassis" "microovn.ovn-nbctl set Logical_Router_Port $lrp external_ids:microovn-bgp-attach=true"
    lxc_exec "$chassis" "microovn.ovn-nbctl set Logical_Router_Port $lrp external_ids:microovn-bgp-peers=$tor_asn@$peer_ll"

    echo "# ($chassis) Waiting for the reconciler to attach (next 30s tick)" >&3
    wait_until "attach_redirect_set $chassis $lrp" false 60

    # routing-protocols must be exactly BGP,BFD while attached so OVN redirects
    # both BGP and BFD to the attachment veth.
    run lxc_exec "$chassis" "microovn.ovn-nbctl get Logical_Router_Port $lrp options:routing-protocols"
    assert_success
    assert_output --regexp '^"?BGP,BFD"?$'

    # dynamic-routing-port-mapping must record the attachment veth.
    run lxc_exec "$chassis" "microovn.ovs-vsctl get Open_vSwitch . external-ids:dynamic-routing-port-mapping"
    assert_success
    assert_output --partial "$port_name="

    # The rendered attach BIRD config must be live (uplink protocol present).
    run lxc_exec "$chassis" "microovn.birdc show protocols | grep -c uplink_0_peer_0"
    assert_success
    assert_output 1

    # The live attach BIRD config must carry the BFD stub and passive BFD so the
    # peer's BFD is answered once it initiates (FRR is configured with bfd on).
    run lxc_exec "$chassis" \
        "grep -q 'protocol bfd' /var/snap/microovn/common/data/bird/bird.conf && grep -q 'passive yes' /var/snap/microovn/common/data/bird/bird.conf"
    assert_success

    echo "# ($chassis) Waiting on established BGP with $BGP_PEER" >&3
    wait_until "attach_bgp_established $chassis" false 60

    echo "# ($chassis) Waiting on BFD session with $BGP_PEER" >&3
    wait_until "attach_bfd_session_up $chassis" false 60

    echo "# ($chassis) Waiting on default route learned into the OVN VRF" >&3
    wait_until "attach_default_route $chassis $learn_table" false 60

    # Prove the advertise/export path: drop a chassis-local /32 host route with a
    # kernel metric into the advertise-table, and assert BIRD advertises it to the
    # FRR peer (advertisements -> export filter -> bgp_med=krt_metric).
    local advertised_ip=172.16.99.99
    echo "# ($chassis) Writing $advertised_ip/32 into advertise-table $advert_table" >&3
    lxc_exec "$chassis" "ip route add $advertised_ip/32 dev lo table $advert_table metric 50"

    echo "# ($BGP_PEER) Waiting to receive $advertised_ip/32 over BGP" >&3
    wait_until "container_has_ipv4_route $BGP_PEER $advertised_ip $BGP_CONTAINER_INT_IFACE" false 60
    lxc_exec "$chassis" "ip route del $advertised_ip/32 table $advert_table"

    echo "# ($chassis) Removing the attach marker (gate stays enabled) to detach" >&3
    lxc_exec "$chassis" "microovn.ovn-nbctl remove Logical_Router_Port $lrp external_ids microovn-bgp-attach"

    echo "# ($chassis) Waiting for the reconciler to detach (next 30s tick)" >&3
    wait_until "attach_redirect_gone $chassis $lrp" false 60

    # Marker removal goes through the len(targets)==0 detach branch: the owned
    # port is detached and BIRD is stopped + its attach config reset.
    wait_until "attach_bird_config_gone $chassis" false 30

    # routing-protocols is restored to the owner's original (unset here).
    run lxc_exec "$chassis" "microovn.ovn-nbctl get Logical_Router_Port $lrp options:routing-protocols"
    assert_failure

    # The attachment is torn down without touching the external topology.
    run lxc_exec "$chassis" "microovn.ovs-vsctl get Open_vSwitch . external-ids:dynamic-routing-port-mapping"
    assert_failure

    run lxc_exec "$chassis" "microovn.ovn-nbctl get Logical_Router_Port $lrp mac"
    assert_success
    assert_output --partial '02:00:00:00:00:02'

    # Remove the enable gate for a clean teardown.
    lxc_exec "$chassis" "microovn.ovs-vsctl remove Open_vSwitch . external-ids microovn-bgp"
}

# attach_external_router_numbered
#
# Same shape as attach_external_router, but the uplink is *numbered*: the LRP
# carries a transit IPv4 in its `networks` (10.0.0.1/30) and the peer is an IPv4
# address (10.0.0.2) inside that network. Verifies the numbered path end-to-end:
# the veth gets exactly one IPv4, BGP establishes, BFD comes up against the IPv4
# neighbour, the default is learned via an IPv4 next hop, the advertised /32
# reaches FRR, and detach is clean.
attach_external_router_numbered() {
    local chassis="$TEST_CONTAINER"
    local tor_asn=4200000100
    local host_asn=4210000001
    local router_id=192.0.2.1
    local learn_table=10
    local advert_table=100
    local physnet=extnet
    local bridge=br-bgp
    local lr=lr-ext
    local ls=ls-ext
    local lrp=lrp-ext
    local port_name=att-ext
    local transit_host=10.0.0.1
    local transit_peer=10.0.0.2

    # Start the FRR peer on the ToR interface with a numbered IPv4 address.
    echo "# Starting numbered BGP on $BGP_PEER" >&3
    lxc_exec "$BGP_PEER" "ip link set $BGP_CONTAINER_INT_IFACE up"
    frr_start_bgp_numbered "$BGP_PEER" "$tor_asn" "$BGP_CONTAINER_INT_IFACE" "$transit_peer/30" "$transit_host"

    echo "# ($chassis) Creating externally owned provider bridge and OVN topology" >&3
    lxc_exec "$chassis" "microovn.ovs-vsctl add-br $bridge"
    lxc_exec "$chassis" "microovn.ovs-vsctl add-port $bridge $OVN_CONTAINER_INT_IFACE"
    lxc_exec "$chassis" "microovn.ovs-vsctl set Open_vSwitch . external-ids:ovn-bridge-mappings=$physnet:$bridge"

    # The "owner" creates the numbered router/switch/ports: the LRP now carries a
    # transit IPv4 (10.0.0.1/30) in its networks, as well as the explicit MAC and
    # the dynamic-routing-port-name.
    lxc_exec "$chassis" \
        "microovn.ovn-nbctl \
            lr-add $lr \
            -- set Logical_Router $lr options:chassis=$chassis \
            -- set Logical_Router $lr options:dynamic-routing=true \
            -- set Logical_Router $lr options:dynamic-routing-vrf-id=$learn_table \
            -- lrp-add $lr $lrp 02:00:00:00:00:02 $transit_host/30 \
            -- set Logical_Router_Port $lrp options:dynamic-routing-maintain-vrf=true \
            -- set Logical_Router_Port $lrp options:dynamic-routing-port-name=$port_name \
            -- ls-add $ls \
            -- lsp-add $ls lsp-$lr \
            -- lsp-set-type lsp-$lr router \
            -- lsp-set-options lsp-$lr router-port=$lrp \
            -- lsp-set-addresses lsp-$lr router \
            -- lsp-add $ls lsp-$ls-localnet \
            -- lsp-set-type lsp-$ls-localnet localnet \
            -- lsp-set-options lsp-$ls-localnet network_name=$physnet \
            -- lsp-set-addresses lsp-$ls-localnet unknown"

    echo "# ($chassis) Waiting for OVN to create the VRF ovnvrf$learn_table" >&3
    wait_until "attach_vrf_ready $chassis $learn_table" false 30

    echo "# ($chassis) Stamping attach markers and knobs" >&3
    lxc_exec "$chassis" "microovn.ovs-vsctl set Open_vSwitch . external-ids:microovn-bgp=enabled"
    lxc_exec "$chassis" "microovn.ovs-vsctl set Open_vSwitch . external-ids:microovn-bgp-asn=$host_asn"
    lxc_exec "$chassis" "microovn.ovs-vsctl set Open_vSwitch . external-ids:microovn-bgp-router-id=$router_id"
    lxc_exec "$chassis" "microovn.ovs-vsctl set Open_vSwitch . external-ids:microovn-bgp-advertise-table=$advert_table"
    lxc_exec "$chassis" "microovn.ovn-nbctl set Logical_Router_Port $lrp external_ids:microovn-bgp-attach=true"
    lxc_exec "$chassis" "microovn.ovn-nbctl set Logical_Router_Port $lrp external_ids:microovn-bgp-peers=$tor_asn@$transit_peer"

    echo "# ($chassis) Waiting for the reconciler to attach (next 30s tick)" >&3
    wait_until "attach_redirect_set $chassis $lrp" false 60

    # routing-protocols must be exactly BGP,BFD while attached.
    run lxc_exec "$chassis" "microovn.ovn-nbctl get Logical_Router_Port $lrp options:routing-protocols"
    assert_success
    assert_output --regexp '^"?BGP,BFD"?$'

    # The rendered attach BIRD config must be live and numbered (local + neighbor).
    run lxc_exec "$chassis" "microovn.birdc show protocols | grep -c uplink_0_peer_0"
    assert_success
    assert_output 1
    run lxc_exec "$chassis" \
        "grep -q 'local $transit_host;' /var/snap/microovn/common/data/bird/bird.conf && grep -q 'neighbor $transit_peer as $tor_asn;' /var/snap/microovn/common/data/bird/bird.conf"
    assert_success

    # The veth carries exactly one IPv4 address (the transit host address).
    wait_until "attach_veth_has_single_ipv4 $chassis $port_name" false 30

    echo "# ($chassis) Waiting on established BGP with $BGP_PEER" >&3
    wait_until "attach_bgp_established $chassis" false 60

    echo "# ($chassis) Waiting on BFD session with $BGP_PEER ($transit_peer)" >&3
    wait_until "attach_bfd_session_up_addr $chassis $transit_peer" false 60

    echo "# ($chassis) Waiting on default route learned via $transit_peer in the OVN VRF" >&3
    wait_until "attach_default_route_via $chassis $learn_table $transit_peer" false 60

    # Prove the advertise/export path over the numbered uplink.
    local advertised_ip=172.16.99.99
    echo "# ($chassis) Writing $advertised_ip/32 into advertise-table $advert_table" >&3
    lxc_exec "$chassis" "ip route add $advertised_ip/32 dev lo table $advert_table metric 50"

    echo "# ($BGP_PEER) Waiting to receive $advertised_ip/32 over BGP" >&3
    wait_until "container_has_ipv4_route $BGP_PEER $advertised_ip $BGP_CONTAINER_INT_IFACE" false 60
    lxc_exec "$chassis" "ip route del $advertised_ip/32 table $advert_table"

    echo "# ($chassis) Removing the attach marker (gate stays enabled) to detach" >&3
    lxc_exec "$chassis" "microovn.ovn-nbctl remove Logical_Router_Port $lrp external_ids microovn-bgp-attach"

    echo "# ($chassis) Waiting for the reconciler to detach (next 30s tick)" >&3
    wait_until "attach_redirect_gone $chassis $lrp" false 60
    wait_until "attach_bird_config_gone $chassis" false 30

    # routing-protocols is restored to the owner's original (unset here).
    run lxc_exec "$chassis" "microovn.ovn-nbctl get Logical_Router_Port $lrp options:routing-protocols"
    assert_failure

    # The LRP keeps its own transit IPv4 (external topology untouched).
    run lxc_exec "$chassis" "microovn.ovn-nbctl get Logical_Router_Port $lrp networks"
    assert_success
    assert_output --partial "$transit_host/30"

    # Remove the enable gate for a clean teardown.
    lxc_exec "$chassis" "microovn.ovs-vsctl remove Open_vSwitch . external-ids microovn-bgp"
}

bgp_attach_register_test_functions
