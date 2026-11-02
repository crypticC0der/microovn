package bgp

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/canonical/lxd/shared"
	"github.com/canonical/lxd/shared/logger"
	microTypes "github.com/canonical/microcluster/v3/microcluster/types"

	"github.com/canonical/microovn/microovn/api/types"
	ovnCmd "github.com/canonical/microovn/microovn/ovn/cmd"
	"github.com/canonical/microovn/microovn/ovn/paths"
	"github.com/canonical/microovn/microovn/snap"
)

// birdConfigMu serialises applyBirdConfig, which the reconciler and `microovn
// setup bird` can both call concurrently (they share the candidate file and the
// running config file).
var birdConfigMu sync.Mutex

// Attach mode markers. Attach lets MicroOVN attach BIRD to an externally owned
// logical router port via native OVN routing-protocol-redirect, instead of
// owning the whole BGP topology (which is what the managed path does).
const (
	// BgpAttachedTag is the Open_vSwitch external-id key that records the
	// comma-separated list of logical router ports this chassis has attached
	// to. It lives in the local OVS DB (not the NB), so it survives a topology
	// owner rewriting NB external_ids (for cleanup), and it is inherently
	// chassis-scoped.
	BgpAttachedTag = "microovn-bgp-attached"
	// BgpAttachMarker triggers attach mode when set to "true" on a
	// Logical_Router_Port.
	BgpAttachMarker = "microovn-bgp-attach"
	// BgpEnableGate is the chassis-wide enable gate stored on Open_vSwitch.
	// Attach is only active when this is "enabled" and at least one LRP carries
	// BgpAttachMarker.
	BgpEnableGate = "microovn-bgp"
	// BgpPeersKey lists the eBGP peers on an attach Logical_Router_Port.
	BgpPeersKey = "microovn-bgp-peers"
)

// Open_vSwitch external-id keys for the chassis-wide attach knobs.
const (
	attachAsnKey       = "microovn-bgp-asn"
	attachRouterIDKey  = "microovn-bgp-router-id"
	attachAdvertiseKey = "microovn-bgp-advertise-table"
)

// Open_vSwitch external-id key prefixes used to remember the owner's pre-attach
// Logical_Router_Port option values, so detach can restore them instead of
// deleting an externally owned router's configuration. The full key is the
// prefix + the LRP name.
const (
	attachOrigRoutingProtocols = "microovn-bgp-orig-routing-protocols."
	attachOrigRA               = "microovn-bgp-orig-ipv6-ra."
)

// attachOrigEmpty is the sentinel stored in the journal when the owner had no
// pre-attach value for a key. OVS external_ids cannot hold an empty value
// ("argument does not end in '=' followed by a value"), so an empty original is
// recorded explicitly.
const attachOrigEmpty = "-"

// attachTarget is the resolved attachment point for a single externally owned
// LRP tagged with BgpAttachMarker.
type attachTarget struct {
	// Router is the logical router owning the LRP (derived).
	Router string
	// Chassis is the chassis the router is pinned to via options:chassis
	// (empty when the owner did not pin it).
	Chassis string
	// RouterPort is the tagged LRP (the marker location).
	RouterPort string
	// Switch is the logical switch connected to the LRP (derived, 2 hops).
	Switch string
	// MAC is the explicit LRP mac.
	MAC string
	// PortName is options:dynamic-routing-port-name on the LRP (owner set).
	PortName string
	// Peers are the eBGP peers from BgpPeersKey on the LRP.
	Peers []types.Peer
	// LocalIP is the local IPv4 transit prefix the LRP carries in its
	// `networks` (the redirect port shares IP+MAC with the LRP, so the LRP's
	// own address is the veth and BIRD `local` address; netip.Prefix preserves
	// the literal host bits, so this is both the address and the containment
	// mask). The zero value means the uplink is unnumbered.
	LocalIP netip.Prefix
}

// attachConfig is the resolved, chassis-wide desired attach state.
type attachConfig struct {
	RouterID       string
	LocalASN       uint32
	AdvertiseTable uint32
	LearnTable     uint32
	Targets        []attachTarget
}

// attachInterfaces returns deterministic Linux veth names keyed off the LRP
// name, independent of any long logical name. The host side carries the
// router-port MAC and sits in the OVN VRF; the ovs side is bound to br-int.
func attachInterfaces(routerPort string) (host string, ovs string) {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(routerPort)))[:8]
	return "mp" + hash + "b", "mp" + hash + "o"
}

// attachPortName identifies the redirect LSP for an attachment without
// colliding with the router port it attaches to.
func attachPortName(chassis string, routerPort string) string {
	_, ovs := attachInterfaces(routerPort)
	return "microovn-bgp-" + chassis + "-" + ovs
}

// ownedAttachPorts returns the logical-router-port names this chassis has
// recorded as attached in the local OVS ownership journal.
func ownedAttachPorts(ctx context.Context, s microTypes.State) ([]string, error) {
	raw, err := VsctlGetIfExists(ctx, s, "Open_vSwitch", ".", "external-ids", BgpAttachedTag)
	if err != nil {
		return nil, err
	}
	var ports []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			ports = append(ports, p)
		}
	}
	return ports, nil
}

// addOwnedPort records lrp in the local ownership journal, idempotently.
func addOwnedPort(ctx context.Context, s microTypes.State, lrp string) error {
	ports, err := ownedAttachPorts(ctx, s)
	if err != nil {
		return err
	}
	for _, p := range ports {
		if p == lrp {
			return nil
		}
	}
	ports = append(ports, lrp)
	return writeOwnedPorts(ctx, s, ports)
}

// removeOwnedPort drops lrp from the local ownership journal.
func removeOwnedPort(ctx context.Context, s microTypes.State, lrp string) error {
	ports, err := ownedAttachPorts(ctx, s)
	if err != nil {
		return err
	}
	out := ports[:0]
	for _, p := range ports {
		if p != lrp {
			out = append(out, p)
		}
	}
	return writeOwnedPorts(ctx, s, out)
}

func writeOwnedPorts(ctx context.Context, s microTypes.State, ports []string) error {
	if len(ports) == 0 {
		_, err := ovnCmd.VSCtl(ctx, s, "remove", "Open_vSwitch", ".", "external_ids", BgpAttachedTag)
		return err
	}
	_, err := ovnCmd.VSCtl(ctx, s, "set", "Open_vSwitch", ".", "external_ids:"+BgpAttachedTag+"="+strings.Join(ports, ","))
	return err
}

// alreadyAttached reports whether a target already carries our own redirect
// option (i.e. a previous attach whose local journal was lost). Such a target is
// re-journaled and repaired rather than rejected by preflight.
func alreadyAttached(ctx context.Context, s microTypes.State, target attachTarget) bool {
	redirect, err := nbGetIfExists(ctx, s, "Logical_Router_Port", target.RouterPort, "options:routing-protocol-redirect")
	if err != nil {
		return false
	}
	return strings.TrimSpace(redirect) == attachPortName(s.Name(), target.RouterPort)
}

// attachRedirectIsOurs reports whether lrp already carries our own redirect
// option, i.e. it was previously attached and rendered. Unlike alreadyAttached it
// returns the NB read error, so the caller can distinguish "not ours" from
// "can't tell" (a transient error must not be treated as "not ours").
func attachRedirectIsOurs(ctx context.Context, s microTypes.State, lrp string) (bool, error) {
	redirect, err := nbGetIfExists(ctx, s, "Logical_Router_Port", lrp, "options:routing-protocol-redirect")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(redirect) == attachPortName(s.Name(), lrp), nil
}

