package bgp

// Turning a bgp-attach group into the attach markers. The work is split by
// where each key lives:
//
//   - The four router-port keys (networks, maintain-vrf, attach marker, peers)
//     live in the OVN Northbound database, which every member can reach. The
//     member handling the `setup bgp-attach` call writes them for every
//     affected compute in one ovn-nbctl call (WriteGroupPorts), after
//     recording each port's previous values in the group's record, and puts
//     those values back when a compute leaves (RestorePorts).
//   - The two chassis knobs (enable gate, advertise table) live in each
//     compute's own Open_vSwitch database, which only that compute can reach.
//     Each compute sets or clears its own on every reconcile tick
//     (reconcileAttachIntent), then the existing marker-driven pipeline
//     attaches exactly as if an operator had stamped the keys by hand.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/canonical/lxd/shared/logger"
	microTypes "github.com/canonical/microcluster/v3/microcluster/types"

	ovnCmd "github.com/canonical/microovn/microovn/ovn/cmd"
)

// attachIntentPriorKey is the local Open_vSwitch external-id holding the
// chassis knobs' values from before the first group was applied here.
const attachIntentPriorKey = "microovn-bgp-intent-prior"

// lrpMaintainVrfOption is the LRP option that makes ovn-controller create the
// ovnvrf<N> device. Attach refuses to run without it.
const lrpMaintainVrfOption = "dynamic-routing-maintain-vrf"

// lrpPrior holds the four router-port fields bgp-attach manages: either a
// port's values before the first write, or the values wanted. Empty strings
// mean "not set".
type lrpPrior struct {
	Networks    []string `json:"networks"`
	MaintainVrf string   `json:"maintain_vrf"`
	Attach      string   `json:"attach"`
	Peers       string   `json:"peers"`
}

func (p lrpPrior) equal(o lrpPrior) bool {
	return slices.Equal(p.Networks, o.Networks) && p.MaintainVrf == o.MaintainVrf &&
		p.Attach == o.Attach && p.Peers == o.Peers
}

// knobPrior is the chassis knobs' values before the first group applied here.
type knobPrior struct {
	Gate      string `json:"gate"`
	Advertise string `json:"advertise"`
}

