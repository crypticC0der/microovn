package bgpapi

// `microovn setup bgp-attach` endpoints. Settings are per group: how one
// rack's members peer with that rack's leaf, identified by the peer. A group's
// members are the members labelled with its rack.
//
//	POST   bgp/attach            set one group (or dry-run it): validate,
//	                             resolve its members from the rack labels,
//	                             allocate addresses, store it, write the router
//	                             ports, ask the added and removed members to
//	                             converge
//	DELETE bgp/attach?peer=P     remove group P (no peer: every group)
//	GET    bgp/attach            every group, each member's plan/state, and
//	                             labelled members no group uses yet
//	POST   bgp/attach/rack       set or clear one member's rack label, which
//	                             adds, moves or removes it
//
// Repeating a call changes nothing. Every write is a read-modify-write in one
// database transaction (labels and groups together), so overlapping calls
// never hand out an address twice and a rack move is one step.
// The member handling the call then writes (or restores) every affected
// compute's router-port keys in one NB transaction, and asks those computes to
// set (or clear) their own Open_vSwitch knobs.
//
// Members also converge on their own every reconcile tick, so a member that is
// down during a change catches up without a rerun. When a request arrives as a
// cluster notification, the member only converges and reports its own state.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/canonical/lxd/shared/logger"
	microTypes "github.com/canonical/microcluster/v3/microcluster/types"

	"github.com/canonical/microovn/microovn/api/types"
	"github.com/canonical/microovn/microovn/bgp"
	microOvnClient "github.com/canonical/microovn/microovn/client"
	"github.com/canonical/microovn/microovn/config"
)

// BgpAttachSetupEndpoint backs `microovn setup bgp-attach`.
var BgpAttachSetupEndpoint = microTypes.Endpoint{
	Path:   "bgp/attach",
	Get:    microTypes.EndpointAction{Handler: getBgpAttachHandler, AllowUntrusted: false, ProxyTarget: true},
	Post:   microTypes.EndpointAction{Handler: postBgpAttachHandler, AllowUntrusted: false, ProxyTarget: true},
	Delete: microTypes.EndpointAction{Handler: deleteBgpAttachHandler, AllowUntrusted: false, ProxyTarget: true},
}

// planGroup computes the plan for each member of a group, in member order.
// Members that are not cluster members, or do not run the switch service, get
// a plan carrying that error.
func planGroup(ctx context.Context, s microTypes.State, group bgp.AttachIntent, ids map[string]int64, chassis map[string]bool) []types.BgpAttachNodePlan {
	plans := make([]types.BgpAttachNodePlan, 0, len(group.Members))
	for _, m := range group.Members {
		id, ok := ids[m]
		var plan types.BgpAttachNodePlan
		switch {
		case !ok:
			plan = types.BgpAttachNodePlan{Member: m, Peer: group.PeerString(),
				Error: "not a cluster member (see 'microovn cluster list')"}
		case !chassis[m]:
			plan = types.BgpAttachNodePlan{Member: m, MemberID: id, Peer: group.PeerString(),
				Error: "does not run the switch service, so it cannot attach"}
		default:
			plan = bgp.PlanAttachForMember(ctx, s, group, m, id)
		}
		plan.Rack = group.Rack
		plans = append(plans, plan)
	}
	return plans
}

// planMembers is planGroup for a subset of the group's members.
func planMembers(ctx context.Context, s microTypes.State, group bgp.AttachIntent, members []string, ids map[string]int64, chassis map[string]bool) []types.BgpAttachNodePlan {
	sub := group
	sub.Members = members
	return planGroup(ctx, s, sub, ids, chassis)
}

// attachWriteMu serialises group writes made through this member. Writes
// through different members are serialised by the database transaction.
var attachWriteMu sync.Mutex