// nbGetIfExists returns a column value from ovn-nbctl, treating a missing key
// as an empty string (mirroring VsctlGetIfExists in redirect.go).
func nbGetIfExists(ctx context.Context, s microTypes.State, table, row, column string) (string, error) {
	result, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--if-exists", "get", table, row, column)
	if err != nil && !strings.Contains(fmt.Sprintf("%v", err), "no key") {
		return "", err
	}
	return strings.Trim(strings.TrimSpace(result), "\""), nil
}

// recordOriginalOptions stashes the owner's pre-attach routing-protocols and
// ipv6_ra_configs values for lrp in the local OVS journal, so detach can restore
// them rather than delete externally owned configuration. It is idempotent: it
// records a key only if the journal entry is absent, so re-running a half-done
// attach never overwrites the owner's original with our own values. It returns
// an error if the owner's routing-protocols could not be read OR journaled — the
// caller must not stamp routing-protocols then, because detach would have no
// record to restore and would delete the owner's real value. The reconciler
// retries on the next attach. RA read/write failures are logged and skipped
// per-key (they are not destructive on detach).
func recordOriginalOptions(ctx context.Context, s microTypes.State, lrp string) error {
	origRP, err := nbGetIfExists(ctx, s, "Logical_Router_Port", lrp, "options:routing-protocols")
	if err != nil {
		// Do not journal an empty sentinel here: restoreOriginalOptions treats the
		// sentinel as "owner had none" and would delete the owner's real
		// routing-protocols on detach. Fail the snapshot so the caller does not
		// stamp routing-protocols; the reconciler re-attempts on the next attach.
		return fmt.Errorf("failed to read pre-attach routing-protocols for %s: %w", lrp, err)
	}
	origRP = strings.TrimSpace(origRP)

	rpKey := attachOrigRoutingProtocols + lrp
	existing, err := VsctlGetIfExists(ctx, s, "Open_vSwitch", ".", "external-ids", rpKey)
	if err != nil {
		return fmt.Errorf("failed to read journal key %s for %s: %w", rpKey, lrp, err)
	}
	if existing == "" {
		rpVal := origRP
		if rpVal == "" {
			rpVal = attachOrigEmpty
		}
		if _, err := ovnCmd.VSCtl(ctx, s, "set", "Open_vSwitch", ".", "external_ids:"+rpKey+"="+rpVal); err != nil {
			return fmt.Errorf("failed to record pre-attach routing-protocols for %s: %w", lrp, err)
		}
	}

	raKey := attachOrigRA + lrp
	if existing, err := VsctlGetIfExists(ctx, s, "Open_vSwitch", ".", "external-ids", raKey); err == nil && existing == "" {
		var parts []string
		for _, k := range []string{"send_periodic", "address_mode", "max_interval", "min_interval"} {
			v, err := nbGetIfExists(ctx, s, "Logical_Router_Port", lrp, "ipv6_ra_configs:"+k)
			if err != nil {
				logger.Warnf("Failed to read pre-attach ipv6_ra_configs:%s for %s: %v", k, lrp, err)
				v = ""
			}
			if strings.TrimSpace(v) != "" {
				parts = append(parts, k+"="+strings.TrimSpace(v))
			}
		}
		raVal := strings.Join(parts, "|")
		if raVal == "" {
			raVal = attachOrigEmpty
		}
		if _, err := ovnCmd.VSCtl(ctx, s, "set", "Open_vSwitch", ".", "external_ids:"+raKey+"="+raVal); err != nil {
			logger.Warnf("Failed to record pre-attach ipv6_ra_configs for %s: %v", lrp, err)
		}
	}
	return nil
}

// restoreOriginalOptions puts back the owner's pre-attach routing-protocols and
// ipv6_ra_configs values (removing them if they were originally unset). While
// attached the LRP's routing-protocols is BGP,BFD; on detach the owner's full
// original (which may omit BFD, or include only BGP) is restored here. It does
// NOT clear the journal — detach clears it (=clearOriginalOptions) only once the
// whole detach (including the final owned-port removal) has succeeded. This
// keeps the record available for the reconciler's retry if any later detach step
// fails transiently: a re-run of this restore is idempotent, whereas an
// already-gone journal would take the "owner had none" branch and delete the
// value just restored. A journal read failure is returned as an error rather than
// treated as "owner had none" for the same reason. When the record is genuinely
// absent (a lost journal), this removes the whole routing-protocols option and is
// therefore best-effort.
func restoreOriginalOptions(ctx context.Context, s microTypes.State, lrp string) error {
	rpKey := attachOrigRoutingProtocols + lrp
	origRP, err := VsctlGetIfExists(ctx, s, "Open_vSwitch", ".", "external-ids", rpKey)
	if err != nil {
		// A failed read must not be treated as "owner had none": that would make
		// the empty branch below delete the owner's real routing-protocols. Fail
		// the restore so detach keeps the journal and the reconciler retries.
		return fmt.Errorf("failed to read journal %s for %s: %w", rpKey, lrp, err)
	}
	raKey := attachOrigRA + lrp
	origRA, err := VsctlGetIfExists(ctx, s, "Open_vSwitch", ".", "external-ids", raKey)
	if err != nil {
		return fmt.Errorf("failed to read journal %s for %s: %w", raKey, lrp, err)
	}

	// routing-protocols: restore the original value, or remove it if the owner
	// had none (recorded as the empty sentinel, or never recorded — best-effort).
	if origRP == "" || origRP == attachOrigEmpty {
		if _, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--if-exists", "remove", "Logical_Router_Port", lrp, "options", "routing-protocols"); err != nil {
			return err
		}
	} else if _, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--if-exists", "set", "Logical_Router_Port", lrp, "options:routing-protocols="+origRP); err != nil {
		return err
	}

	orig := map[string]string{}
	for _, kv := range strings.Split(origRA, "|") {
		kv = strings.TrimSpace(kv)
		if kv == "" || kv == attachOrigEmpty {
			continue
		}
		if i := strings.Index(kv, "="); i >= 0 {
			orig[kv[:i]] = kv[i+1:]
		}
	}
	for _, k := range []string{"send_periodic", "address_mode", "max_interval", "min_interval"} {
		if v, ok := orig[k]; ok {
			if _, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--if-exists", "set", "Logical_Router_Port", lrp, "ipv6_ra_configs:"+k+"="+v); err != nil {
				return err
			}
		} else if _, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--if-exists", "remove", "Logical_Router_Port", lrp, "ipv6_ra_configs", k); err != nil {
			return err
		}
	}
	return nil
}

// clearOriginalOptions removes the journaled pre-attach originals for lrp. It is
// the detach-completion step: detachPort calls it only after the whole detach
// (including removeOwnedPort) has succeeded, so a partially-failed detach keeps
// the journal and the reconciler's retry re-runs restoreOriginalOptions
// idempotently instead of deleting the owner's values.
func clearOriginalOptions(ctx context.Context, s microTypes.State, lrp string) error {
	rpKey := attachOrigRoutingProtocols + lrp
	raKey := attachOrigRA + lrp
	if _, err := ovnCmd.VSCtl(ctx, s, "remove", "Open_vSwitch", ".", "external_ids", rpKey, raKey); err != nil {
		return fmt.Errorf("failed to clear journal %s for %s: %w", rpKey, lrp, err)
	}
	return nil
}

