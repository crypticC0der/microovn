package main

import (
	"context"
	"fmt"

	"github.com/canonical/microcluster/microcluster"
	"github.com/spf13/cobra"

	"github.com/canonical/microovn/microovn/client"
)

type cmdDisable struct {
	common  *CmdControl
	cluster *cmdCluster
}

func (c *cmdDisable) Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "disable <SERVICE>",
		Short: "disables a service",
		RunE:  c.Run,
	}

	return cmd
}

func (c *cmdDisable) Run(cmd *cobra.Command, args []string) error {
	var err error
	m, err := microcluster.App(microcluster.Args{StateDir: c.common.FlagStateDir, Verbose: c.common.FlagLogVerbose, Debug: c.common.FlagLogDebug})
	if err != nil {
		return err
	}

	cli, err := m.LocalClient()
	if err != nil {
		return err
	}

	targetService := args[0]
	err = client.DisableService(context.Background(), cli, targetService)

	if err != nil {
		return fmt.Errorf("command failed: %s", err)
	}
	return nil
}

type cmdEnable struct {
	common  *CmdControl
	cluster *cmdCluster
}

func (c *cmdEnable) Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "enable <SERVICE>",
		Short: "enables a service",
		RunE:  c.Run,
	}

	return cmd
}

func (c *cmdEnable) Run(cmd *cobra.Command, args []string) error {
	var err error
	m, err := microcluster.App(microcluster.Args{StateDir: c.common.FlagStateDir, Verbose: c.common.FlagLogVerbose, Debug: c.common.FlagLogDebug})
	if err != nil {
		return err
	}

	cli, err := m.LocalClient()
	if err != nil {
		return err
	}

	targetService := args[0]
	err = client.EnableService(context.Background(), cli, targetService)

	if err != nil {
		return fmt.Errorf("command failed: %s", err)
	}
	return nil
}