// convergeMembers runs one attach pass on each named member (locally when the
// name is this member) and returns each member's reported state by name.
// Members that can't be reached are returned in unreached with the error.
// Members not named are not contacted, so nodes outside the change (including
// ones running an older MicroOVN without this endpoint) are left alone.
func convergeMembers(ctx context.Context, s microTypes.State, members map[string]bool, disable bool) (map[string]types.BgpAttachNodePlan, map[string]string, error) {
	addresses, err := bgp.ClusterMemberAddresses(ctx, s)
	if err != nil {
		return nil, nil, err
	}
	byAddr := map[string]string{}
	for name, addr := range addresses {
		byAddr[addr] = name
	}
	cluster, err := s.Connect().Cluster(true)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get cluster clients: %w", err)
	}

	var mu sync.Mutex
	reported := map[string]types.BgpAttachNodePlan{}
	unreached := map[string]string{}

	var local sync.WaitGroup
	if members[s.Name()] {
		local.Add(1)
		go func() {
			defer local.Done()
			if plan, ok := localConverge(ctx, s); ok {
				mu.Lock()
				reported[s.Name()] = plan
				mu.Unlock()
			}
		}()
	}

	err = cluster.Query(ctx, true, func(ctx context.Context, c microTypes.Client) error {
		name, ok := byAddr[c.URL().Host]
		if !ok || !members[name] {
			return nil
		}
		var resp types.SetupBgpAttachResponse
		var err error
		if disable {
			resp, err = microOvnClient.DisableBgpAttach(ctx, c, "")
		} else {
			resp, err = microOvnClient.SetupBgpAttach(ctx, c, types.SetupBgpAttachRequest{})
		}
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			unreached[name] = err.Error()
			return nil
		}
		for _, n := range resp.Nodes {
			reported[n.Member] = n
		}
		return nil
	})
	local.Wait()
	if err != nil {
		return nil, nil, err
	}
	return reported, unreached, nil
}

// localConverge runs one attach pass on this member and reports its own state.
// ok is false for members that are not a chassis (nothing to report).
func localConverge(ctx context.Context, s microTypes.State) (types.BgpAttachNodePlan, bool) {
	chassis, err := bgp.SwitchMembers(ctx, s)
	if err != nil {
		return types.BgpAttachNodePlan{Member: s.Name(), Error: err.Error()}, true
	}
	if !chassis[s.Name()] {
		return types.BgpAttachNodePlan{}, false
	}

	passErr := bgp.ReconcileAttach(ctx, s)

	groups, err := bgp.LoadAttachGroups(ctx, s)
	if err != nil {
		return types.BgpAttachNodePlan{Member: s.Name(), Error: err.Error()}, true
	}
	group := bgp.GroupForMember(groups, s.Name())

	if group == nil {
		// In no group: report whatever is still attached.
		plan := types.BgpAttachNodePlan{Member: s.Name()}
		attached, err := bgp.AttachLocalState(ctx, s)
		switch {
		case err != nil:
			plan.Error = err.Error()
		case len(attached) > 0:
			// Hand-stamped markers (not from a group) keep working.
			plan.Attached = true
			plan.RouterPort = attached[0]
		}
		if passErr != nil && plan.Error == "" {
			plan.Error = passErr.Error()
		}
		return plan, true
	}

	ids, err := bgp.ClusterMemberIDs(ctx, s)
	if err != nil {
		return types.BgpAttachNodePlan{Member: s.Name(), Peer: group.PeerString(), Error: err.Error()}, true
	}
	plan := bgp.PlanAttachForMember(ctx, s, *group, s.Name(), ids[s.Name()])
	plan.Rack = group.Rack
	if plan.Error == "" && !plan.Attached {
		if passErr != nil {
			plan.Error = passErr.Error()
		} else {
			plan.Error = "keys set but not attached yet; check 'sudo snap logs microovn.daemon'"
		}
	}
	return plan, true
}

func respond(resp types.SetupBgpAttachResponse) microTypes.Response {
	return microTypes.SyncResponse(true, &resp)
}

