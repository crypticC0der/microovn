package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/canonical/microcluster/v3/microcluster"
	"github.com/spf13/cobra"

	"github.com/canonical/microovn/microovn/api/types"
	"github.com/canonical/microovn/microovn/client"
)

type cmdSetupBird struct {
	common *CmdControl
}

func (c *cmdSetupBird) Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bird",
		Short: "Apply a raw BIRD configuration (read from stdin)",
		Args:  cobra.NoArgs,
		RunE:  c.Run,
	}

	return cmd
}

func (c *cmdSetupBird) Run(_ *cobra.Command, _ []string) error {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to read BIRD configuration from stdin: %w", err)
	}

	m, err := microcluster.App(microcluster.Args{StateDir: c.common.FlagStateDir})
	if err != nil {
		return err
	}

	cli, err := m.LocalClient()
	if err != nil {
		return err
	}

	req := types.SetupBirdRequest{
		Config: string(raw),
	}

	resp, err := client.SetupBird(context.Background(), cli, req)
	if err != nil {
		return fmt.Errorf("BIRD configuration failed: %w", err)
	}

	if !resp.Success {
		fmt.Printf("BIRD configuration failed: %s\n", resp.Message)
		return fmt.Errorf("BIRD configuration did not complete successfully")
	}

	fmt.Println(resp.Message)
	return nil
}
