package ovn

import (
	"context"
	"fmt"

	microTypes "github.com/canonical/microcluster/v3/microcluster/types"

	"github.com/canonical/lxd/shared/logger"

	ovnCmd "github.com/canonical/microovn/microovn/ovn/cmd"
	"github.com/canonical/microovn/microovn/ovn/inference"
	"github.com/canonical/microovn/microovn/ovn/paths"
)

// configureDocaDatapath switches OVS onto the DOCA-accelerated datapath when
// the host runs the DOCA engine and is ready for it. On a host that is not
// DOCA-capable, runs a different engine, is missing prerequisites, or whose
// hardware cannot be inspected, it does nothing and leaves the kernel datapath
// in place. Only an actual failure to configure OVS is returned to the caller.
func configureDocaDatapath(ctx context.Context, s microTypes.State) error {
	active, err := inference.Active()
	if err != nil {
		logger.Warnf("Could not read the active engine; leaving the kernel datapath in place: %v", err)
		return nil
	}

	if active != inference.DocaEngineName {
		return nil
	}

	engines, err := inference.LoadEngines(paths.EnginesDir())
	if err != nil {
		logger.Warnf("Could not load engine manifests; leaving the kernel datapath in place: %v", err)
		return nil
	}

	hardware, err := inference.Detect()
	if err != nil {
		logger.Warnf("Could not inspect host hardware; leaving the kernel datapath in place: %v", err)
		return nil
	}

	if !inference.UseDocaDatapath(active, inference.AssessDoca(engines, hardware)) {
		logger.Warn("DOCA engine active but the host is not DOCA-ready; leaving the kernel datapath in place")
		return nil
	}

	_, err = ovnCmd.VSCtl(ctx, s, "set", "open_vswitch", ".", "other_config:doca-init=true")
	if err != nil {
		return fmt.Errorf("enabling the DOCA datapath: %w", err)
	}

	_, err = ovnCmd.VSCtl(ctx, s, "set", "open_vswitch", ".", "external_ids:ovn-bridge-datapath-type=doca")
	if err != nil {
		return fmt.Errorf("configuring the DOCA datapath type: %w", err)
	}

	return nil
}
