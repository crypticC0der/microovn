package bgp

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/canonical/lxd/shared"
	"github.com/canonical/lxd/shared/logger"
	microTypes "github.com/canonical/microcluster/v3/microcluster/types"

	ovnCmd "github.com/canonical/microovn/microovn/ovn/cmd"
)

// The interconnect stitches the two fabric-facing localnets together so provider
// traffic can reach the per-chassis BGP router. MicroOVN owns one NIC-less OVS
// bridge and the two cookie-scoped forwarding rules across it; the logical
// localnets (and their patch ports) are created by the topology owner's
// ovn-controller from the provider physnet mapping.
const (
	// interconnectBridgeName is the MicroOVN-owned, NIC-less OVS bridge that
	// carries the provider<->interconnect forwarding. Its name (not an
	// external-id tag) is the ownership marker: the owner never uses this name.
	interconnectBridgeName = "microovn-bgp-interconnect-bridge"

	// interconnectFlowCookie scopes the OpenFlow rules MicroOVN installs on the
	// interconnect bridge, so teardown removes only our own flows.
	interconnectFlowCookie = "0x6d6963726f706f63"
)

// Neutron Core OVN BGP topology constants. These are the conventions the
// server-side service plugin (and its naming helpers) publish; MicroOVN reads
// them rather than anything being handed over explicitly.
const (
	// neutronFlatNetworkExtKey is the external_ids key Neutron stamps on a flat
	// provider switch. It is the escaped form Neutron stores in the DB, with a
	// backslash-escaped colon: ovn-nbctl's column:key shorthand treats the first
	// unescaped colon as the column/key separator, so an unescaped
	// "neutron:provnet-network-type" key cannot be expressed on the ovn-nbctl
	// command line and must be written "neutron\:provnet-network-type".
	neutronFlatNetworkExtKey = "neutron\\:provnet-network-type"
	neutronFlatNetworkType   = "flat"
	neutronLocalnetPortKey   = "ovn-localnet-port"
	neutronMainRouterName    = "bgp-lr-main"
	neutronNamePrefix        = "neutron-"
	neutronProvnetPortPrefix = "provnet-"
)

// interconnectFacts is the small, owner-agnostic set of facts the interconnect
// engine needs. Neutron is the first resolver; a future MicroCloud resolver
// produces the same struct.
type interconnectFacts struct {
	// ProviderPhysnet is the provider localnet's options:network_name, used as
	// the physnet key in the ovn-bridge-mappings entry for the interconnect
	// bridge. Both the provider localnet and the interconnect localnet live on
	// this one physnet, so one entry patches them both onto the IC bridge.
	ProviderPhysnet string
	// ProviderLocalnetLSP is the localnet LSP name the provider patch carries
	// in its Port's external_ids:ovn-localnet-port. The interconnect patch is
	// the second patch port that does not match this name.
	ProviderLocalnetLSP string
	// IngressMAC is the ingress (main) router's interconnect-port MAC, which the
	// provider->interconnect forwarding rule rewrites the destination to. Empty
	// until the router port is resolvable (treated as not-ready for flows).
	IngressMAC string
}

// interconnectResolver resolves the owner-specific facts the interconnect needs.
type interconnectResolver interface {
	// Resolve returns the facts for this owner, or present=false (with no error)
	// when the owner's topology is absent on the node, so the engine can skip it.
	Resolve(ctx context.Context, s microTypes.State) (interconnectFacts, bool, error)
}

// interconnectResolvers are tried in order until one reports the topology is
// present. A node runs a single topology owner in practice, so the first match
// wins. Only the Neutron resolver ships today, so a non-Neutron attach node
// resolves "absent" and the reconciler tears the (absent) bridge down each tick
// — which is a cheap no-op until the MicroCloud resolver lands behind this seam.
var interconnectResolvers = []interconnectResolver{
	neutronInterconnect{},
}

