package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/canonical/microcluster/v3/microcluster"
	"github.com/spf13/cobra"

	"github.com/canonical/microovn/microovn/api/types"
	"github.com/canonical/microovn/microovn/client"
)

// cmdEnableBird is the `microovn enable bird` command: a structured host/fabric
// underlay BIRD render (the non-CLI-"setup bgp-attach"-style answer to raw
// `setup bird`). It is a dedicated child of the `enable` parent, so it coexists
// with `enable <SERVICE>`.
type cmdEnableBird struct {
	common  *CmdControl
	uplinks []string
	config  []string
	stdin   bool
}

func (c *cmdEnableBird) Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bird",
		Short: "Enable structured host/fabric underlay BIRD (or apply a raw config with --stdin)",
		Args:  cobra.NoArgs,
		RunE:  c.Run,
	}

	cmd.Flags().StringArrayVar(&c.uplinks, "uplink", nil, "Host interface for a fabric session (repeatable; at least one required)")
	cmd.Flags().StringArrayVar(&c.config, "config", nil, "Underlay config key=value (asn, router_id, ula)")
	cmd.Flags().BoolVar(&c.stdin, "stdin", false, "Read a raw BIRD configuration from stdin")

	return cmd
}

func (c *cmdEnableBird) Run(_ *cobra.Command, _ []string) error {
	m, err := microcluster.App(microcluster.Args{StateDir: c.common.FlagStateDir})
	if err != nil {
		return err
	}

	cli, err := m.LocalClient()
	if err != nil {
		return err
	}

	req := types.EnableBirdRequest{}

	if c.stdin {
		if len(c.uplinks) > 0 || len(c.config) > 0 {
			return errors.New("--stdin is mutually exclusive with --uplink and --config")
		}
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("failed to read BIRD configuration from stdin: %w", err)
		}
		req.Raw = string(raw)
	} else {
		if len(c.uplinks) == 0 {
			return errors.New("at least one --uplink interface is required (or use --stdin)")
		}
		req.Uplinks = c.uplinks
		configMap, err := parseEnableBirdConfigFlags(c.config)
		if err != nil {
			return err
		}
		req.Config = configMap
	}

	resp, err := client.EnableBird(context.Background(), cli, req)
	if err != nil {
		return fmt.Errorf("enable bird failed: %w", err)
	}
	if !resp.Success {
		return errors.New(resp.Message)
	}

	fmt.Println(resp.Message)
	return nil
}

// parseEnableBirdConfigFlags parses the repeatable --config key=value pairs into
// a map, rejecting duplicate keys and malformed values.
func parseEnableBirdConfigFlags(configStrings []string) (map[string]string, error) {
	rawConfig := map[string]string{}
	for _, configString := range configStrings {
		key, value, found := strings.Cut(configString, "=")
		if !found {
			return nil, fmt.Errorf("configuration '%s' does not conform to the 'key=value' format", configString)
		}
		if _, exists := rawConfig[key]; exists {
			return nil, fmt.Errorf("configuration '%s' already set", key)
		}
		rawConfig[key] = value
	}
	return rawConfig, nil
}
