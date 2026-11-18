package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/canonical/microovn/microovn/ovn/inference"
	"github.com/canonical/microovn/microovn/ovn/paths"
)

type cmdInference struct {
	common *CmdControl
}

func (c *cmdInference) Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inference",
		Short: "Inspect and select the Open vSwitch engine for this host",
	}

	cmd.AddCommand(c.listCommand())
	cmd.AddCommand(c.autoCommand())
	cmd.AddCommand(c.statusCommand())

	return cmd
}

func (c *cmdInference) listCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available OVS engines and their compatibility with this host",
		RunE: func(_ *cobra.Command, _ []string) error {
			hardware, err := inference.Detect()
			if err != nil {
				return err
			}

			engines, err := inference.LoadEngines(paths.EnginesDir())
			if err != nil {
				return err
			}

			active, err := inference.Active()
			if err != nil {
				return err
			}

			writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

			if _, err := fmt.Fprintln(writer, "ENGINE\tCOMPATIBLE\tSCORE\tACTIVE\tMISSING\tSUMMARY"); err != nil {
				return err
			}

			for _, engine := range engines {
				match := inference.Score(engine, hardware)

				compatible := "no"
				if match.Compatible && match.Score > 0 {
					compatible = "yes"
				}

				marker := ""
				if engine.Name == active {
					marker = "*"
				}

				missing := "-"
				if len(match.Missing) > 0 {
					missing = strings.Join(match.Missing, ", ")
				}

				_, err := fmt.Fprintf(writer, "%s\t%s\t%d\t%s\t%s\t%s\n",
					engine.Name, compatible, match.Score, marker, missing, engine.Summary)
				if err != nil {
					return err
				}
			}

			return writer.Flush()
		},
	}
}

func (c *cmdInference) autoCommand() *cobra.Command {
	var fallback string

	cmd := &cobra.Command{
		Use:   "auto",
		Short: "Select and activate the optimal OVS engine for this host",
		RunE: func(_ *cobra.Command, _ []string) error {
			engines, err := inference.LoadEngines(paths.EnginesDir())
			if err != nil {
				return err
			}

			hardware, err := inference.Detect()
			if err != nil {
				return activateFallback(engines, fallback, err)
			}

			selected, err := inference.Select(engines, hardware)
			if err != nil {
				return activateFallback(engines, fallback, err)
			}

			fmt.Printf("Selected engine %q for %s\n", selected.Name, hardware.Architecture)

			return inference.Activate(context.Background(), selected)
		},
	}

	cmd.Flags().StringVar(&fallback, "fallback", "", "engine to use when hardware information is unavailable")

	return cmd
}

func (c *cmdInference) statusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the currently active OVS engine",
		RunE: func(_ *cobra.Command, _ []string) error {
			active, err := inference.Active()
			if err != nil {
				return err
			}

			if active == "" {
				fmt.Println("No engine has been selected yet")
				return nil
			}

			fmt.Println(active)

			return nil
		},
	}
}

// activateFallback activates the named fallback engine, or returns the original
// error when no fallback was requested.
func activateFallback(engines []inference.Engine, fallback string, cause error) error {
	if fallback == "" {
		return cause
	}

	for i := range engines {
		if engines[i].Name == fallback {
			fmt.Printf("Hardware information unavailable, falling back to engine %q\n", fallback)
			return inference.Activate(context.Background(), &engines[i])
		}
	}

	return fmt.Errorf("fallback engine %q not found: %w", fallback, cause)
}
