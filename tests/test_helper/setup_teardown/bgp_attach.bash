# setup_file
#
# This function sets up a minimal topology for testing the BGP attach mode,
# where MicroOVN attaches BIRD to an *externally owned* logical router port
# instead of creating the whole BGP topology itself.
#
#     +-------------+              +-------------------+
#     |  BGP peer   +---- ether ---+   MicroOVN node   |
#     |   (FRR)     |              | (chassis+central) |
#     +-------------+              +-------------------+
#
# The "external owner" (this test) creates the OVS provider bridge and the OVN
# logical router/switch/ports, then stamps the attach markers. MicroOVN only
# attaches BIRD via routing-protocol-redirect.
#
setup_file() {
    load test_helper/common.bash
    load test_helper/lxd.bash
    load test_helper/microovn.bash
    load test_helper/bgp_utils.bash

    TEST_CONTAINER=$(container_names "$BATS_TEST_FILENAME" 1 | tr -d '[:space:]')
    export TEST_CONTAINER

    launch_containers_args "-c linux.kernel_modules=vrf,openvswitch -c security.nesting=true" $TEST_CONTAINER
    wait_containers_ready $TEST_CONTAINER
    install_microovn "$MICROOVN_SNAP_PATH" $TEST_CONTAINER
    setup_snap_aliases $TEST_CONTAINER

    bootstrap_cluster $TEST_CONTAINER

    # Wait until the chassis is registered in the OVN Southbound DB. The chassis
    # name matches the container hostname (the MicroOVN node name).
    wait_until "ovn_chassis_registered $TEST_CONTAINER $TEST_CONTAINER" false 60

    # Single network between the MicroOVN node and the BGP peer. (LXD network
    # names are limited to 15 characters.)
    BGP_INT_NET="ovn-att-net"
    create_lxd_network_no_dhcp "$BGP_INT_NET"

    BGP_PEER="microovn-bgp-attach-peer"
    launch_containers "$BGP_PEER"
    wait_containers_ready "$BGP_PEER"

    BGP_CONTAINER_INT_IFACE="eth1"
    OVN_CONTAINER_INT_IFACE="eth1"

    connect_container_to_network_no_ip "$BGP_PEER" "$BGP_INT_NET" "$BGP_CONTAINER_INT_IFACE"
    connect_container_to_network_no_ip "$TEST_CONTAINER" "$BGP_INT_NET" "$OVN_CONTAINER_INT_IFACE"

    install_frr_bgp $BGP_PEER

    export BGP_PEER
    export BGP_CONTAINER_INT_IFACE
    export OVN_CONTAINER_INT_IFACE
    export BGP_INT_NET
}

teardown_file() {
    print_diagnostics_on_failure $TEST_CONTAINERS
    collect_coverage $TEST_CONTAINERS
    delete_containers "$TEST_CONTAINER $BGP_PEER"
    delete_lxd_network "$BGP_INT_NET"
}