func resolveInterconnect(ctx context.Context, s microTypes.State) (interconnectFacts, bool, error) {
	for _, r := range interconnectResolvers {
		facts, present, err := r.Resolve(ctx, s)
		if err != nil {
			return interconnectFacts{}, false, err
		}
		if present {
			return facts, true, nil
		}
	}
	return interconnectFacts{}, false, nil
}

// neutronInterconnect resolves the interconnect from the Neutron Core OVN BGP
// topology: flat provider network -> provider network name -> interconnect
// switch -> bgp-lr-main interconnect-port MAC.
type neutronInterconnect struct{}

func (neutronInterconnect) Resolve(ctx context.Context, s microTypes.State) (interconnectFacts, bool, error) {
	var facts interconnectFacts

	out, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--bare", "--columns=name",
		"find", "Logical_Switch", "external_ids:"+neutronFlatNetworkExtKey+"="+neutronFlatNetworkType)
	if err != nil {
		return facts, false, err
	}
	switches := parseOvnFind(out)
	if len(switches) == 0 {
		// No flat provider switch: not a Neutron BGP topology (or it has not
		// been created yet). Either way there is nothing to interconnect.
		return facts, false, nil
	}
	if len(switches) > 1 {
		return facts, false, fmt.Errorf("multiple flat provider switches (%v); only one is supported", switches)
	}
	providerSwitch := switches[0]

	if !strings.HasPrefix(providerSwitch, neutronNamePrefix) {
		return facts, false, fmt.Errorf("flat provider switch %q does not follow the %q naming convention", providerSwitch, neutronNamePrefix)
	}

	// The provider localnet LSP is provnet-<network-id>; its options:network_name
	// is the physnet the interconnect bridge must be mapped under.
	facts.ProviderLocalnetLSP = providerLocalnetLspName(providerSwitch)

	physnet, err := nbGetIfExists(ctx, s, "Logical_Switch_Port", facts.ProviderLocalnetLSP, "options:network_name")
	if err != nil {
		return facts, false, err
	}
	facts.ProviderPhysnet = strings.TrimSpace(physnet)

	// The main router's interconnect-port MAC is the destination MAC the
	// provider->interconnect flow rewrites to. Empty until bgp-lr-main's LRP
	// exists (treated as not-ready, never a hard failure).
	ingressLRP := mainRouterInterconnectLrpName(providerSwitch)
	mac, err := nbGetIfExists(ctx, s, "Logical_Router_Port", ingressLRP, "mac")
	if err != nil {
		return facts, false, err
	}
	facts.IngressMAC = strings.ToLower(strings.TrimSpace(mac))

	return facts, true, nil
}

// providerLocalnetLspName derives the provider switch's localnet LSP name from
// the switch name (neutron-<id> -> provnet-<id>).
func providerLocalnetLspName(providerSwitch string) string {
	return neutronProvnetPortPrefix + strings.TrimPrefix(providerSwitch, neutronNamePrefix)
}

// providerInterconnectSwitchName mirrors Neutron's
// get_provider_interconnect_switch_name.
func providerInterconnectSwitchName(providerSwitch string) string {
	return "bgp-ls-interconnect-" + providerSwitch
}

// mainRouterInterconnectLrpName mirrors Neutron's
// get_lrp_name(MAIN_ROUTER_NAME, interconnect_switch_name): the bgp-lr-main LRP
// facing the interconnect switch, whose MAC the provider->interconnect flow
// rewrites to.
func mainRouterInterconnectLrpName(providerSwitch string) string {
	return "bgp-lrp-" + neutronMainRouterName + "-to-" + providerInterconnectSwitchName(providerSwitch)
}

// reconcileInterconnect brings the interconnect bridge, its ovn-bridge-mappings
// entry and its forwarding rules in line with the resolved owner topology. It
// tears down the bridge when the topology is absent.
func reconcileInterconnect(ctx context.Context, s microTypes.State) error {
	facts, present, err := resolveInterconnect(ctx, s)
	if err != nil {
		return err
	}
	if !present || facts.ProviderPhysnet == "" {
		return teardownInterconnect(ctx, s)
	}

	if err := ensureInterconnect(ctx, s, facts.ProviderPhysnet); err != nil {
		return err
	}
	return reconcileInterconnectFlows(ctx, s, facts)
}