// summariseWarnings fills Success/Message/Warnings for a request that was
// carried out: per-member problems (not reached, router port not created yet,
// not attached yet) resolve on their own, so they are warnings and the request
// still succeeds. Only addNbErrors, called after it, can fail the request.
func summariseWarnings(resp *types.SetupBgpAttachResponse, okMessage string) {
	for _, n := range resp.Nodes {
		if n.Error != "" {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("%s: %s", n.Member, n.Error))
		}
	}
	resp.Success = true
	resp.Message = okMessage
	if len(resp.Warnings) > 0 {
		resp.Message = fmt.Sprintf("%s, with %d warning(s)", okMessage, len(resp.Warnings))
	}
}

// summarise fills Success/Message/Errors from the per-node plans. It is used
// for a member's own reply to a cluster notification.
func summarise(resp *types.SetupBgpAttachResponse, okMessage string) {
	for _, n := range resp.Nodes {
		if n.Error != "" {
			resp.Errors = append(resp.Errors, fmt.Sprintf("%s: %s", n.Member, n.Error))
		}
	}
	resp.Success = len(resp.Errors) == 0
	if resp.Success {
		resp.Message = okMessage
	} else {
		resp.Message = fmt.Sprintf("%s, with %d node error(s)", okMessage, len(resp.Errors))
	}
}

// sortNodes orders node rows by peer (group), then member.
func sortNodes(nodes []types.BgpAttachNodePlan) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Peer != nodes[j].Peer {
			return nodes[i].Peer < nodes[j].Peer
		}
		return nodes[i].Member < nodes[j].Member
	})
}

// collectReports turns convergeMembers results into node rows for members,
// filling in a row for members that did not report.
func collectReports(members map[string]bool, reported map[string]types.BgpAttachNodePlan, unreached map[string]string, fallback map[string]types.BgpAttachNodePlan) []types.BgpAttachNodePlan {
	var nodes []types.BgpAttachNodePlan
	for m := range members {
		if got, ok := reported[m]; ok {
			nodes = append(nodes, got)
			continue
		}
		row, ok := fallback[m]
		if !ok {
			row = types.BgpAttachNodePlan{Member: m}
		}
		if msg, ok := unreached[m]; ok {
			row.Error = fmt.Sprintf("not reached (%s); it will converge on its next reconcile tick", msg)
		} else if row.Error == "" {
			row.Error = "no report from this member; it will converge on its next reconcile tick"
		}
		nodes = append(nodes, row)
	}
	sortNodes(nodes)
	return nodes
}

// attachCall describes one change for applyAttachChange.
type attachCall struct {
	// change edits the stored state (labels and groups). It must only compute:
	// it is re-run if the database transaction is retried.
	change func(st *bgp.AttachState) error
	// focus is the key of the group the call is about (the group being set,
	// or the group of the member being labelled), "" for none. Its router
	// ports are rewritten even when nothing changed, so a rerun repairs a key
	// removed by hand, and it is reported even when unchanged.
	focus func(st bgp.AttachState) string
	// requireUplink makes the call fail when the focus group's uplink pattern
	// matches no router port on any member that needs one (a typo, not a
	// transient state).
	requireUplink bool
	// alsoConverge are members to converge and report on even if unchanged.
	alsoConverge []string
	// disable selects the notification used to reach the members.
	disable bool
	dryRun  bool
	// message builds the response message.
	message func(changes []bgp.GroupChange, focus *bgp.AttachIntent) string
}

