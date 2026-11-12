package bgp

// Uplink discovery for bgp-attach: find a chassis's BGP uplink LRP without
// being told its name. NB is cluster-wide, so this works for any chassis from
// any member (the node the operator talks to uses it for dry-run and status).

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"slices"
	"strings"

	microTypes "github.com/canonical/microcluster/v3/microcluster/types"

	"github.com/canonical/microovn/microovn/api/types"
	"github.com/canonical/microovn/microovn/database"
	ovnCmd "github.com/canonical/microovn/microovn/ovn/cmd"
)

// parseUUIDSet parses an OVSDB set as printed by `ovn-nbctl get`, e.g.
// "[1b2c..., 3d4e...]", a bare single uuid, or "[]".
func parseUUIDSet(raw string) []string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "[")
	raw = strings.TrimSuffix(raw, "]")
	var out []string
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.Trim(strings.TrimSpace(entry), "\"")
		if entry != "" {
			out = append(out, entry)
		}
	}
	return out
}

// matchUplinkNames returns the names matching the glob pattern, sorted.
func matchUplinkNames(pattern string, names []string) []string {
	var out []string
	for _, n := range names {
		if ok, err := path.Match(pattern, n); err == nil && ok {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// pickUplink chooses the single uplink LRP for a chassis. Exactly one is
// required: one node gets one address, so the command never guesses.
func pickUplink(chassis, pattern string, candidates []string) (string, error) {
	switch len(candidates) {
	case 0:
		return "", fmt.Errorf("no router port matching %q on a router pinned to chassis %q (has the topology owner created this node's BGP router port yet?)", pattern, chassis)
	case 1:
		return candidates[0], nil
	default:
		return "", fmt.Errorf("%d router ports match %q on chassis %q, expected exactly one: %s (narrow --uplink-pattern)",
			len(candidates), pattern, chassis, strings.Join(candidates, ", "))
	}
}

// chassisRouterPortNames lists the LRP names of routers pinned to chassis
// (options:chassis), skipping MicroOVN's own managed-mode routers.
func chassisRouterPortNames(ctx context.Context, s microTypes.State, chassis string) ([]string, error) {
	routersOut, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--bare", "--columns=name",
		"find", "Logical_Router", fmt.Sprintf("options:chassis=%q", chassis))
	if err != nil {
		return nil, fmt.Errorf("failed to list routers pinned to %q: %w", chassis, err)
	}

	var names []string
	for _, router := range parseOvnFind(routersOut) {
		managed, err := nbGetIfExists(ctx, s, "Logical_Router", router, "external_ids:"+BgpManagedTag)
		if err != nil {
			return nil, err
		}
		if managed != "" {
			continue
		}
		portsOut, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "get", "Logical_Router", router, "ports")
		if err != nil {
			return nil, fmt.Errorf("failed to list ports of router %s: %w", router, err)
		}
		for _, uuid := range parseUUIDSet(portsOut) {
			name, err := nbGetIfExists(ctx, s, "Logical_Router_Port", uuid, "name")
			if err != nil {
				return nil, err
			}
			if name = strings.TrimSpace(name); name != "" {
				names = append(names, name)
			}
		}
	}
	return names, nil
}

// uplinkHasLocalnet reports whether the switch the LRP connects to has a
// localnet port, i.e. it reaches a physical network (the leaf). This keeps
// internal router-to-router ports out even if a pattern is too broad.
func uplinkHasLocalnet(ctx context.Context, s microTypes.State, lrp string) (bool, error) {
	lspOut, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--bare", "--columns=_uuid",
		"find", "Logical_Switch_Port", "type=router", "options:router-port="+lrp)
	if err != nil {
		return false, err
	}
	lsps := parseOvnFind(lspOut)
	if len(lsps) != 1 {
		return false, nil
	}
	swOut, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--bare", "--columns=_uuid",
		"find", "Logical_Switch", fmt.Sprintf("ports{>=}%s", lsps[0]))
	if err != nil {
		return false, err
	}
	switches := parseOvnFind(swOut)
	if len(switches) != 1 {
		return false, nil
	}
	portsOut, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "get", "Logical_Switch", switches[0], "ports")
	if err != nil {
		return false, err
	}
	localnetOut, err := ovnCmd.NBCtlCluster(ctx, s, ovnCmd.DefaultDBStateWait, "--bare", "--columns=_uuid",
		"find", "Logical_Switch_Port", "type=localnet")
	if err != nil {
		return false, err
	}
	localnets := parseOvnFind(localnetOut)
	for _, p := range parseUUIDSet(portsOut) {
		if slices.Contains(localnets, p) {
			return true, nil
		}
	}
	return false, nil
}

