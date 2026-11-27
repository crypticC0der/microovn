# setup_file
#
# Single bootstrapped MicroOVN node + an FRR BGP peer, connected over a shared
# L2 network. The test drives the structured underlay via
# `microovn enable bird --uplink <iface>` and asserts the underlay BGP/bfD
# session comes up end-to-end against the FRR peer.
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

    # Single shared L2 network between the node and the BGP peer (LXD network
    # names are limited to 15 characters).
    BGP_INT_NET="ovn-eb-net"
    create_lxd_network_no_dhcp "$BGP_INT_NET"

    BGP_PEER="microovn-eb-peer"
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