// centralSupportsAttach reports whether the central (northd) side is new enough
// to drive attach mode. Attach is anchored on options:dynamic-routing-vrf-id,
// first released in OVN 26.03. It never hard-fails: an unknown/unparseable
// version is treated as unsupported and the caller skips attach with a warning.
//
// Only the central (northd) side is checked: that is the component that may run
// a different OVN in a split deployment (e.g. Sunbeam's separate ovn-central).
// The local chassis always runs the OVN version shipped by this snap, so it is
// guaranteed to be >= 26.03 whenever the snap is.
func centralSupportsAttach(ctx context.Context, s microTypes.State) (bool, string, error) {
	result, err := ovnCmd.SBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--if-exists", "get", "SB_Global", ".", "options:northd_internal_version")
	if err != nil && !strings.Contains(fmt.Sprintf("%v", err), "no key") {
		return false, "", err
	}
	raw := strings.Trim(strings.TrimSpace(result), "\"")
	major, minor, _, ok := parseNorthdVersion(raw)
	if !ok {
		logger.Warnf("Unable to determine central OVN version from northd_internal_version %q; skipping attach", raw)
		return false, raw, nil
	}
	return major > 26 || (major == 26 && minor >= 3), raw, nil
}

// parseNorthdVersion extracts the leading "<major>.<minor>[.<patch>]" from a
// northd_internal_version string like "26.03.0-...". The patch defaults to 0
// when the string has only two components. It returns ok=false if the version
// can't be parsed.
func parseNorthdVersion(raw string) (major int, minor int, patch int, ok bool) {
	raw = strings.TrimSpace(raw)
	lead, _, _ := strings.Cut(raw, "-")
	parts := strings.Split(lead, ".")
	if len(parts) < 2 {
		return 0, 0, 0, false
	}
	m, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0, false
	}
	p := 0
	if len(parts) >= 3 {
		p, err = strconv.Atoi(parts[2])
		if err != nil {
			return 0, 0, 0, false
		}
	}
	return m, n, p, true
}

// supportsSlash31LRP reports whether the central (northd) version handles a
// /31 router port. RFC 3021 /31 support landed on main as 1e738aa7a and was
// backported to branch-26.03 as b17ac57f7, first released in 26.03.3; 26.09.0
// and later also carry it. An older central misclassifies the /31 peer as the
// broadcast address and drops peer-sourced routed traffic, so a /31 uplink on
// such a central is warned about (the BGP/BFD session itself still forms).
func supportsSlash31LRP(raw string) bool {
	major, minor, patch, ok := parseNorthdVersion(raw)
	if !ok {
		return false
	}
	if major > 26 || (major == 26 && minor > 3) {
		return true
	}
	return major == 26 && minor == 3 && patch >= 3
}

// discoverAttachTargets scans the NB for LRPs tagged with BgpAttachMarker and
// resolves them into attach targets. It returns:
//   - targets: resolved local targets (to attach),
//   - tagged: every tagged LRP name (marker present),
//   - foreign: tagged LRPs whose router is pinned to a different chassis,
//   - err: non-nil only if the initial scan failed.
//
// A per-target resolution error is logged and skipped (not fatal), and the LRP
// stays in "tagged", so the reconciler can tell a transient read error apart
// from an affirmative marker removal (gone) or an affirmative chassis move
// (foreign).
func discoverAttachTargets(ctx context.Context, s microTypes.State) (targets []attachTarget, tagged []string, foreign []string, err error) {
	out, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--bare", "--columns=name",
		"find", "Logical_Router_Port", fmt.Sprintf("external_ids:%s=true", BgpAttachMarker))
	if err != nil {
		return nil, nil, nil, err
	}
	tagged = parseOvnFind(out)

	for _, lrp := range tagged {
		router, chassis, err := resolveTargetRouter(ctx, s, lrp)
		if err != nil {
			logger.Warnf("Skipping attach target %q: %v", lrp, err)
			continue
		}
		if chassis != s.Name() {
			logger.Infof("Skipping attach target %q on router %q (pinned to chassis %q)", lrp, router, chassis)
			foreign = append(foreign, lrp)
			continue
		}

		target, err := resolveAttachTarget(ctx, s, lrp, router, chassis)
		if err != nil {
			logger.Warnf("Skipping attach target %q: %v", lrp, err)
			continue
		}
		targets = append(targets, target)
	}
	return targets, tagged, foreign, nil
}

// resolveTargetRouter derives the router owning an LRP and the chassis that
// router is pinned to, without doing the (more expensive) switch lookup.
func resolveTargetRouter(ctx context.Context, s microTypes.State, lrp string) (router string, chassis string, err error) {
	lrpUUIDOut, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--bare", "--columns=_uuid", "find", "Logical_Router_Port", "name="+lrp)
	if err != nil {
		return "", "", err
	}
	lrpUUIDs := parseOvnFind(lrpUUIDOut)
	if len(lrpUUIDs) != 1 {
		return "", "", fmt.Errorf("expected exactly one logical router port %s, found %d", lrp, len(lrpUUIDs))
	}

	routerOut, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--bare", "--columns=name",
		"find", "Logical_Router", fmt.Sprintf("ports{>=}%s", lrpUUIDs[0]))
	if err != nil {
		return "", "", err
	}
	routers := parseOvnFind(routerOut)
	if len(routers) != 1 {
		return "", "", fmt.Errorf("expected exactly one logical router for port %s, found %d", lrp, len(routers))
	}
	router = routers[0]

	chassis, err = nbGetIfExists(ctx, s, "Logical_Router", router, "options:chassis")
	if err != nil {
		return "", "", err
	}
	return router, strings.TrimSpace(chassis), nil
}

// resolveAttachTarget derives the switch for a tagged LRP (whose router and
// chassis are already known) and reads its identity and peers. The switch
// lookup uses the unquoted "ports{>=}<uuid>" set-membership idiom (OVSDB: set
// is a superset of {uuid}, i.e. it contains the port) - note {>=} with a single
// bound, not {<=}, which would mean "subset" and fail once the switch has more
// than one port.
func resolveAttachTarget(ctx context.Context, s microTypes.State, lrp string, router string, chassis string) (attachTarget, error) {
	target := attachTarget{RouterPort: lrp, Router: router, Chassis: chassis}

	// Switch: 2 hops. (1) the linking LSP of type router pointing back at the
	// LRP; (2) the switch whose ports set contains that LSP.
	lspOut, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--bare", "--columns=name,_uuid",
		"find", "Logical_Switch_Port", "type=router", "options:router-port="+lrp)
	if err != nil {
		return target, err
	}
	lspFields := strings.Fields(strings.TrimSpace(lspOut))
	if len(lspFields) != 2 {
		return target, fmt.Errorf("expected exactly one linking LSP for port %s, found %d", lrp, len(lspFields))
	}
	lspName, lspUUID := lspFields[0], lspFields[1]

	swOut, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--bare", "--columns=name",
		"find", "Logical_Switch", fmt.Sprintf("ports{>=}%s", lspUUID))
	if err != nil {
		return target, err
	}
	switches := parseOvnFind(swOut)
	if len(switches) != 1 {
		return target, fmt.Errorf("expected exactly one logical switch for LSP %s, found %d", lspName, len(switches))
	}
	target.Switch = switches[0]

	// MAC (explicit, not auto).
	mac, err := nbGetIfExists(ctx, s, "Logical_Router_Port", lrp, "mac")
	if err != nil {
		return target, err
	}
	target.MAC = strings.ToLower(strings.TrimSpace(mac))

	// dynamic-routing-port-name (owner set).
	portName, err := nbGetIfExists(ctx, s, "Logical_Router_Port", lrp, "options:dynamic-routing-port-name")
	if err != nil {
		return target, err
	}
	target.PortName = strings.TrimSpace(portName)

	// Peers.
	peersRaw, err := nbGetIfExists(ctx, s, "Logical_Router_Port", lrp, "external_ids:"+BgpPeersKey)
	if err != nil {
		return target, err
	}
	if peersRaw != "" {
		peers, err := types.ParsePeers(peersRaw)
		if err != nil {
			return target, err
		}
		target.Peers = peers
	}

	// Derive the numbered local prefix from the LRP's `networks`: the redirect port
	// shares IP and MAC with the LRP, so the local address is one of the LRP's
	// own network addresses (not an independent key). Numbered is detected
	// purely from the peer family (an IPv4 peer), and the transit prefix is the
	// LRP network containing those IPv4 peers. No IPv4 peer => unnumbered and
	// `networks` is not read at all (so a malformed networks value can't block
	// an unnumbered attach that never uses it).
	if hasIPv4Peer(target.Peers) {
		networksRaw, err := nbGetIfExists(ctx, s, "Logical_Router_Port", lrp, "networks")
		if err != nil {
			return target, err
		}
		networks, err := parseLrpNetworks(networksRaw)
		if err != nil {
			return target, fmt.Errorf("router port %s has invalid networks %q: %w", lrp, networksRaw, err)
		}
		if prefix, numbered, err := classifyNumbered(networks, target.Peers); err != nil {
			return target, fmt.Errorf("router port %s: %w", lrp, err)
		} else if numbered {
			target.LocalIP = prefix
		}
	}

	return target, nil
}