// encodeJournal / decodeJournal store a record as base64url JSON, so the OVS
// value never needs quoting or escaping.
func encodeJournal(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeJournal(s string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// peerAddr returns the group's peer address (validated at parse time).
func peerAddr(intent AttachIntent) netip.Addr {
	a, _ := netip.ParseAddr(intent.Peer.Address)
	return a
}

// networksArg renders an ovn-nbctl `networks=` argument with each prefix
// quoted: networks="192.0.2.3/28".
func networksArg(prefixes []string) string {
	quoted := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		quoted = append(quoted, fmt.Sprintf("%q", p))
	}
	return "networks=" + strings.Join(quoted, ",")
}

// readLrpFields reads the four fields bgp-attach manages on an LRP.
func readLrpFields(ctx context.Context, s microTypes.State, lrp string) (lrpPrior, error) {
	var f lrpPrior
	networksRaw, err := nbGetIfExists(ctx, s, "Logical_Router_Port", lrp, "networks")
	if err != nil {
		return f, err
	}
	networks, err := parseLrpNetworks(networksRaw)
	if err != nil {
		return f, err
	}
	for _, n := range networks {
		f.Networks = append(f.Networks, n.String())
	}
	if f.MaintainVrf, err = nbGetIfExists(ctx, s, "Logical_Router_Port", lrp, "options:"+lrpMaintainVrfOption); err != nil {
		return f, err
	}
	if f.Attach, err = nbGetIfExists(ctx, s, "Logical_Router_Port", lrp, "external_ids:"+BgpAttachMarker); err != nil {
		return f, err
	}
	if f.Peers, err = nbGetIfExists(ctx, s, "Logical_Router_Port", lrp, "external_ids:"+BgpPeersKey); err != nil {
		return f, err
	}
	return f, nil
}

// ReadPortPrior reads an uplink's current values, to record them before the
// first bgp-attach write.
func ReadPortPrior(ctx context.Context, s microTypes.State, lrp string) (PortRecord, error) {
	prior, err := readLrpFields(ctx, s, lrp)
	if err != nil {
		return PortRecord{}, fmt.Errorf("failed to read router port %s: %w", lrp, err)
	}
	return PortRecord{RouterPort: lrp, Prior: prior}, nil
}

// desiredLrpFields is what a group wants on one member's uplink.
func desiredLrpFields(intent AttachIntent, address netip.Prefix) lrpPrior {
	f := lrpPrior{MaintainVrf: "true", Attach: "true", Peers: intent.PeerString()}
	if address.IsValid() {
		f.Networks = []string{address.String()}
	}
	return f
}

// lrpSetArgs is the ovn-nbctl `set` command that gives an LRP the wanted
// values. Unnumbered groups leave networks alone. Setting a value it already
// has changes nothing, so the command is safe to repeat.
func lrpSetArgs(lrp string, want lrpPrior, numbered bool) []string {
	args := []string{"set", "Logical_Router_Port", lrp}
	if numbered {
		args = append(args, networksArg(want.Networks))
	}
	return append(args,
		fmt.Sprintf("options:%s=%s", lrpMaintainVrfOption, want.MaintainVrf),
		fmt.Sprintf("external_ids:%s=%s", BgpAttachMarker, want.Attach),
		fmt.Sprintf("external_ids:%s=%q", BgpPeersKey, want.Peers))
}

// lrpRestoreArgs are the ovn-nbctl commands that put an LRP's recorded values
// back (each tolerating a port that no longer exists).
func lrpRestoreArgs(lrp string, prior lrpPrior) [][]string {
	var cmds [][]string
	if len(prior.Networks) == 0 {
		cmds = append(cmds, []string{"--if-exists", "clear", "Logical_Router_Port", lrp, "networks"})
	} else {
		cmds = append(cmds, []string{"--if-exists", "set", "Logical_Router_Port", lrp, networksArg(prior.Networks)})
	}
	restore := func(column, key, value string) []string {
		if value == "" {
			return []string{"--if-exists", "remove", "Logical_Router_Port", lrp, column, key}
		}
		return []string{"--if-exists", "set", "Logical_Router_Port", lrp, fmt.Sprintf("%s:%s=%q", column, key, value)}
	}
	return append(cmds,
		restore("options", lrpMaintainVrfOption, prior.MaintainVrf),
		restore("external_ids", BgpAttachMarker, prior.Attach),
		restore("external_ids", BgpPeersKey, prior.Peers))
}

// joinNbCommands joins several ovn-nbctl commands into one invocation, so
// they run as one OVSDB transaction: "cmd1 -- cmd2 -- cmd3".
func joinNbCommands(cmds [][]string) []string {
	var args []string
	for i, c := range cmds {
		if i > 0 {
			args = append(args, "--")
		}
		args = append(args, c...)
	}
	return args
}

// groupPortCommands builds the commands that write a group's keys on the
// given members' uplinks (members without a recorded uplink are skipped).
func groupPortCommands(group AttachIntent, members []string) [][]string {
	var cmds [][]string
	for _, m := range members {
		rec, ok := group.Ports[m]
		if !ok || rec.RouterPort == "" {
			continue
		}
		cmds = append(cmds, lrpSetArgs(rec.RouterPort, desiredLrpFields(group, group.Addresses[m]), group.Numbered()))
	}
	return cmds
}

// WriteGroupPorts writes the group's router-port keys for the given members in
// one ovn-nbctl call (one NB transaction). It is run by the member handling
// the `setup bgp-attach` call, after the group (with each port's previous
// values) has been stored.
func WriteGroupPorts(ctx context.Context, s microTypes.State, group AttachIntent, members []string) error {
	cmds := groupPortCommands(group, members)
	if len(cmds) == 0 {
		return nil
	}
	if _, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, joinNbCommands(cmds)...); err != nil {
		return fmt.Errorf("failed to write attach keys on %d router port(s): %w", len(cmds), err)
	}
	logger.Infof("bgp-attach: wrote attach keys on %d router port(s) for %s", len(cmds), group.Key())
	return nil
}