// ensureInterconnect creates the NIC-less interconnect bridge and registers it
// in ovn-bridge-mappings under the provider physnet, so ovn-controller patches
// the two provider-physnet localnets onto it. Idempotent.
func ensureInterconnect(ctx context.Context, s microTypes.State, physnet string) error {
	if _, err := ovnCmd.VSCtl(ctx, s, "--may-exist", "add-br", interconnectBridgeName); err != nil {
		return fmt.Errorf("failed to create interconnect bridge %s: %w", interconnectBridgeName, err)
	}
	return setInterconnectMapping(ctx, s, physnet)
}

// setInterconnectMapping inserts/removes the interconnect bridge's
// ovn-bridge-mappings entry (physnet:bridge), preserving every unrelated entry.
func setInterconnectMapping(ctx context.Context, s microTypes.State, physnet string) error {
	current, err := VsctlGetIfExists(ctx, s, "Open_vSwitch", ".", "external_ids", "ovn-bridge-mappings")
	if err != nil {
		return err
	}
	updated := interconnectMapping(current, physnet)
	if updated == current {
		return nil
	}
	if updated == "" {
		_, err = ovnCmd.VSCtl(ctx, s, "remove", "Open_vSwitch", ".", "external_ids", "ovn-bridge-mappings")
	} else {
		_, err = ovnCmd.VSCtl(ctx, s, "set", "Open_vSwitch", ".", "external_ids:ovn-bridge-mappings="+updated)
	}
	return err
}

// interconnectMapping computes the desired ovn-bridge-mappings value: it drops
// any existing entry that maps to the interconnect bridge (handling a physnet
// change) and, when physnet is non-empty, appends physnet:bridge. Unrelated
// entries are preserved verbatim.
func interconnectMapping(current, physnet string) string {
	var entries []string
	for _, entry := range strings.Split(current, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		_, bridge, ok := strings.Cut(entry, ":")
		if !ok {
			// Preserve whatever we cannot parse as physnet:bridge (e.g. a bare
			// bridge name) verbatim rather than dropping it.
			entries = append(entries, entry)
			continue
		}
		if bridge == interconnectBridgeName {
			continue
		}
		entries = append(entries, entry)
	}
	if physnet != "" {
		entries = append(entries, physnet+":"+interconnectBridgeName)
	}
	sort.Strings(entries)
	return strings.Join(entries, ",")
}

// reconcileInterconnectFlows installs (or refreshes) the two forwarding rules
// once the patches and the ingress MAC are ready; otherwise it waits for the
// next tick. It never hard-fails a half-formed topology.
func reconcileInterconnectFlows(ctx context.Context, s microTypes.State, facts interconnectFacts) error {
	mac, ok := normalizeMAC(facts.IngressMAC)
	if !ok {
		logger.Debugf("Interconnect ingress MAC %q is not resolvable yet; waiting", facts.IngressMAC)
		return nil
	}

	provider, interconnect, ready, err := interconnectPatchOfports(ctx, s, facts.ProviderLocalnetLSP)
	if err != nil {
		return err
	}
	if !ready {
		logger.Debugf("Interconnect patch ports are not ready yet; waiting")
		return nil
	}

	return applyInterconnectFlows(ctx, interconnectFlows(provider, interconnect, mac))
}