// hasIPv4Peer reports whether any peer address is IPv4 (which makes the uplink
// numbered).
func hasIPv4Peer(peers []types.Peer) bool {
	for _, p := range peers {
		if a, err := netip.ParseAddr(p.Address); err == nil && a.Is4() {
			return true
		}
	}
	return false
}

// parseLrpNetworks parses the `ovn-nbctl get ... networks` output (a bracketed
// list of "ip/prefix" strings, e.g. ["10.0.0.1/31", "fdaa::1/64"]). Empty or
// absent returns an empty slice.
func parseLrpNetworks(raw string) ([]netip.Prefix, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil, nil
	}
	body := raw
	if strings.HasPrefix(body, "[") && strings.HasSuffix(body, "]") {
		body = body[1 : len(body)-1]
	}
	var out []netip.Prefix
	for _, entry := range strings.Split(body, ",") {
		entry = strings.Trim(strings.TrimSpace(entry), "\"")
		if entry == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid network prefix %q: %w", entry, err)
		}
		out = append(out, prefix)
	}
	return out, nil
}

// classifyNumbered decides whether an uplink is numbered and derives its local
// transit prefix. An uplink is numbered exactly when at least one peer is IPv4;
// the transit prefix is the (unique) LRP network containing every IPv4 peer. If
// no peer is IPv4 the uplink is unnumbered and the zero prefix is returned.
func classifyNumbered(networks []netip.Prefix, peers []types.Peer) (netip.Prefix, bool, error) {
	var v4 []netip.Addr
	for _, p := range peers {
		a, err := netip.ParseAddr(p.Address)
		if err != nil {
			return netip.Prefix{}, false, fmt.Errorf("peer %q is not a valid address", p.Address)
		}
		if a.Is4() {
			v4 = append(v4, a)
		}
	}
	if len(v4) == 0 {
		return netip.Prefix{}, false, nil
	}
	var matches []netip.Prefix
	for _, n := range networks {
		all := true
		for _, a := range v4 {
			if !n.Contains(a) {
				all = false
				break
			}
		}
		if all {
			matches = append(matches, n)
		}
	}
	switch len(matches) {
	case 0:
		return netip.Prefix{}, false, fmt.Errorf("numbered uplink has IPv4 peers but none are within an LRP network (peers must be within the LRP's `networks`)")
	case 1:
		return matches[0], true, nil
	default:
		return netip.Prefix{}, false, fmt.Errorf("numbered uplink IPv4 peers span multiple LRP networks (%d); put them in a single transit network", len(matches))
	}
}

// readAttachKnobs reads the chassis-wide knobs from Open_vSwitch external_ids.
func readAttachKnobs(ctx context.Context, s microTypes.State) (routerID string, asn uint32, advertise uint32, err error) {
	idStr, err := VsctlGetIfExists(ctx, s, "Open_vSwitch", ".", "external-ids", attachRouterIDKey)
	if err != nil {
		return "", 0, 0, err
	}
	routerID = idStr

	asnStr, err := VsctlGetIfExists(ctx, s, "Open_vSwitch", ".", "external-ids", attachAsnKey)
	if err != nil {
		return routerID, 0, 0, err
	}
	if asnStr != "" {
		n, err := strconv.ParseUint(asnStr, 10, 32)
		if err != nil {
			return routerID, 0, 0, fmt.Errorf("invalid %s: %s", attachAsnKey, asnStr)
		}
		asn = uint32(n)
	}

	advStr, err := VsctlGetIfExists(ctx, s, "Open_vSwitch", ".", "external-ids", attachAdvertiseKey)
	if err != nil {
		return routerID, asn, 0, err
	}
	if advStr != "" {
		n, err := strconv.ParseUint(advStr, 10, 32)
		if err != nil {
			return routerID, asn, 0, fmt.Errorf("invalid %s: %s", attachAdvertiseKey, advStr)
		}
		advertise = uint32(n)
	}

	return routerID, asn, advertise, nil
}

// resolveLearnTable derives the VRF table id for a router. The id is normally
// recorded in options:dynamic-routing-vrf-id (set by OVN when it created the
// VRF). When it is absent but dynamic-routing is enabled, the effective id is
// the router's Datapath_Binding.tunnel_key (read-only; never assign so as not
// to churn the VRF). If neither is available, the owner has not set up dynamic
// routing and attach cannot proceed.
func resolveLearnTable(ctx context.Context, s microTypes.State, router string) (uint32, error) {
	raw, err := nbGetIfExists(ctx, s, "Logical_Router", router, "options:dynamic-routing-vrf-id")
	if err != nil {
		return 0, err
	}
	raw = strings.TrimSpace(raw)
	if raw != "" {
		n, err := strconv.ParseUint(raw, 10, 32)
		if err != nil || n == 0 {
			return 0, fmt.Errorf("router %s has invalid dynamic-routing-vrf-id %q", router, raw)
		}
		return uint32(n), nil
	}

	dr, err := nbGetIfExists(ctx, s, "Logical_Router", router, "options:dynamic-routing")
	if err != nil {
		return 0, err
	}
	if dr == "true" {
		key, err := datapathTunnelKey(ctx, s, router)
		if err != nil {
			return 0, err
		}
		if key == 0 {
			return 0, fmt.Errorf("router %s has no Datapath_Binding tunnel_key", router)
		}
		return key, nil
	}

	return 0, fmt.Errorf("owner must set options:dynamic-routing-vrf-id (or options:dynamic-routing=true) on router %s", router)
}

// datapathTunnelKey looks up the Datapath_Binding.tunnel_key for a logical
// datapath name (the router) from the Southbound DB. Datapath_Binding's
// external_ids:name holds the logical datapath name (verified against a live
// SB DB); tunnel_key is the id OVN uses as the VRF table id for that router.
func datapathTunnelKey(ctx context.Context, s microTypes.State, datapath string) (uint32, error) {
	out, err := ovnCmd.SBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--bare", "--columns=tunnel_key",
		"find", "Datapath_Binding", "external_ids:name="+datapath)
	if err != nil {
		return 0, err
	}
	keys := parseOvnFind(out)
	if len(keys) != 1 {
		return 0, fmt.Errorf("expected exactly one Datapath_Binding for %s, found %d", datapath, len(keys))
	}
	n, err := strconv.ParseUint(strings.TrimSpace(keys[0]), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid Datapath_Binding tunnel_key %q for %s: %w", keys[0], datapath, err)
	}
	return uint32(n), nil
}

