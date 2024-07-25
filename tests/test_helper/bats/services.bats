
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

services_register_test_functions() {
    bats_test_function \
        --description "Enable already enabled service" \
        -- enable_already_enabled_service
    bats_test_function \
        --description "Enable not existing service" \
        -- enable_not_existing_service
    bats_test_function \
        --description "Disable non existing service" \
        -- disable_non_existing_service
    bats_test_function \
        --description "Disable enabled service " \
        -- disable_enabled_service
    bats_test_function \
        --description "Disable non enabled service" \
        -- disable_non_enabled_service
    bats_test_function \
        --description "Enable disabled service" \
        -- enable_disabled_service
}

enable_disabled_service() {
    for container in $TEST_CONTAINERS; do
        run lxc_exec "$container" "microovn enable switch"
        assert_output "Service switch enabled"
    done
}

disable_enabled_service() {
    for container in $TEST_CONTAINERS; do
        run lxc_exec "$container" "microovn disable switch"
        assert_output "Service switch disabled"
    done
}

disable_non_enabled_service() {
    for container in $TEST_CONTAINERS; do
        run lxc_exec "$container" "microovn disable switch"
        assert_output "Error: command failed: No such service"
    done
}

disable_non_existing_service() {
    for container in $TEST_CONTAINERS; do
        run lxc_exec "$container" "microovn disable switchh"
        assert_output "Error: command failed: No such service"
    done
}

enable_not_existing_service() {
    for container in $TEST_CONTAINERS; do
        run lxc_exec "$container" "microovn enable switchh"
        assert_output "Error: command failed: Snapctl error, likely due to service not existing:
Failed to run: snapctl start microovn.switchh --enable: exit status 1 (error: error running snapctl: unknown service: \"microovn.switchh\")"
    done
}

enable_already_enabled_service() {
    for container in $TEST_CONTAINERS; do
        run lxc_exec "$container" "microovn enable switch"
        assert_output "Error: command failed: Service already exists"
    done
}

services_register_test_functions
