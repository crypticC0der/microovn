package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	lxdCmd "github.com/canonical/lxd/shared/cmd"
	"github.com/canonical/microcluster/v3/microcluster"
	microTypes "github.com/canonical/microcluster/v3/microcluster/types"
	"github.com/spf13/cobra"

	"github.com/canonical/microovn/microovn/api/types"
	"github.com/canonical/microovn/microovn/client"
)

type cmdSetupBgpAttach struct {
	common *CmdControl

	flagRack           string
	flagPeer           string
	flagNetworks       string
	flagAdvertiseTable uint32
	flagUplinkPattern  string
	flagDryRun         bool
	flagDisablePeer    string

	flagLabelRack   string
	flagLabelMember string
	flagLabelClear  bool
}

func (c *cmdSetupBgpAttach) Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bgp-attach",
		Short: "Attach BIRD to the BGP uplink of every member of one rack, peering with that rack's leaf",
		Long: `Attach BIRD to the BGP uplink router port of every member of one rack,
peering with that rack's leaf.

A rack's members are the cluster members labelled with the rack:

  microovn setup bgp-attach label --rack rack1        (run on each member)

Then set how the rack peers, once, on any member:

  microovn setup bgp-attach --peer <asn>@<ip> [--networks <ip>/<len>] --rack rack1

The setting is identified by its --peer; one setting per rack. Members join
and leave the setting as their labels change, with no rerun: labelling a
member adds it, changing its label moves it to the other rack's setting, and
clearing it ("label --clear") or removing the member from the cluster removes
it. Repeating a call changes nothing. Use "disable" to remove a setting (the
labels stay).

The member handling a change writes the router-port keys for every member
in one OVN Northbound transaction; each member sets its own Open_vSwitch
knobs.

  on the uplink router port   networks (numbered only),
                              options:dynamic-routing-maintain-vrf=true,
                              external_ids:microovn-bgp-attach=true,
                              external_ids:microovn-bgp-peers=<peer>
  on Open_vSwitch             microovn-bgp=enabled,
                              microovn-bgp-advertise-table=<table>

Previous router-port values are recorded and put back when a member leaves the
setting or the setting is disabled.

Numbered: give the leaf's IPv4 --peer and the --networks pool as
<first-ip>/<len>. Addresses are handed out from <first-ip> upward within that
subnet, skipping the network, broadcast and peer addresses: 192.0.2.0/28
starts at .1, while 192.0.2.18/28 leaves .16-.17 alone and starts at .18.
Each member gets the lowest free address and keeps it while it stays in the
rack, so the pool needs one address per member of the rack. Each rack needs
its own subnet. The leaf can accept the subnet with "neighbor range".
"status" lists every member's address.

Unnumbered: give an IPv6 link-local --peer and no --networks.

The uplink is the router port matching --uplink-pattern on a router pinned to
the member (options:chassis) whose switch has a localnet port.`,
		Example: `  microovn setup bgp-attach label --rack rack1
  microovn setup bgp-attach --peer 65101@192.0.2.17 --networks 192.0.2.18/28 --rack rack1
  microovn setup bgp-attach --peer 65102@192.0.2.33 --networks 192.0.2.34/28 --rack rack2 --dry-run
  microovn setup bgp-attach --peer 65103@fe80::1 --rack rack3
  microovn setup bgp-attach label --member node-4 --rack rack2
  microovn setup bgp-attach label --clear
  microovn setup bgp-attach status
  microovn setup bgp-attach disable --peer 65102@192.0.2.33
  microovn setup bgp-attach disable`,
		Args: cobra.NoArgs,
		RunE: c.Run,
	}

	cmd.Flags().StringVar(&c.flagPeer, "peer", "", "Leaf peer as <asn>@<ip>; identifies the setting (required)")
	cmd.Flags().StringVar(&c.flagRack, "rack", "", "Rack whose labelled members peer with this leaf (required)")
	cmd.Flags().StringVar(&c.flagNetworks, "networks", "", "IPv4 pool as <first-ip>/<len>; addresses are handed out from <first-ip> upward, e.g. 192.0.2.18/28 (numbered only)")
	cmd.Flags().Uint32Var(&c.flagAdvertiseTable, "advertise-table", types.DefaultBgpAttachAdvertiseTable, "Kernel table BIRD reads advertised /32s from")
	cmd.Flags().StringVar(&c.flagUplinkPattern, "uplink-pattern", types.DefaultBgpAttachUplinkPattern, "Glob matched against router port names to find each member's uplink")
	cmd.Flags().BoolVar(&c.flagDryRun, "dry-run", false, "Validate and print the per-member plan without writing anything")

	disable := &cobra.Command{
		Use:   "disable",
		Short: "Remove the bgp-attach setting for one leaf (or all) and restore its members' previous values",
		Args:  cobra.NoArgs,
		RunE:  c.runDisable,
	}
	disable.Flags().StringVar(&c.flagDisablePeer, "peer", "", "Leaf peer (<asn>@<ip>) whose setting to remove (default: all)")

	status := &cobra.Command{
		Use:   "status",
		Short: "Show every bgp-attach setting and each member's uplink, address and state",
		Args:  cobra.NoArgs,
		RunE:  c.runStatus,
	}
	label := &cobra.Command{
		Use:   "label",
		Short: "Set or clear a member's rack label, which adds, moves or removes it",
		Long: `Set or clear the rack label of a member (default: this member).

Labelling a member adds it to the bgp-attach setting of that rack, if there is
one; changing the label moves it to the other rack's setting; --clear removes
it. A label without a setting is kept until a setting for that rack is made.`,
		Args: cobra.NoArgs,
		RunE: c.runLabel,
	}
	label.Flags().StringVar(&c.flagLabelRack, "rack", "", "Rack name, e.g. rack1")
	label.Flags().BoolVar(&c.flagLabelClear, "clear", false, "Remove the member's rack label")
	label.Flags().StringVar(&c.flagLabelMember, "member", "", "Member to label, as in 'microovn cluster list' (default: this member)")

	cmd.AddCommand(disable, status, label)

	return cmd
}