// validateAttachAddressing validates the resolved peer/addressing state of a
// single attach uplink. It is pure (no NB/OVS/state access), so it runs for
// both new and already-owned targets every reconcile tick — an owner editing or
// adding addressing on a live attachment is caught here rather than bypassing
// the new-target-only preflight.
func validateAttachAddressing(peers []types.Peer, localIP netip.Prefix, localASN uint32) error {
	if len(peers) == 0 {
		return fmt.Errorf("no %s peers", BgpPeersKey)
	}
	for _, p := range peers {
		if p.ASN == 0 || p.ASN == localASN {
			return fmt.Errorf("invalid peer ASN %d", p.ASN)
		}
	}

	numbered := localIP.IsValid()
	if numbered && localIP.Bits() >= 32 {
		return fmt.Errorf("%s is a host route (/32), not a real transit prefix", localIP)
	}
	if numbered && isIPv4NetOrBroadcast(localIP.Addr(), localIP) {
		return fmt.Errorf("local transit address %s is the network/broadcast address of %s", localIP.Addr(), localIP)
	}

	for _, p := range peers {
		peer, err := netip.ParseAddr(p.Address)
		if err != nil {
			return fmt.Errorf("peer %q is not a valid address", p.Address)
		}
		if numbered {
			if !peer.Is4() {
				return fmt.Errorf("numbered uplink requires IPv4 peers, got %q (IPv6 link-local is for unnumbered peering)", p.Address)
			}
			if !localIP.Contains(peer) {
				return fmt.Errorf("peer %s is outside transit prefix %s", peer, localIP)
			}
			if peer == localIP.Addr() {
				return fmt.Errorf("peer %s collides with the local transit address %s", peer, localIP.Addr())
			}
			if isIPv4NetOrBroadcast(peer, localIP) {
				return fmt.Errorf("peer %s is the network/broadcast address of %s", peer, localIP)
			}
		} else {
			if !peer.Is6() || !peer.IsLinkLocalUnicast() {
				return fmt.Errorf("unnumbered uplink requires IPv6 link-local peers, got %q (use an IPv4 peer within the LRP network for numbered peering)", p.Address)
			}
		}
	}
	return nil
}

// isIPv4NetOrBroadcast reports whether addr is the network or directed
// broadcast address of an IPv4 prefix. Only meaningful for prefixes shorter
// than /31 (RFC 3021 /31 has no network/broadcast distinction).
func isIPv4NetOrBroadcast(addr netip.Addr, prefix netip.Prefix) bool {
	if !prefix.IsValid() || !addr.Is4() || prefix.Bits() >= 31 {
		return false
	}
	network := prefix.Masked().Addr().As4()
	asU32 := uint32(network[0])<<24 | uint32(network[1])<<16 | uint32(network[2])<<8 | uint32(network[3])
	hostBits := uint32(32 - prefix.Bits())
	broadcast := asU32 | ((1 << hostBits) - 1)

	a := addr.As4()
	addrU32 := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	return addrU32 == asU32 || addrU32 == broadcast
}

// preflightAttach validates a resolved attach target before any mutation.
func preflightAttach(ctx context.Context, s microTypes.State, cfg attachConfig, target attachTarget) error {
	// Router must be pinned to the local chassis.
	chassis, err := nbGetIfExists(ctx, s, "Logical_Router", target.Router, "options:chassis")
	if err != nil {
		return err
	}
	if chassis != s.Name() {
		return fmt.Errorf("router %s is not pinned to local chassis %q", target.Router, s.Name())
	}

	// dynamic-routing is owner-set and required: attach relies on OVN having
	// created the VRF and redirect plumbing.
	dr, err := nbGetIfExists(ctx, s, "Logical_Router", target.Router, "options:dynamic-routing")
	if err != nil {
		return err
	}
	if dr != "true" {
		return fmt.Errorf("owner must set options:dynamic-routing=true on router %s", target.Router)
	}

	// dynamic-routing-maintain-vrf is owner-set and required: it is what makes
	// ovn-controller create ovnvrf<N> on this chassis.
	mv, err := nbGetIfExists(ctx, s, "Logical_Router_Port", target.RouterPort, "options:dynamic-routing-maintain-vrf")
	if err != nil {
		return err
	}
	if mv != "true" {
		return fmt.Errorf("owner must set options:dynamic-routing-maintain-vrf=true on router port %s", target.RouterPort)
	}

	// v1 collapses to a single VRF per chassis: every target's router must
	// resolve to the same learn table as the one already selected, otherwise a
	// target would silently fly on the wrong VRF.
	tLearn, err := resolveLearnTable(ctx, s, target.Router)
	if err != nil {
		return err
	}
	if tLearn != cfg.LearnTable {
		return fmt.Errorf("router %s VRF table %d differs from the chassis VRF %d", target.Router, tLearn, cfg.LearnTable)
	}

	// A custom VRF name is not supported: attach relies on the default
	// ovnvrf<N> naming (getVrfName). Reject an explicit override rather than
	// silently using the wrong device name.
	if vrfName, err := nbGetIfExists(ctx, s, "Logical_Router", target.Router, "options:dynamic-routing-vrf-name"); err != nil {
		return err
	} else if v := strings.TrimSpace(vrfName); v != "" && v != getVrfName(strconv.FormatUint(uint64(cfg.LearnTable), 10)) {
		return fmt.Errorf("router %s sets dynamic-routing-vrf-name=%q; attach requires the default ovnvrf<N> naming", target.Router, v)
	}

	// The LRP must not already carry a redirect: a genuinely-new target with a
	// pre-existing routing-protocol-redirect is a conflict. (A target that already
	// points at our own LSP is handled by the reconciler, not preflight.)
	redirect, err := nbGetIfExists(ctx, s, "Logical_Router_Port", target.RouterPort, "options:routing-protocol-redirect")
	if err != nil {
		return err
	}
	if r := strings.TrimSpace(redirect); r != "" {
		return fmt.Errorf("router port %s already has routing-protocol-redirect %q", target.RouterPort, r)
	}

	// MAC must be explicit and valid.
	if parsed, err := net.ParseMAC(target.MAC); err != nil || len(parsed) != 6 {
		return fmt.Errorf("router port %s must have an explicit, valid MAC (got %q)", target.RouterPort, target.MAC)
	}

	// dynamic-routing-port-name must be set and non-empty.
	if target.PortName == "" {
		return fmt.Errorf("router port %s is missing options:dynamic-routing-port-name", target.RouterPort)
	}

	// dynamic-routing-port-name must be unique across THIS chassis: it is the
	// key in Open_vSwitch's (chassis-local) dynamic-routing-port-mapping, so two
	// ports pinned to the same chassis sharing it would clobber each other's
	// mapping entry. The same name on a different chassis is fine.
	portNamePorts, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--bare", "--columns=name",
		"find", "Logical_Router_Port", "options:dynamic-routing-port-name="+target.PortName)
	if err != nil {
		return err
	}
	for _, name := range parseOvnFind(portNamePorts) {
		if name == target.RouterPort {
			continue
		}
		_, otherChassis, err := resolveTargetRouter(ctx, s, name)
		if err != nil {
			return fmt.Errorf("router port %s shares dynamic-routing-port-name %q with unresolvable port %s: %w", target.RouterPort, target.PortName, name, err)
		}
		if otherChassis == s.Name() {
			return fmt.Errorf("router port %s has a duplicate dynamic-routing-port-name %q on chassis %q", target.RouterPort, target.PortName, s.Name())
		}
	}

	// The attachment veth pair and redirect LSP must not already exist.
	host, ovs := attachInterfaces(target.RouterPort)
	for _, iface := range []string{host, ovs} {
		if _, err := os.Stat("/sys/class/net/" + iface); err == nil {
			return fmt.Errorf("interface %s already exists", iface)
		}
	}
	lsp := attachPortName(s.Name(), target.RouterPort)
	existing, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--bare", "--columns=name", "find", "Logical_Switch_Port", "name="+lsp)
	if err != nil {
		return err
	}
	if strings.TrimSpace(existing) != "" {
		return fmt.Errorf("attachment LSP %s already exists", lsp)
	}

	return nil
}