// applyAttachChange runs one bgp-attach change end to end:
//
//  1. preview the change on the stored state (refuse it if invalid);
//  2. find the uplinks of members that need one, and record their current
//     values (outside the transaction: it may only compute);
//  3. store the change in one transaction, resolving every group again;
//  4. write the changed groups' router ports in one NB transaction and
//     restore those of members that left;
//  5. ask the added and removed members (all of a group's members if its
//     settings changed) to converge, and report their state.
func applyAttachChange(ctx context.Context, s microTypes.State, call attachCall) microTypes.Response {
	stored, err := bgp.LoadAttachState(ctx, s)
	if err != nil {
		return microTypes.InternalError(err)
	}
	preview, err := bgp.PreviewAttachChange(stored, call.change)
	if err != nil {
		return microTypes.BadRequest(err)
	}
	ids, err := bgp.ClusterMemberIDs(ctx, s)
	if err != nil {
		return microTypes.InternalError(err)
	}
	chassis, err := bgp.SwitchMembers(ctx, s)
	if err != nil {
		return microTypes.InternalError(err)
	}

	previewState := bgp.AttachState{Groups: slices.Clone(stored.Groups), Labels: stored.Labels}
	for _, c := range preview {
		if c.After != nil {
			previewState.SetGroup(*c.After)
		} else {
			previewState.RemoveGroup(c.Key)
		}
	}
	focusKey := ""
	if call.focus != nil {
		focusKey = call.focus(previewState)
	}

	// Find the uplinks that are not recorded yet: new members, or every member
	// of a group whose settings changed (the pattern may pick another port).
	var plans []types.BgpAttachNodePlan
	for _, c := range preview {
		if c.After == nil {
			continue
		}
		var discover []string
		for _, m := range c.After.Members {
			if _, ok := c.After.Ports[m]; !ok || c.SettingsChanged() {
				discover = append(discover, m)
			}
		}
		found := 0
		for _, p := range planMembers(ctx, s, *c.After, discover, ids, chassis) {
			plans = append(plans, p)
			if p.RouterPort == "" {
				continue
			}
			found++
			if err := bgp.CheckAttachAdvertiseTable(ctx, s, *c.After, p.RouterPort); err != nil {
				return microTypes.BadRequest(err)
			}
		}
		if call.requireUplink && c.Key == focusKey && len(discover) > 0 && found == 0 {
			return microTypes.BadRequest(fmt.Errorf("--uplink-pattern %q matches no router port on the members labelled %s", c.After.UplinkPattern, c.After.Rack))
		}
	}

	if call.dryRun {
		resp := types.SetupBgpAttachResponse{}
		for _, c := range preview {
			resp.Added = append(resp.Added, c.Added()...)
			resp.Removed = append(resp.Removed, c.Removed()...)
		}
		if g := bgp.FindGroup(previewState.Groups, focusKey); g != nil {
			resp.Groups = []types.BgpAttachGroup{g.Group()}
			resp.Nodes = planGroup(ctx, s, *g, ids, chassis)
		}
		sort.Strings(resp.Added)
		sort.Strings(resp.Removed)
		summariseWarnings(&resp, "dry run: nothing was written")
		return respond(resp)
	}

	// Record each newly found uplink's current values before the first write,
	// so they can be put back when the member leaves.
	newPorts := map[string]bgp.PortRecord{}
	for _, p := range plans {
		if p.RouterPort == "" {
			continue
		}
		rec, err := bgp.ReadPortPrior(ctx, s, p.RouterPort)
		if err != nil {
			return microTypes.InternalError(err)
		}
		newPorts[p.Member] = rec
	}
	fix := func(g *bgp.AttachIntent) []bgp.PortRecord {
		var restore []bgp.PortRecord
		for m, rec := range newPorts {
			if !g.HasMember(m) {
				continue
			}
			if old, ok := g.Ports[m]; ok {
				if old.RouterPort == rec.RouterPort {
					continue
				}
				// The member's uplink changed: give the old one its values back.
				restore = append(restore, old)
			}
			ports := maps.Clone(g.Ports)
			if ports == nil {
				ports = map[string]bgp.PortRecord{}
			}
			ports[m] = rec
			g.Ports = ports
		}
		return restore
	}

	attachWriteMu.Lock()
	changes, toRestore, err := bgp.UpdateAttachState(ctx, s, call.change, fix)
	attachWriteMu.Unlock()
	if err != nil {
		return microTypes.BadRequest(err)
	}
	final, err := bgp.LoadAttachState(ctx, s)
	if err != nil {
		return microTypes.InternalError(err)
	}
	focus := bgp.FindGroup(final.Groups, focusKey)

	// Give the uplinks of members that left their values back first, then
	// write the router-port keys centrally, one NB transaction per group
	// (setting a value it already has changes nothing, and a key removed by
	// hand is put back). The computes then set their own OVS knobs.
	affected := map[string]bool{}
	var added, removed []string
	var nbErrors []string
	if err := bgp.RestorePorts(ctx, s, toRestore); err != nil {
		nbErrors = append(nbErrors, err.Error())
	}
	written := map[string]bool{}
	for _, c := range changes {
		added = append(added, c.Added()...)
		removed = append(removed, c.Removed()...)
		for _, m := range append(c.Added(), c.Removed()...) {
			affected[m] = true
		}
		if c.After == nil {
			continue
		}
		if c.SettingsChanged() {
			for _, m := range c.After.Members {
				affected[m] = true
			}
		}
		logger.Infof("bgp-attach group %s (rack %s) stored: networks %q, members %s (added %v, removed %v)",
			c.Key, c.After.Rack, c.After.Request().Networks, strings.Join(c.After.Members, ","), c.Added(), c.Removed())
		if err := bgp.WriteGroupPorts(ctx, s, *c.After, c.After.Members); err != nil {
			nbErrors = append(nbErrors, err.Error())
		}
		written[c.Key] = true
	}
	if focus != nil && !written[focus.Key()] {
		if err := bgp.WriteGroupPorts(ctx, s, *focus, focus.Members); err != nil {
			nbErrors = append(nbErrors, err.Error())
		}
	}
	for _, m := range call.alsoConverge {
		affected[m] = true
	}

	fallback := map[string]types.BgpAttachNodePlan{}
	for _, p := range plans {
		fallback[p.Member] = p
	}
	reported, unreached, err := convergeMembers(ctx, s, affected, call.disable)
	if err != nil {
		return microTypes.SmartError(err)
	}

	sort.Strings(added)
	sort.Strings(removed)
	resp := types.SetupBgpAttachResponse{Added: added, Removed: removed}
	reportedGroups := map[string]bool{}
	for _, c := range changes {
		if c.After != nil {
			resp.Groups = append(resp.Groups, c.After.Group())
		} else {
			resp.Groups = append(resp.Groups, c.Before.Group())
		}
		reportedGroups[c.Key] = true
	}
	if focus != nil && !reportedGroups[focus.Key()] {
		resp.Groups = append(resp.Groups, focus.Group())
	}
	resp.Nodes = collectReports(affected, reported, unreached, fallback)
	for i := range resp.Nodes {
		if resp.Nodes[i].Rack == "" {
			resp.Nodes[i].Rack = final.Labels[resp.Nodes[i].Member]
		}
	}
	summariseWarnings(&resp, call.message(changes, focus))
	if focus != nil && len(focus.Members) == 0 && focus.Rack != "" {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("no member is labelled %s yet; members join as they are labelled ('microovn setup bgp-attach label --rack %s')", focus.Rack, focus.Rack))
		resp.Message = fmt.Sprintf("%s, with %d warning(s)", call.message(changes, focus), len(resp.Warnings))
	}
	addNbErrors(&resp, nbErrors)
	return respond(resp)
}