// findChassisUplink finds the chassis's BGP uplink LRP: a port whose name
// matches pattern, on a router pinned to the chassis, connected to a switch
// with a localnet port.
func findChassisUplink(ctx context.Context, s microTypes.State, chassis, pattern string) (string, error) {
	names, err := chassisRouterPortNames(ctx, s, chassis)
	if err != nil {
		return "", err
	}
	var candidates []string
	for _, name := range matchUplinkNames(pattern, names) {
		ok, err := uplinkHasLocalnet(ctx, s, name)
		if err != nil {
			return "", err
		}
		if ok {
			candidates = append(candidates, name)
		}
	}
	return pickUplink(chassis, pattern, candidates)
}

// clusterMembers returns every cluster member's dqlite member ID (the same ID
// the ASN is derived from) and "host:port" address, keyed by member name.
func clusterMembers(ctx context.Context, s microTypes.State) (ids map[string]int64, addresses map[string]string, err error) {
	ids = map[string]int64{}
	addresses = map[string]string{}
	err = s.Database().Transaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, name, address FROM core_cluster_members`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var name, address string
			if err := rows.Scan(&id, &name, &address); err != nil {
				return err
			}
			ids[name] = id
			addresses[name] = address
		}
		return rows.Err()
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list cluster members: %w", err)
	}
	return ids, addresses, nil
}

// clusterMemberIDs returns every cluster member's name mapped to its member ID.
func clusterMemberIDs(ctx context.Context, s microTypes.State) (map[string]int64, error) {
	ids, _, err := clusterMembers(ctx, s)
	return ids, err
}

// switchMembers returns the names of members running the OVS switch service,
// i.e. the chassis that can attach.
func switchMembers(ctx context.Context, s microTypes.State) (map[string]bool, error) {
	out := map[string]bool{}
	srv := string(types.SrvSwitch)
	err := s.Database().Transaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		services, err := database.GetServices(ctx, tx, database.ServiceFilter{Service: &srv})
		if err != nil {
			return err
		}
		for _, svc := range services {
			out[svc.Member] = true
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list switch members: %w", err)
	}
	return out, nil
}

// PlanAttachForMember works out what bgp-attach does on one member of a group:
// its uplink, its allocated address and whether it is attached right now. Errors are reported in the
// plan, not returned, so one bad node never hides the others.
func PlanAttachForMember(ctx context.Context, s microTypes.State, intent AttachIntent, member string, memberID int64) types.BgpAttachNodePlan {
	plan := types.BgpAttachNodePlan{Member: member, MemberID: memberID, Peer: intent.PeerString()}

	if intent.Numbered() {
		addr, ok := intent.Addresses[member]
		if !ok {
			plan.Error = fmt.Sprintf("no address allocated in group %s; set the group again to allocate one", intent.Key())
			return plan
		}
		plan.Address = addr.String()
	}

	lrp, err := findChassisUplink(ctx, s, member, intent.UplinkPattern)
	if err != nil {
		plan.Error = err.Error()
		return plan
	}
	plan.RouterPort = lrp

	redirect, err := nbGetIfExists(ctx, s, "Logical_Router_Port", lrp, "options:routing-protocol-redirect")
	if err == nil && strings.TrimSpace(redirect) == attachPortName(member, lrp) {
		plan.Attached = true
	}
	return plan
}

// ClusterMemberIDs returns every member's name mapped to its cluster member ID.
func ClusterMemberIDs(ctx context.Context, s microTypes.State) (map[string]int64, error) {
	return clusterMemberIDs(ctx, s)
}

// ClusterMemberAddresses returns every member's name mapped to its "host:port"
// cluster address.
func ClusterMemberAddresses(ctx context.Context, s microTypes.State) (map[string]string, error) {
	_, addresses, err := clusterMembers(ctx, s)
	return addresses, err
}

// SwitchMembers returns the members running the OVS switch service.
func SwitchMembers(ctx context.Context, s microTypes.State) (map[string]bool, error) {
	return switchMembers(ctx, s)
}

// CheckAttachAdvertiseTable refuses an advertise table equal to the learn table
// (dynamic-routing-vrf-id) of the router owning lrp: BIRD would read the routes
// it is meant to advertise from the table it writes learned routes into.
func CheckAttachAdvertiseTable(ctx context.Context, s microTypes.State, intent AttachIntent, lrp string) error {
	router, _, err := resolveTargetRouter(ctx, s, lrp)
	if err != nil {
		return err
	}
	learn, err := resolveLearnTable(ctx, s, router)
	if err != nil {
		return err
	}
	if learn == intent.AdvertiseTable {
		return fmt.Errorf("--advertise-table %d is the VRF (learn) table of router %s; use the table the advertised routes are written to (e.g. the main router's VRF)", intent.AdvertiseTable, router)
	}
	return nil
}

// AttachLocalState reports which uplinks this chassis has attached (used to
// report disable results).
func AttachLocalState(ctx context.Context, s microTypes.State) ([]string, error) {
	return ownedAttachPorts(ctx, s)
}