// attachConverged reports whether an attachment is fully in place: the host
// veth exists and is enslaved to the OVN VRF, the OVS side carries the right
// iface-id, the dynamic-routing-port-mapping entry is present, the LRP
// carries the routing-protocol-redirect to our LSP, and the LRP's
// routing-protocols still names both BGP and BFD. Checking routing-protocols
// means a pre-BFD attachment (or an owner that rewrote the option mid-attach to
// BGP-only) fails convergence and gets re-stamped by attachUplink, so BFD peers
// are not silently left with BFD redirected nowhere. It gates the idempotent
// attachUplink so a drifted or partially-built attachment is repaired.
func attachConverged(ctx context.Context, s microTypes.State, cfg attachConfig, target attachTarget) bool {
	host, ovs := attachInterfaces(target.RouterPort)
	lsp := attachPortName(s.Name(), target.RouterPort)
	vrf := getVrfName(strconv.FormatUint(uint64(cfg.LearnTable), 10))

	// Host veth must exist, carry the LRP's MAC and be enslaved to the OVN VRF.
	linkOut, err := shared.RunCommand(ctx, "ip", "-d", "link", "show", "dev", host)
	if err != nil || !linkHasMaster(linkOut, vrf) || !linkHasAddress(linkOut, target.MAC) {
		return false
	}

	// Addressing must match the uplink's mode: a numbered uplink carries exactly
	// its local transit CIDR (no stale extras); an unnumbered uplink has no
	// IPv4 at all and its LRP still sends periodic RAs (so the veth acquires the
	// LRP's link-local). This makes a numbered<->unnumbered or address flip
	// fail convergence so attachUplink repairs it rather than leaving the old
	// address/RA state behind.
	if target.LocalIP.IsValid() {
		cidrs, err := vethIPv4CIDRs(ctx, host)
		if err != nil || len(cidrs) != 1 || cidrs[0] != target.LocalIP.String() {
			return false
		}
	} else {
		send, err := nbGetIfExists(ctx, s, "Logical_Router_Port", target.RouterPort, "ipv6_ra_configs:send_periodic")
		if err != nil || strings.TrimSpace(send) != "true" {
			return false
		}
		cidrs, err := vethIPv4CIDRs(ctx, host)
		if err != nil || len(cidrs) != 0 {
			return false
		}
	}

	// The OVS side must exist on br-int with our iface-id.
	ifaceID, err := VsctlGetIfExists(ctx, s, "Interface", ovs, "external_ids", "iface-id")
	if err != nil || strings.TrimSpace(ifaceID) != lsp {
		return false
	}

	// The dynamic-routing-port-mapping entry must map the port to the host veth.
	mapping, err := VsctlGetIfExists(ctx, s, "Open_vSwitch", ".", "external-ids", "dynamic-routing-port-mapping")
	if err != nil || !mappingContains(mapping, target.PortName, host) {
		return false
	}

	// The LRP must carry the redirect to our LSP.
	redirect, err := nbGetIfExists(ctx, s, "Logical_Router_Port", target.RouterPort, "options:routing-protocol-redirect")
	if err != nil || strings.TrimSpace(redirect) != lsp {
		return false
	}

	// The LRP's routing-protocols must name both BGP and BFD: without BFD, OVN
	// would not redirect UDP 3784 to our veth and BFD would never come up,
	// silently losing fast failure detection for a redundant fabric that expects
	// it (a mid-attach rewrite back to BGP-only once BFD is live would also drop
	// the established session). Fail convergence so attachUplink re-stamps it.
	rp, err := nbGetIfExists(ctx, s, "Logical_Router_Port", target.RouterPort, "options:routing-protocols")
	if err != nil || !routingProtocolsHas(rp, "BGP") || !routingProtocolsHas(rp, "BFD") {
		return false
	}

	return true
}

// routingProtocolsHas reports whether a routing-protocols list (a
// comma-separated set like "BGP,BFD", optionally whitespace padded) contains
// proto. A missing/empty list never matches.
func routingProtocolsHas(routingProtocols, proto string) bool {
	for _, p := range strings.Split(routingProtocols, ",") {
		if strings.TrimSpace(p) == proto {
			return true
		}
	}
	return false
}

// linkHasMaster reports whether `ip -d link show` output enslaves a device to
// the named master, using a token match so e.g. ovnvrf10 does not match
// ovnvrf100.
func linkHasMaster(linkOut string, master string) bool {
	fields := strings.Fields(linkOut)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "master" && fields[i+1] == master {
			return true
		}
	}
	return false
}

// vethIPv4CIDRs returns the IPv4 "ip/len" addresses currently on a veth, from
// `ip -o addr show`. Used to detect stale addresses left behind by a
// numbered<->unnumbered or address flip so they can be flushed.
func vethIPv4CIDRs(ctx context.Context, iface string) ([]string, error) {
	out, err := shared.RunCommand(ctx, "ip", "-o", "addr", "show", "dev", iface)
	if err != nil {
		return nil, err
	}
	var cidrs []string
	fields := strings.Fields(out)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "inet" {
			cidrs = append(cidrs, fields[i+1])
		}
	}
	return cidrs, nil
}

// linkHasAddress reports whether `ip -d link show` output shows the given MAC.
func linkHasAddress(linkOut string, mac string) bool {
	fields := strings.Fields(linkOut)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "link/ether" && fields[i+1] == mac {
			return true
		}
	}
	return false
}

// mappingContains reports whether the comma-separated dynamic-routing-port-mapping
// value contains an exact "key=value" entry.
func mappingContains(mapping string, key string, value string) bool {
	for _, m := range strings.Split(mapping, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(m), "=")
		if ok && k == key && v == value {
			return true
		}
	}
	return false
}