// addNbErrors appends central NB write errors to a response (the setting is
// stored; rerunning the call writes the keys again).
func addNbErrors(resp *types.SetupBgpAttachResponse, errs []string) {
	if len(errs) == 0 {
		return
	}
	for _, e := range errs {
		resp.Errors = append(resp.Errors, "router ports: "+e)
	}
	resp.Success = false
	resp.Message += "; router-port keys were not fully written, rerun the call"
}

// groupForRack returns the key of the group that uses rack, or "".
func groupForRack(groups []bgp.AttachIntent, rack string) string {
	for _, g := range groups {
		if rack != "" && g.Rack == rack {
			return g.Key()
		}
	}
	return ""
}

func postBgpAttachHandler(s microTypes.State, r *http.Request) microTypes.Response {
	ctx := r.Context()

	if microTypes.IsNotification(r) {
		resp := types.SetupBgpAttachResponse{}
		if plan, ok := localConverge(ctx, s); ok {
			resp.Nodes = []types.BgpAttachNodePlan{plan}
		}
		summarise(&resp, "converged")
		return respond(resp)
	}

	var req types.SetupBgpAttachRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return microTypes.BadRequest(errors.New("failed to decode request body"))
	}
	group, err := bgp.ParseAttachIntent(req)
	if err != nil {
		return microTypes.BadRequest(err)
	}

	// Mutual exclusion with managed `setup bgp`.
	if item, err := config.GetConfig(ctx, s, types.BgpConfigKeyCluster); err != nil {
		return microTypes.InternalError(err)
	} else if item != nil && item.Value == "true" {
		return microTypes.BadRequest(errors.New("managed BGP ('microovn setup bgp') is configured; disable it before using bgp-attach"))
	}

	return applyAttachChange(ctx, s, attachCall{
		change: func(st *bgp.AttachState) error {
			st.SetGroup(group)
			return nil
		},
		focus:         func(bgp.AttachState) string { return group.Key() },
		requireUplink: true,
		dryRun:        req.DryRun,
		message: func(changes []bgp.GroupChange, focus *bgp.AttachIntent) string {
			msg := fmt.Sprintf("bgp-attach group %s applied", group.Key())
			if focus != nil {
				msg = fmt.Sprintf("bgp-attach group %s (rack %s) applied (%d member(s))", focus.Key(), focus.Rack, len(focus.Members))
			}
			if len(changes) == 0 {
				msg += "; no change"
			}
			return msg
		},
	})
}