// RestorePorts puts the recorded pre-attach values back on the given router
// ports in one ovn-nbctl call.
func RestorePorts(ctx context.Context, s microTypes.State, records []PortRecord) error {
	var cmds [][]string
	for _, rec := range records {
		if rec.RouterPort != "" {
			cmds = append(cmds, lrpRestoreArgs(rec.RouterPort, rec.Prior)...)
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	if _, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, joinNbCommands(cmds)...); err != nil {
		return fmt.Errorf("failed to restore %d router port(s): %w", len(records), err)
	}
	logger.Infof("bgp-attach: restored %d router port(s)", len(records))
	return nil
}

// applyKnobs sets this chassis's gate and advertise table, recording their
// prior values on first use.
func applyKnobs(ctx context.Context, s microTypes.State, intent AttachIntent) error {
	gate, err := VsctlGetIfExists(ctx, s, "Open_vSwitch", ".", "external-ids", BgpEnableGate)
	if err != nil {
		return err
	}
	adv, err := VsctlGetIfExists(ctx, s, "Open_vSwitch", ".", "external-ids", attachAdvertiseKey)
	if err != nil {
		return err
	}
	wantAdv := fmt.Sprintf("%d", intent.AdvertiseTable)
	if gate == "enabled" && adv == wantAdv {
		return nil
	}

	prior, err := VsctlGetIfExists(ctx, s, "Open_vSwitch", ".", "external-ids", attachIntentPriorKey)
	if err != nil {
		return err
	}
	args := []string{"set", "Open_vSwitch", "."}
	if prior == "" {
		rec, err := encodeJournal(knobPrior{Gate: gate, Advertise: adv})
		if err != nil {
			return err
		}
		args = append(args, fmt.Sprintf("external_ids:%s=%q", attachIntentPriorKey, rec))
	}
	// Advertise table before the gate, so the gate never turns on with a stale table.
	args = append(args,
		fmt.Sprintf("external_ids:%s=%s", attachAdvertiseKey, wantAdv),
		fmt.Sprintf("external_ids:%s=enabled", BgpEnableGate))
	_, err = ovnCmd.VSCtl(ctx, s, args...)
	return err
}

// restoreKnobs puts this chassis's knobs back to their prior values. With no
// record it reads one local OVS key and returns, so the idle tick stays cheap.
func restoreKnobs(ctx context.Context, s microTypes.State) error {
	raw, err := VsctlGetIfExists(ctx, s, "Open_vSwitch", ".", "external-ids", attachIntentPriorKey)
	if err != nil || raw == "" {
		return err
	}
	var prior knobPrior
	if err := decodeJournal(raw, &prior); err != nil {
		return fmt.Errorf("corrupt journal %s: %w", attachIntentPriorKey, err)
	}
	restore := func(key, value string) error {
		var err error
		if value == "" {
			_, err = ovnCmd.VSCtl(ctx, s, "remove", "Open_vSwitch", ".", "external_ids", key)
		} else {
			_, err = ovnCmd.VSCtl(ctx, s, "set", "Open_vSwitch", ".", fmt.Sprintf("external_ids:%s=%q", key, value))
		}
		return err
	}
	if err := restore(BgpEnableGate, prior.Gate); err != nil {
		return err
	}
	if err := restore(attachAdvertiseKey, prior.Advertise); err != nil {
		return err
	}
	_, err = ovnCmd.VSCtl(ctx, s, "remove", "Open_vSwitch", ".", "external_ids", attachIntentPriorKey)
	return err
}

// reconcileAttachIntent converges this chassis's own knobs with the group
// that lists it: on when listed, back to their prior values when not. The
// router-port keys are not written here; the member handling the
// `setup bgp-attach` call writes them centrally. It runs at the top of every
// attach pass, before marker discovery.
func reconcileAttachIntent(ctx context.Context, s microTypes.State) error {
	groups, err := LoadAttachGroups(ctx, s)
	if err != nil {
		return err
	}
	intent := GroupForMember(groups, s.Name())
	if intent == nil {
		return restoreKnobs(ctx, s)
	}

	managed, err := HasManagedBgp(ctx, s)
	if err != nil {
		return fmt.Errorf("failed to check for managed BGP: %w", err)
	}
	if managed {
		logger.Warnf("bgp-attach group %s lists %s but managed BGP is active there; not enabling attach", intent.Key(), s.Name())
		return nil
	}

	// The router-port keys are written centrally; point out when ours are
	// missing (e.g. removed by hand) so the operator knows to rerun the call.
	if rec, ok := intent.Ports[s.Name()]; ok && rec.RouterPort != "" {
		if marker, err := nbGetIfExists(ctx, s, "Logical_Router_Port", rec.RouterPort, "external_ids:"+BgpAttachMarker); err == nil && marker != "true" {
			logger.Warnf("bgp-attach: router port %s lacks %s; rerun 'microovn setup bgp-attach' for %s to rewrite it",
				rec.RouterPort, BgpAttachMarker, intent.Key())
		}
	}

	return applyKnobs(ctx, s, *intent)
}