// attachUplink creates the veth pair, binds the host side to the OVN VRF, adds
// the ovs side to br-int, and wires the redirect on the target LRP. It is
// idempotent: if the attachment is already fully present it returns nil, and if
// a previous attempt failed part-way it repairs the missing pieces.
func attachUplink(ctx context.Context, s microTypes.State, cfg attachConfig, target attachTarget) error {
	host, ovs := attachInterfaces(target.RouterPort)
	lsp := attachPortName(s.Name(), target.RouterPort)
	vrf := getVrfName(strconv.FormatUint(uint64(cfg.LearnTable), 10))

	// Already converged? Return early so the steady-state tick doesn't churn
	// subprocess calls, but still repair a partial attach from an earlier tick.
	if attachConverged(ctx, s, cfg, target) {
		return nil
	}

	// Record ownership in the local journal FIRST. If any later step fails (VRF
	// not ready, add-port, mapping, NB commit), the LRP is still journaled, so
	// the next tick re-runs this idempotently (or disableAttach tears it down)
	// instead of wedging.
	if err := addOwnedPort(ctx, s, target.RouterPort); err != nil {
		return err
	}

	// The VRF is owned by OVN: wait for it rather than creating a competing one.
	if _, err := shared.RunCommand(ctx, "ip", "link", "show", "dev", vrf); err != nil {
		return fmt.Errorf("OVN VRF %s not ready: %w", vrf, err)
	}

	// Create the veth pair if absent.
	if _, err := shared.RunCommand(ctx, "ip", "link", "show", "dev", host); err != nil {
		if _, err := shared.RunCommand(ctx, "ip", "link", "add", host, "type", "veth", "peer", "name", ovs); err != nil {
			return err
		}
	}

	for _, args := range [][]string{
		{"link", "set", "dev", host, "address", target.MAC},
		{"link", "set", "dev", host, "master", vrf},
		{"link", "set", "dev", host, "up"},
		{"link", "set", "dev", ovs, "up"},
	} {
		if _, err := shared.RunCommand(ctx, "ip", args...); err != nil {
			return err
		}
	}

	// Address the veth per its mode. A numbered uplink gets exactly its local
	// transit CIDR (the kernel derives the connected route to the transit
	// prefix, which is what makes the IPv4 peer directly reachable); any other
	// IPv4 present (from a previous address or an unnumbered->numbered flip) is
	// flushed. An unnumbered uplink carries no IPv4 at all, so any leftover is
	// flushed too. This is intentionally converge-on-change, not add-only, so a
	// mode/address flip never leaves a stale address or connected route.
	{
		cidrs, err := vethIPv4CIDRs(ctx, host)
		if err != nil {
			return err
		}
		want := target.LocalIP.String()
		haveWant := false
		for _, c := range cidrs {
			if target.LocalIP.IsValid() && c == want {
				haveWant = true
				continue
			}
			if _, err := shared.RunCommand(ctx, "ip", "addr", "del", c, "dev", host); err != nil {
				return fmt.Errorf("failed to remove stale address %s from %s: %w", c, host, err)
			}
		}
		if target.LocalIP.IsValid() && !haveWant {
			if _, err := shared.RunCommand(ctx, "ip", "addr", "add", want, "dev", host); err != nil {
				return fmt.Errorf("failed to add %s to %s: %w", want, host, err)
			}
		}
	}

	brInt, err := getOvnIntegrationBridge(ctx, s)
	if err != nil {
		return err
	}

	if _, err := ovnCmd.VSCtl(ctx, s,
		"--may-exist", "add-port", brInt, ovs,
		"--", "set", "Interface", ovs,
		"external_ids:iface-id="+lsp,
	); err != nil {
		return err
	}

	if err := updatePortMapping(ctx, s, target.PortName, host); err != nil {
		return err
	}

	// Remember the owner's pre-attach routing-protocols/RA values (idempotent) and
	// set routing-protocols to BGP,BFD while attached (mirroring the managed path
	// in redirect.go) so OVN redirects both BGP and BFD to our veth. BIRD listens
	// for BFD passively (attachBirdConfig), so BFD is only used when the peer has
	// it configured; peers that do not speak BFD are unaffected. On detach the
	// owner's original value is restored rather than deleted. If the snapshot
	// failed we must NOT stamp routing-protocols (or the redirect depends on it):
	// detach would otherwise have no record and would remove the owner's real
	// value. Fail the attach; the reconciler retries on the next tick.
	if err := recordOriginalOptions(ctx, s, target.RouterPort); err != nil {
		return fmt.Errorf("failed to snapshot owner's pre-attach options for %s: %w", target.RouterPort, err)
	}

	// LRP set is conditional: an unnumbered uplink asks the LRP to send
	// periodic RAs so the host veth acquires a link-local address (mirrors the
	// managed path in redirect.go); a numbered uplink has an explicit static
	// IPv4 on the veth and needs no RA.
	lrpSet := []string{
		"options:routing-protocols=BGP,BFD",
		"options:routing-protocol-redirect=" + lsp,
	}
	if !target.LocalIP.IsValid() {
		lrpSet = append(lrpSet,
			"ipv6_ra_configs:send_periodic=true",
			"ipv6_ra_configs:address_mode=slaac",
			"ipv6_ra_configs:max_interval=4",
			"ipv6_ra_configs:min_interval=3",
		)
	}

	args := []string{
		"--may-exist", "lsp-add", target.Switch, lsp,
		"--", "lsp-set-addresses", lsp, "unknown",
		"--", "set", "Logical_Router_Port", target.RouterPort,
	}
	args = append(args, lrpSet...)
	if _, err = ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, args...); err != nil {
		return err
	}

	return nil
}

// detachPort removes the resources created for a single attachment. Ownership
// is established by the caller (the LRP was in the local journal), and the
// veth/LSP/redirect names are deterministic, so removal is by name with
// --if-exists rather than by an external_ids ownership tag. It re-reads the
// dynamic-routing-port-name before mutating anything so it can restore the
// mapping entry without a prior discovery pass.
func detachPort(ctx context.Context, s microTypes.State, routerPort string) error {
	host, ovs := attachInterfaces(routerPort)
	lsp := attachPortName(s.Name(), routerPort)

	portName, err := nbGetIfExists(ctx, s, "Logical_Router_Port", routerPort, "options:dynamic-routing-port-name")
	if err != nil {
		return err
	}
	portName = strings.TrimSpace(portName)

	// Remove our redirect option if it still points at our LSP (or is unset),
	// then restore the owner's routing-protocols and ipv6_ra_configs originals.
	redirect, err := nbGetIfExists(ctx, s, "Logical_Router_Port", routerPort, "options:routing-protocol-redirect")
	if err != nil {
		return err
	}
	if redirect == "" || strings.TrimSpace(redirect) == lsp {
		if _, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait,
			"--if-exists", "remove", "Logical_Router_Port", routerPort, "options", "routing-protocol-redirect",
		); err != nil {
			return err
		}
	}
	if err := restoreOriginalOptions(ctx, s, routerPort); err != nil {
		return err
	}

	// Remove the redirect LSP.
	if _, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--if-exists", "lsp-del", lsp); err != nil {
		return err
	}

	// Remove the ovs interface.
	if _, err := ovnCmd.VSCtl(ctx, s, "--if-exists", "del-port", ovs); err != nil {
		return err
	}

	// Remove the veth if present.
	if _, err := os.Stat("/sys/class/net/" + host); err == nil {
		if _, err := shared.RunCommand(ctx, "ip", "link", "delete", host); err != nil {
			return err
		}
	}

	// Restore the dynamic-routing-port-mapping entry and drop the owned-port
	// journal entry. Only once the whole detach has succeeded do we clear the
	// original-options journal: if any earlier step failed transiently, the port
	// stays owned and the reconciler's retry re-runs restoreOriginalOptions with
	// the journal still intact (idempotent) rather than mis-deleting the values
	// it just restored.
	if err := updatePortMapping(ctx, s, portName, ""); err != nil {
		return err
	}
	if err := removeOwnedPort(ctx, s, routerPort); err != nil {
		return err
	}
	return clearOriginalOptions(ctx, s, routerPort)
}