// BgpAttachRackEndpoint backs `microovn setup bgp-attach label`.
var BgpAttachRackEndpoint = microTypes.Endpoint{
	Path: "bgp/attach/rack",
	Post: microTypes.EndpointAction{Handler: postBgpAttachRackHandler, AllowUntrusted: false, ProxyTarget: true},
}

func postBgpAttachRackHandler(s microTypes.State, r *http.Request) microTypes.Response {
	ctx := r.Context()

	var req types.SetBgpAttachRackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return microTypes.BadRequest(errors.New("failed to decode request body"))
	}
	member := strings.TrimSpace(req.Member)
	if member == "" {
		member = s.Name()
	}
	rack := strings.TrimSpace(req.Rack)
	if req.Clear {
		rack = ""
	} else if err := bgp.ValidateRackName(rack); err != nil {
		return microTypes.BadRequest(fmt.Errorf("--rack: %w", err))
	}

	ids, err := bgp.ClusterMemberIDs(ctx, s)
	if err != nil {
		return microTypes.InternalError(err)
	}
	if _, ok := ids[member]; !ok {
		return microTypes.BadRequest(fmt.Errorf("member %s is not a cluster member (see 'microovn cluster list')", member))
	}
	if !req.Clear {
		chassis, err := bgp.SwitchMembers(ctx, s)
		if err != nil {
			return microTypes.InternalError(err)
		}
		if !chassis[member] {
			return microTypes.BadRequest(fmt.Errorf("member %s does not run the switch service, so it cannot attach", member))
		}
	}

	return applyAttachChange(ctx, s, attachCall{
		change: func(st *bgp.AttachState) error {
			if rack == "" {
				delete(st.Labels, member)
			} else {
				st.Labels[member] = rack
			}
			return nil
		},
		focus:        func(st bgp.AttachState) string { return groupForRack(st.Groups, rack) },
		alsoConverge: []string{member},
		message: func(_ []bgp.GroupChange, focus *bgp.AttachIntent) string {
			switch {
			case rack == "":
				return fmt.Sprintf("rack label of %s cleared", member)
			case focus == nil:
				return fmt.Sprintf("%s labelled %s; no bgp-attach group uses rack %s yet", member, rack, rack)
			default:
				return fmt.Sprintf("%s labelled %s (group %s)", member, rack, focus.Key())
			}
		},
	})
}