func (c *cmdSetupBgpAttach) localClient() (microTypes.Client, error) {
	m, err := microcluster.App(microcluster.Args{StateDir: c.common.FlagStateDir})
	if err != nil {
		return nil, err
	}
	return m.LocalClient()
}

func (c *cmdSetupBgpAttach) Run(_ *cobra.Command, _ []string) error {
	switch {
	case c.flagPeer == "":
		return errors.New("--peer is required (e.g. --peer 65101@192.0.2.17)")
	case c.flagRack == "":
		return errors.New("--rack is required (e.g. --rack rack1)")
	}
	cli, err := c.localClient()
	if err != nil {
		return err
	}
	resp, err := client.SetupBgpAttach(context.Background(), cli, types.SetupBgpAttachRequest{
		Rack:           c.flagRack,
		Peer:           c.flagPeer,
		Networks:       c.flagNetworks,
		AdvertiseTable: c.flagAdvertiseTable,
		UplinkPattern:  c.flagUplinkPattern,
		DryRun:         c.flagDryRun,
	})
	if err != nil {
		return fmt.Errorf("bgp-attach failed: %w", err)
	}
	return printAttachResponse(resp)
}

func (c *cmdSetupBgpAttach) runLabel(_ *cobra.Command, _ []string) error {
	switch {
	case c.flagLabelClear && c.flagLabelRack != "":
		return errors.New("use either --rack or --clear, not both")
	case !c.flagLabelClear && c.flagLabelRack == "":
		return errors.New("--rack is required (e.g. --rack rack1), or --clear to remove the label")
	}
	cli, err := c.localClient()
	if err != nil {
		return err
	}
	resp, err := client.SetBgpAttachRack(context.Background(), cli, types.SetBgpAttachRackRequest{
		Member: c.flagLabelMember,
		Rack:   c.flagLabelRack,
		Clear:  c.flagLabelClear,
	})
	if err != nil {
		return fmt.Errorf("bgp-attach label failed: %w", err)
	}
	return printAttachResponse(resp)
}