// updatePortMapping adds/removes a single key from the
// external_ids:dynamic-routing-port-mapping on Open_vSwitch, preserving all
// unrelated entries.
func updatePortMapping(ctx context.Context, s microTypes.State, key, value string) error {
	old, err := VsctlGetIfExists(ctx, s, "Open_vSwitch", ".", "external-ids", "dynamic-routing-port-mapping")
	if err != nil {
		return err
	}

	var mappings []string
	for _, m := range strings.Split(old, ",") {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		k, _, ok := strings.Cut(m, "=")
		if !ok || k != key {
			mappings = append(mappings, m)
		}
	}
	if value != "" {
		mappings = append(mappings, key+"="+value)
	}

	if len(mappings) == 0 {
		_, err = ovnCmd.VSCtl(ctx, s, "remove", "Open_vSwitch", ".", "external_ids", "dynamic-routing-port-mapping")
		return err
	}
	_, err = ovnCmd.VSCtl(ctx, s, "set", "Open_vSwitch", ".", "external_ids:dynamic-routing-port-mapping="+strings.Join(mappings, ","))
	return err
}

// attachBirdConfig renders the BIRD configuration for attach mode. It uses the
// advertise-table kernel protocol (learning /32 originations shipped by the
// topology owner), a pipe of defaults into a learned table, and an egress
// kernel protocol writing into the OVN VRF (learn-table). Only /32 host routes
// from the advertisement protocol are exported to peers, with the kernel route
// metric preserved as BGP MED. BFD is wired in the same passive way as the
// managed path: a global strict-bind BFD socket plus per-neighbor "passive yes",
// so BFD is only used when the peer has it configured.
func attachBirdConfig(cfg attachConfig) string {
	var b strings.Builder
	vrfName := getVrfName(strconv.FormatUint(uint64(cfg.LearnTable), 10))
	fmt.Fprintf(&b, "# Generated by microovn attach mode.\nlog syslog all;\nrouter id %s;\nprotocol device {}\n", cfg.RouterID)
	fmt.Fprintf(&b, `protocol bfd {
 strict bind yes;
}
protocol kernel advertisements {
 kernel table %d;
 learn;
 ipv4 { import filter { if net.len = 32 then accept; reject; }; export none; };
}
ipv4 table learned;
protocol pipe to_ovn {
 table master4;
 peer table learned;
 import none;
 export filter { if source = RTS_BGP && net = 0.0.0.0/0 then accept; reject; };
}
protocol kernel egress {
 kernel table %d;
 ipv4 { table learned; import none; export all; };
}
`, cfg.AdvertiseTable, cfg.LearnTable)
	for i, target := range cfg.Targets {
		host, _ := attachInterfaces(target.RouterPort)
		for j, p := range target.Peers {
			if target.LocalIP.IsValid() {
				// Numbered: explicit local (the LRP's own transit address,
				// which the veth shares with the LRP) and a plain IPv4 next hop
				// — no `interface`/IPv6-extended-next-hop, which are the
				// unnumbered mechanisms.
				fmt.Fprintf(&b, `protocol bgp uplink_%d_peer_%d {
 vrf "%s";
 local as %d;
 local %s;
 neighbor %s as %d;
 ipv4 {
  next hop self;
  import filter { if net = 0.0.0.0/0 then accept; reject; };
  export filter {
   if proto = "advertisements" && net.len = 32 && defined(krt_metric) then {
    bgp_med = krt_metric;
    accept;
   }
   reject;
  };
 };
 bfd { passive yes; };
}
`, i, j, vrfName, cfg.LocalASN, target.LocalIP.Addr(), p.Address, p.ASN)
				continue
			}
			// Unnumbered (unchanged from the pre-numbered render): pin the
			// source via `interface` + IPv6 extended-next-hop so the IPv4
			// routes ride an IPv6 link-local peer.
			fmt.Fprintf(&b, `protocol bgp uplink_%d_peer_%d {
 vrf "%s";
 interface "%s";
 local as %d;
 neighbor %s as %d;
 ipv4 {
  extended next hop on;
  require extended next hop on;
  next hop self;
  import filter { if net = 0.0.0.0/0 then accept; reject; };
  export filter {
   if proto = "advertisements" && net.len = 32 && defined(krt_metric) then {
    bgp_med = krt_metric;
    accept;
   }
   reject;
  };
 };
 bfd { passive yes; };
}
`, i, j, vrfName, host, cfg.LocalASN, p.Address, p.ASN)
		}
	}
	return b.String()
}

// writeBirdConfig renders the attach BIRD config and applies it.
func writeBirdConfig(ctx context.Context, s microTypes.State, cfg attachConfig) error {
	return applyBirdConfig(ctx, s, attachBirdConfig(cfg))
}

// resetBirdConfig restores BIRD's default configuration, used by the
// full-teardown path so a stopped daemon is not left with a config that
// references removed veths.
func resetBirdConfig(ctx context.Context) error {
	if _, err := shared.RunCommand(ctx, "cp", paths.BirdDefaultConfig(), paths.BirdConfigFile()); err != nil {
		return fmt.Errorf("failed to reset Bird config: %w", err)
	}
	return nil
}

// birdConfigIsAttach reports whether the on-disk BIRD config was generated by
// attach mode. It is used to decide whether to tear BIRD down once there are no
// local attach targets left, even if the last teardown attempt failed on a
// previous tick.
func birdConfigIsAttach() bool {
	data, err := os.ReadFile(paths.BirdConfigFile())
	return err == nil && strings.Contains(string(data), "# Generated by microovn attach mode.")
}

// applyBirdConfig validates the given BIRD config with `bird -p`, writes it to
// the running config location, starts BIRD and reloads it via birdc (retrying
// briefly for the control socket to come up). It is used by both attach mode
// and `microovn setup bird`.
func applyBirdConfig(ctx context.Context, s microTypes.State, content string) error {
	// Serialise access to the shared BIRD config file + candidate across the
	// reconciler, setup bird and the managed path.
	birdConfigMu.Lock()
	defer birdConfigMu.Unlock()

	// Skip the whole apply if the running config is already identical AND BIRD
	// is actually up: re-running bird -p + write + snapctl start + birdc
	// configure every reconcile tick would otherwise generate snapd
	// service-control changes (same contention class as the snap-refresh race)
	// and a pointless BIRD reload. The liveness check means a BIRD that
	// crashed/stopped while the daemon stayed up is still brought back.
	if current, err := os.ReadFile(paths.BirdConfigFile()); err == nil && string(current) == content {
		if _, err := shared.RunCommand(ctx, filepath.Join(paths.Wrappers(), "birdc"), "show", "status"); err == nil {
			return nil
		}
	}

	candidate := filepath.Join(paths.BirdConfigDir(), "80-bgp-attach.candidate")
	if err := os.WriteFile(candidate, []byte(content), 0o600); err != nil {
		return fmt.Errorf("failed to write BIRD candidate config: %w", err)
	}
	defer os.Remove(candidate)

	birdBin := "bird"
	if snapRoot := os.Getenv("SNAP"); snapRoot != "" {
		birdBin = filepath.Join(snapRoot, "usr/sbin/bird")
	}
	if _, err := shared.RunCommand(ctx, birdBin, "-p", "-c", candidate); err != nil {
		return fmt.Errorf("invalid BIRD config: %w", err)
	}

	// The candidate validated; commit it to the real config file that the BIRD
	// snap service reads.
	if err := os.WriteFile(paths.BirdConfigFile(), []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write BIRD config: %w", err)
	}

	if err := snap.Start(ctx, BirdService, true); err != nil {
		return err
	}

	var lastErr error
	for i := 0; i < 10; i++ {
		_, err := shared.RunCommand(ctx, filepath.Join(paths.Wrappers(), "birdc"), "configure")
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("birdc configure failed: %w", lastErr)
}