func deleteBgpAttachHandler(s microTypes.State, r *http.Request) microTypes.Response {
	ctx := r.Context()

	if microTypes.IsNotification(r) {
		resp := types.SetupBgpAttachResponse{}
		if plan, ok := localConverge(ctx, s); ok {
			resp.Nodes = []types.BgpAttachNodePlan{plan}
		}
		summarise(&resp, "converged")
		return respond(resp)
	}

	key := ""
	if peer := strings.TrimSpace(r.URL.Query().Get("peer")); peer != "" {
		var err error
		if key, err = bgp.GroupKeyFromPeer(peer); err != nil {
			return microTypes.BadRequest(err)
		}
	}

	stored, err := bgp.LoadAttachState(ctx, s)
	if err != nil {
		return microTypes.InternalError(err)
	}
	if len(stored.Groups) == 0 {
		return respond(types.SetupBgpAttachResponse{Success: true, Message: "bgp-attach is not configured; nothing to disable"})
	}
	if key != "" && bgp.FindGroup(stored.Groups, key) == nil {
		return microTypes.BadRequest(fmt.Errorf("no bgp-attach group for peer %s", key))
	}

	// Rack labels stay: they describe where the members are, not BGP, so
	// setting the group again brings the same members back.
	return applyAttachChange(ctx, s, attachCall{
		change: func(st *bgp.AttachState) error {
			if key == "" {
				st.Groups = nil
				return nil
			}
			if bgp.FindGroup(st.Groups, key) == nil {
				return fmt.Errorf("no bgp-attach group for peer %s", key)
			}
			st.RemoveGroup(key)
			return nil
		},
		disable: true,
		message: func(changes []bgp.GroupChange, _ *bgp.AttachIntent) string {
			var keys []string
			for _, c := range changes {
				keys = append(keys, c.Key)
			}
			msg := fmt.Sprintf("bgp-attach disabled for %s", strings.Join(keys, ", "))
			logger.Infof("bgp-attach: %s", msg)
			return msg
		},
	})
}

func getBgpAttachHandler(s microTypes.State, r *http.Request) microTypes.Response {
	ctx := r.Context()
	st, err := bgp.LoadAttachState(ctx, s)
	if err != nil {
		return microTypes.InternalError(err)
	}
	if len(st.Groups) == 0 && len(st.Labels) == 0 {
		return respond(types.SetupBgpAttachResponse{Success: true, Message: "bgp-attach is not configured"})
	}

	ids, err := bgp.ClusterMemberIDs(ctx, s)
	if err != nil {
		return microTypes.InternalError(err)
	}
	chassis, err := bgp.SwitchMembers(ctx, s)
	if err != nil {
		return microTypes.InternalError(err)
	}

	resp := types.SetupBgpAttachResponse{}
	for _, g := range st.Groups {
		resp.Groups = append(resp.Groups, g.Group())
		resp.Nodes = append(resp.Nodes, planGroup(ctx, s, g, ids, chassis)...)
	}
	for i := range resp.Nodes {
		if resp.Nodes[i].Error == "" && !resp.Nodes[i].Attached {
			resp.Nodes[i].Error = "not attached yet"
		}
	}
	resp.Nodes = append(resp.Nodes, unusedLabelRows(st, ids)...)
	sortNodes(resp.Nodes)
	summariseWarnings(&resp, fmt.Sprintf("bgp-attach is configured for %d group(s), %d labelled member(s)", len(st.Groups), len(st.Labels)))
	return respond(resp)
}

// unusedLabelRows reports labelled members whose rack no group uses yet, so
// the operator sees them waiting rather than missing.
func unusedLabelRows(st bgp.AttachState, ids map[string]int64) []types.BgpAttachNodePlan {
	var rows []types.BgpAttachNodePlan
	for m, rack := range st.Labels {
		if groupForRack(st.Groups, rack) != "" {
			continue
		}
		rows = append(rows, types.BgpAttachNodePlan{Member: m, MemberID: ids[m], Rack: rack,
			Error: fmt.Sprintf("labelled %s, but no bgp-attach group uses rack %s yet", rack, rack)})
	}
	return rows
}
