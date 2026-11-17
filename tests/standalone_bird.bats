load "${ABS_TOP_TEST_DIRNAME}test_helper/setup_teardown/$(basename "${BATS_TEST_FILENAME//.bats/.bash}")"

setup() {
    load ${ABS_TOP_TEST_DIRNAME}test_helper/common.bash
    load ${ABS_TOP_TEST_DIRNAME}test_helper/lxd.bash
    load ${ABS_TOP_TEST_DIRNAME}test_helper/microovn.bash
    load ${ABS_TOP_TEST_DIRNAME}../.bats/bats-support/load.bash
    load ${ABS_TOP_TEST_DIRNAME}../.bats/bats-assert/load.bash

    # Ensure TEST_CONTAINERS is populated, otherwise the tests below will
    # provide false positive results.
    assert [ -n "$TEST_CONTAINERS" ]
}

teardown() {
    print_diagnostics_on_failure $TEST_CONTAINERS
}

standalone_bird_register_test_functions() {
    bats_test_function \
        --description "Run BIRD standalone before bootstrap" \
        -- run_bird_standalone
}

# bird_running CONTAINER — BIRD's control socket answers (daemon is up).
bird_running() {
    local container=$1; shift
    lxc_exec "$container" "microovn.birdc show status"
}

run_bird_standalone() {
    for container in $TEST_CONTAINERS; do
        # Pre-flight: the node is not bootstrapped and BIRD is not running.
        run lxc_exec "$container" "microovn status"
        assert_failure

        run lxc_exec "$container" "microovn.birdc show status"
        assert_failure

        # The BIRD snap service starts on its own, with no MicroOVN bootstrap.
        run lxc_exec "$container" "snap start microovn.bird"
        assert_success

        run lxc_exec "$container" "snap services microovn.bird |
                                   grep -q inactive"
        assert_failure

        run lxc_exec "$container" "microovn.birdc show status"
        assert_success

        # The config file survives a service restart.
        run lxc_exec "$container" "snap restart microovn.bird"
        assert_success
        wait_until "bird_running $container" false 30

        # A missing config file is re-seeded from the default before BIRD starts
        # (fresh install / deleted data dir), so the service does not fail.
        run lxc_exec "$container" "snap stop microovn.bird"
        assert_success
        lxc_exec "$container" "rm -f /var/snap/microovn/common/data/bird/bird.conf"

        run lxc_exec "$container" "snap start microovn.bird"
        assert_success
        wait_until "bird_running $container" false 30
        run lxc_exec "$container" \
            "grep -q 'protocol device' /var/snap/microovn/common/data/bird/bird.conf"
        assert_success
    done
}

standalone_bird_register_test_functions