// interconnectPatchOfports resolves the provider and interconnect patch ofports
// on the interconnect bridge, and reports whether both are ready (type=patch
// with a positive ofport). The provider patch is the one whose Port carries the
// given localnet LSP in external_ids:ovn-localnet-port; the interconnect patch
// is the other one. This assumes the two-localnet model (provider + interconnect
// both on the provider physnet) — a third patch on the bridge is an error rather
// than a guess, since the forwarding rules have no meaning for it.
func interconnectPatchOfports(ctx context.Context, s microTypes.State, providerLsp string) (provider, interconnect int, ready bool, err error) {
	out, err := ovnCmd.VSCtl(ctx, s, "list-ports", interconnectBridgeName)
	if err != nil {
		return 0, 0, false, err
	}

	for _, port := range strings.Fields(out) {
		ifaceType, err := VsctlGetIfExists(ctx, s, "Interface", port, "type", "")
		if err != nil {
			return 0, 0, false, err
		}
		if strings.TrimSpace(ifaceType) != "patch" {
			continue
		}

		ofportRaw, err := VsctlGetIfExists(ctx, s, "Interface", port, "ofport", "")
		if err != nil {
			return 0, 0, false, err
		}
		ofport, err := strconv.Atoi(strings.TrimSpace(ofportRaw))
		if err != nil || ofport <= 0 {
			// ovn-controller may not have assigned the ofport yet.
			continue
		}

		lsp, err := VsctlGetIfExists(ctx, s, "Port", port, "external_ids", neutronLocalnetPortKey)
		if err != nil {
			return 0, 0, false, err
		}
		switch lsp {
		case providerLsp:
			if provider != 0 {
				return 0, 0, false, fmt.Errorf("multiple provider patches on interconnect bridge %s", interconnectBridgeName)
			}
			provider = ofport
		default:
			if interconnect != 0 {
				return 0, 0, false, fmt.Errorf("multiple interconnect patches on interconnect bridge %s", interconnectBridgeName)
			}
			interconnect = ofport
		}
	}

	if provider == 0 || interconnect == 0 {
		return 0, 0, false, nil
	}
	return provider, interconnect, true, nil
}

// interconnectFlows renders the two cookie-scoped forwarding rules: provider ->
// interconnect rewrites the destination MAC to the ingress router; interconnect
// -> provider forwards as-is.
func interconnectFlows(providerOfport, interconnectOfport int, ingressMAC string) []string {
	return []string{
		fmt.Sprintf("cookie=%s,priority=10,in_port=%d,actions=mod_dl_dst:%s,output:%d", interconnectFlowCookie, providerOfport, ingressMAC, interconnectOfport),
		fmt.Sprintf("cookie=%s,priority=10,in_port=%d,actions=output:%d", interconnectFlowCookie, interconnectOfport, providerOfport),
	}
}

// applyInterconnectFlows idempotently replaces the interconnect bridge's
// cookie-scoped flows with the given set.
func applyInterconnectFlows(ctx context.Context, flows []string) error {
	if _, err := shared.RunCommand(ctx, "ovs-ofctl", "del-flows", interconnectBridgeName, "cookie="+interconnectFlowCookie+"/-1"); err != nil {
		return fmt.Errorf("failed to clear interconnect flows: %w", err)
	}
	for _, flow := range flows {
		if _, err := shared.RunCommand(ctx, "ovs-ofctl", "add-flow", interconnectBridgeName, flow); err != nil {
			return fmt.Errorf("failed to install interconnect flow %q: %w", flow, err)
		}
	}
	return nil
}

// teardownInterconnect removes the interconnect bridge (and its flows) and its
// ovn-bridge-mappings entry. Idempotent: safe to run when nothing was created.
func teardownInterconnect(ctx context.Context, s microTypes.State) error {
	if _, err := ovnCmd.VSCtl(ctx, s, "--if-exists", "del-br", interconnectBridgeName); err != nil {
		logger.Warnf("Failed to delete interconnect bridge: %v", err)
	}
	return setInterconnectMapping(ctx, s, "")
}

// normalizeMAC validates a MAC and returns its canonical lowercase form,
// reporting false for anything that is not a valid unicast address.
func normalizeMAC(raw string) (string, bool) {
	mac, err := net.ParseMAC(strings.TrimSpace(raw))
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 {
		return "", false
	}
	return mac.String(), true
}