func (c *cmdSetupBgpAttach) runDisable(_ *cobra.Command, _ []string) error {
	cli, err := c.localClient()
	if err != nil {
		return err
	}
	resp, err := client.DisableBgpAttach(context.Background(), cli, c.flagDisablePeer)
	if err != nil {
		return fmt.Errorf("bgp-attach disable failed: %w", err)
	}
	return printAttachResponse(resp)
}

func (c *cmdSetupBgpAttach) runStatus(_ *cobra.Command, _ []string) error {
	cli, err := c.localClient()
	if err != nil {
		return err
	}
	resp, err := client.GetBgpAttachStatus(context.Background(), cli)
	if err != nil {
		return fmt.Errorf("bgp-attach status failed: %w", err)
	}
	return printAttachResponse(resp)
}

// attachGroupLine renders one setting as a single line.
func attachGroupLine(g types.BgpAttachGroup) string {
	networks := g.Networks
	if networks == "" {
		networks = "(unnumbered)"
	}
	rack := g.Rack
	if rack == "" {
		rack = "(none)"
	}
	members := strings.Join(g.Members, ",")
	if members == "" {
		members = "(none yet)"
	}
	return fmt.Sprintf("rack: %s  peer: %s  networks: %s  advertise-table: %d  uplink-pattern: %s  members: %s",
		rack, g.Peer, networks, g.AdvertiseTable, g.UplinkPattern, members)
}

// attachTableRows renders the per-member plan as table rows.
func attachTableRows(nodes []types.BgpAttachNodePlan) [][]string {
	dash := func(v string) string {
		if v == "" {
			return "-"
		}
		return v
	}
	rows := make([][]string, 0, len(nodes))
	for _, n := range nodes {
		id := "-"
		if n.MemberID > 0 {
			id = strconv.FormatInt(n.MemberID, 10)
		}
		rows = append(rows, []string{dash(n.Rack), dash(n.Peer), n.Member, id, dash(n.Address), strconv.FormatBool(n.Attached), dash(n.RouterPort)})
	}
	return rows
}

// printAttachResponse prints the settings, a per-member table, warnings and
// errors. It returns an error (non-zero exit) only when the request failed:
// refused, or the router-port write failed. Per-member warnings (a member not
// reached, its router port not created yet) keep exit 0.
func printAttachResponse(resp types.SetupBgpAttachResponse) error {
	for _, r := range resp.Groups {
		fmt.Printf("%s\n", attachGroupLine(r))
	}
	if len(resp.Groups) > 0 {
		fmt.Printf("\n")
	}

	if len(resp.Nodes) > 0 {
		header := []string{"RACK", "PEER", "MEMBER", "ID", "ADDRESS", "ATTACHED", "ROUTER PORT"}
		if err := lxdCmd.RenderTable(lxdCmd.TableFormatTable, header, attachTableRows(resp.Nodes), resp.Nodes); err != nil {
			return err
		}
	}

	if len(resp.Added) > 0 {
		fmt.Printf("added: %s\n", strings.Join(resp.Added, ", "))
	}
	if len(resp.Removed) > 0 {
		fmt.Printf("removed: %s\n", strings.Join(resp.Removed, ", "))
	}
	for _, w := range resp.Warnings {
		fmt.Printf("Warning: %s\n", w)
	}
	for _, e := range resp.Errors {
		fmt.Printf("Error: %s\n", e)
	}
	fmt.Printf("%s\n", resp.Message)
	if !resp.Success {
		return errors.New(resp.Message)
	}
	return nil
}
