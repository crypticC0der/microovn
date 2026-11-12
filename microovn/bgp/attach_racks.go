package bgp

// Rack labels and the groups they make up.
//
// Each member may carry one rack label (stored in dqlite under
// "microovn-bgp-attach-rack.<member>"). A group's members are the members
// labelled with its rack. Labels change only when someone sets or clears one
// (`microovn setup bgp-attach label`), or when a member leaves the cluster; a
// label that is merely late never removes anyone.
//
// Whenever a label or a group changes, every group is resolved again from the
// labels, in the same database transaction as the change:
//
//	members   = members labelled with the group's rack
//	addresses = kept for members that stay, lowest free for new ones
//	ports     = kept for members that stay; leavers' ports are restored
//
// So a label moved from rack1 to rack2 takes the member out of rack1's group
// and into rack2's in one step.

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/canonical/lxd/shared/logger"
	microTypes "github.com/canonical/microcluster/v3/microcluster/types"

	"github.com/canonical/microovn/microovn/api/types"
	"github.com/canonical/microovn/microovn/database"
)

// rackNameRe is what a rack name may look like: what Sunbeam and MAAS use for
// names (letters, digits, '.', '_', ':', '-'), starting with a letter or digit.
var rackNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// ValidateRackName checks a rack name. Names are compared exactly, so
// "Rack1" and "rack1" are different racks.
func ValidateRackName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("a rack name is required (e.g. rack1)")
	}
	if !rackNameRe.MatchString(name) {
		return fmt.Errorf("invalid rack name %q: use up to 128 letters, digits, '.', '_', ':' or '-', starting with a letter or digit", name)
	}
	return nil
}

// AttachState is everything bgp-attach stores: the groups and the rack labels
// (member -> rack).
type AttachState struct {
	Groups []AttachIntent
	Labels map[string]string
}

// clone copies the state so a change can be tried without touching the
// original.
func (st AttachState) clone() AttachState {
	out := AttachState{Groups: make([]AttachIntent, len(st.Groups)), Labels: maps.Clone(st.Labels)}
	copy(out.Groups, st.Groups)
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}
	return out
}

// SetGroup adds a group, or replaces the stored group with the same key.
func (st *AttachState) SetGroup(group AttachIntent) {
	for i := range st.Groups {
		if st.Groups[i].Key() == group.Key() {
			st.Groups[i] = group
			return
		}
	}
	st.Groups = append(st.Groups, group)
	sort.Slice(st.Groups, func(a, b int) bool { return st.Groups[a].Key() < st.Groups[b].Key() })
}

// RemoveGroup drops the group with the given key, if present.
func (st *AttachState) RemoveGroup(key string) {
	st.Groups = slices.DeleteFunc(st.Groups, func(g AttachIntent) bool { return g.Key() == key })
}

// decodeLabels picks the rack labels out of the config table's rows.
func decodeLabels(items []database.ConfigItem) map[string]string {
	labels := map[string]string{}
	for _, item := range items {
		member, ok := strings.CutPrefix(item.Key, types.BgpAttachRackKeyPrefix)
		if !ok || member == "" {
			continue
		}
		labels[member] = item.Value
	}
	return labels
}

// RackMembers lists the members labelled with rack, sorted.
func RackMembers(labels map[string]string, rack string) []string {
	var out []string
	for m, r := range labels {
		if r == rack {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// GroupChange is how one group changed: Before is the stored version (nil if
// new), After the new one (nil if removed).
type GroupChange struct {
	Key    string
	Before *AttachIntent
	After  *AttachIntent
}

// Added and Removed list the members the change adds and removes, sorted.
func (c GroupChange) Added() []string {
	if c.After == nil {
		return nil
	}
	added, _ := MembershipChange(c.Before, *c.After)
	return added
}

func (c GroupChange) Removed() []string {
	if c.After == nil {
		if c.Before == nil {
			return nil
		}
		return slices.Clone(c.Before.Members)
	}
	_, removed := MembershipChange(c.Before, *c.After)
	return removed
}

// SettingsChanged reports whether more than the members changed (so every
// member must converge, not only the added and removed ones).
func (c GroupChange) SettingsChanged() bool {
	return c.Before == nil || c.After == nil || !SameSettings(*c.Before, *c.After)
}

// PortFix adjusts a resolved group before it is written (it records newly
// discovered uplinks) and returns router ports whose values must be restored.
// It must only compute.
type PortFix func(group *AttachIntent) []PortRecord

// ResolveGroups resolves every group in next from next's labels, against the
// stored groups in before (whose addresses and ports are kept for members
// that stay), and returns the groups to write, how each one changed and the
// router ports to restore (members that left, uplinks replaced by fix).
// Groups present in before but not in next are removed. Groups that come out
// unchanged are left out. It only computes, so it is safe inside a retried
// transaction. fix may be nil.
func ResolveGroups(before []AttachIntent, next AttachState, fix PortFix) (map[string]*AttachIntent, []GroupChange, []PortRecord, error) {
	stored := map[string]*AttachIntent{}
	for i := range before {
		g := before[i]
		stored[g.Key()] = &g
	}

	var resolved []AttachIntent
	var restore []PortRecord
	for _, g := range next.Groups {
		incoming := g
		if incoming.Rack != "" {
			incoming.Members = RackMembers(next.Labels, incoming.Rack)
		}
		merged, err := MergeGroup(stored[g.Key()], incoming)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("group %s: %w", g.Key(), err)
		}
		carryMovedPorts(&merged, before)
		if fix != nil {
			restore = append(restore, fix(&merged)...)
		}
		resolved = append(resolved, merged)
	}
	for _, g := range resolved {
		if err := CheckGroupConflicts(g, resolved); err != nil {
			return nil, nil, nil, err
		}
	}

	writes := map[string]*AttachIntent{}
	var changes []GroupChange
	seen := map[string]bool{}
	for i := range resolved {
		g := resolved[i]
		seen[g.Key()] = true
		prev := stored[g.Key()]
		restore = append(restore, DroppedPorts(prev, &g)...)
		if prev != nil && prev.Equal(g) {
			continue
		}
		writes[g.Key()] = &g
		changes = append(changes, GroupChange{Key: g.Key(), Before: prev, After: &g})
	}
	for key, prev := range stored {
		if seen[key] {
			continue
		}
		writes[key] = nil
		restore = append(restore, DroppedPorts(prev, nil)...)
		changes = append(changes, GroupChange{Key: key, Before: prev})
	}
	sort.Slice(changes, func(a, b int) bool { return changes[a].Key < changes[b].Key })
	return writes, changes, keepInUse(restore, resolved), nil
}

// carryMovedPorts gives a member that moved in from another group the uplink
// record it had there. The record holds the port's values from before any
// attach, which is what must come back when the member finally leaves; reading
// the port now would record the old group's attach values instead.
func carryMovedPorts(group *AttachIntent, before []AttachIntent) {
	for _, m := range group.Members {
		if _, ok := group.Ports[m]; ok {
			continue
		}
		for _, other := range before {
			if other.Key() == group.Key() {
				continue
			}
			rec, ok := other.Ports[m]
			if !ok {
				continue
			}
			ports := maps.Clone(group.Ports)
			if ports == nil {
				ports = map[string]PortRecord{}
			}
			ports[m] = rec
			group.Ports = ports
			break
		}
	}
}

// keepInUse drops, from the ports to restore, those still used by a group (a
// member that moved between groups keeps its uplink): restoring them would
// undo the new group's keys.
func keepInUse(restore []PortRecord, groups []AttachIntent) []PortRecord {
	inUse := map[string]bool{}
	for _, g := range groups {
		for _, rec := range g.Ports {
			inUse[rec.RouterPort] = true
		}
	}
	var out []PortRecord
	seen := map[string]bool{}
	for _, rec := range restore {
		if rec.RouterPort == "" || inUse[rec.RouterPort] || seen[rec.RouterPort] {
			continue
		}
		seen[rec.RouterPort] = true
		out = append(out, rec)
	}
	return out
}

// readState reads the groups and labels inside a transaction.
func readState(ctx context.Context, tx *sql.Tx) (AttachState, error) {
	items, err := database.GetConfigItems(ctx, tx)
	if err != nil {
		return AttachState{}, err
	}
	groups, err := decodeGroups(items)
	if err != nil {
		return AttachState{}, err
	}
	return AttachState{Groups: groups, Labels: decodeLabels(items)}, nil
}

// LoadAttachState reads every group and rack label from dqlite.
func LoadAttachState(ctx context.Context, s microTypes.State) (AttachState, error) {
	var st AttachState
	err := s.Database().Transaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		st, err = readState(ctx, tx)
		return err
	})
	if err != nil {
		return AttachState{}, fmt.Errorf("failed to read bgp-attach state: %w", err)
	}
	return st, nil
}

// PreviewAttachChange applies change to a copy of the stored state and
// resolves the groups, without writing anything. Callers use it to find the
// members whose uplinks must be discovered before the real write.
func PreviewAttachChange(stored AttachState, change func(st *AttachState) error) ([]GroupChange, error) {
	next := stored.clone()
	if err := change(&next); err != nil {
		return nil, err
	}
	_, changes, _, err := ResolveGroups(stored.Groups, next, nil)
	return changes, err
}

// UpdateAttachState applies change to the stored state and writes back the
// labels and every group that changed as a result, all in one database
// transaction. Concurrent updates therefore never lose each other's members or
// hand out the same address twice: a transaction that races another is
// retried by the database layer, re-running change and fix on the fresh
// state, so both must only compute (no side effects). It returns how each
// group changed and the router ports to restore.
func UpdateAttachState(ctx context.Context, s microTypes.State, change func(st *AttachState) error, fix PortFix) ([]GroupChange, []PortRecord, error) {
	var changes []GroupChange
	var restore []PortRecord
	err := s.Database().Transaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		changes, restore = nil, nil
		stored, err := readState(ctx, tx)
		if err != nil {
			return err
		}
		next := stored.clone()
		if err := change(&next); err != nil {
			return err
		}
		writes, resolved, toRestore, err := ResolveGroups(stored.Groups, next, fix)
		if err != nil {
			return err
		}
		if err := writeLabels(ctx, tx, stored.Labels, next.Labels); err != nil {
			return err
		}
		if err := writeGroups(ctx, tx, writes); err != nil {
			return err
		}
		changes, restore = resolved, toRestore
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to update bgp-attach state: %w", err)
	}
	return changes, restore, nil
}

// writeLabels writes the labels that differ between before and after.
func writeLabels(ctx context.Context, tx *sql.Tx, before, after map[string]string) error {
	for member, rack := range after {
		if before[member] == rack {
			continue
		}
		key := types.BgpAttachRackKeyPrefix + member
		item := database.ConfigItem{Key: key, Value: rack}
		var err error
		if _, ok := before[member]; ok {
			err = database.UpdateConfigItem(ctx, tx, key, item)
		} else {
			_, err = database.CreateConfigItem(ctx, tx, item)
		}
		if err != nil {
			return err
		}
	}
	for member := range before {
		if _, ok := after[member]; ok {
			continue
		}
		if err := database.DeleteConfigItem(ctx, tx, types.BgpAttachRackKeyPrefix+member); err != nil {
			return err
		}
	}
	return nil
}

// writeGroups writes (or, for nil, deletes) the given groups.
func writeGroups(ctx context.Context, tx *sql.Tx, writes map[string]*AttachIntent) error {
	for key, group := range writes {
		dbKey := types.BgpAttachGroupKeyPrefix + key
		exists, err := database.ConfigItemExists(ctx, tx, dbKey)
		if err != nil {
			return err
		}
		if group == nil {
			if exists {
				if err := database.DeleteConfigItem(ctx, tx, dbKey); err != nil {
					return err
				}
			}
			continue
		}
		value, err := encodeGroup(*group)
		if err != nil {
			return err
		}
		item := database.ConfigItem{Key: dbKey, Value: value}
		if exists {
			err = database.UpdateConfigItem(ctx, tx, dbKey, item)
		} else {
			_, err = database.CreateConfigItem(ctx, tx, item)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// departedLabels lists labelled members that are no longer cluster members.
func departedLabels(labels map[string]string, ids map[string]int64) []string {
	var out []string
	for m := range labels {
		if _, ok := ids[m]; !ok {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

// PruneDepartedMembers drops the labels of members that have left the
// cluster, which takes them out of their groups, frees their addresses and
// restores their router ports. It runs after a member is removed and on every
// reconcile tick as a backstop; when nothing has departed it only reads.
func PruneDepartedMembers(ctx context.Context, s microTypes.State) error {
	st, err := LoadAttachState(ctx, s)
	if err != nil {
		return err
	}
	if len(st.Labels) == 0 {
		return nil
	}
	ids, err := clusterMemberIDs(ctx, s)
	if err != nil {
		return err
	}
	// Never prune against an empty member list: that is a read gone wrong,
	// not a cluster with no members.
	if len(ids) == 0 || len(departedLabels(st.Labels, ids)) == 0 {
		return nil
	}

	var pruned []string
	changes, restore, err := UpdateAttachState(ctx, s, func(next *AttachState) error {
		pruned = departedLabels(next.Labels, ids)
		for _, m := range pruned {
			delete(next.Labels, m)
		}
		return nil
	}, nil)
	if err != nil {
		return err
	}
	if len(pruned) > 0 {
		logger.Infof("bgp-attach: dropped the rack labels of departed member(s) %s", strings.Join(pruned, ", "))
	}
	if err := RestorePorts(ctx, s, restore); err != nil {
		return err
	}
	for _, c := range changes {
		if c.After == nil {
			continue
		}
		// Members that stay are unchanged; rewrite them anyway in case an
		// earlier write was lost (it changes nothing when already set).
		if err := WriteGroupPorts(ctx, s, *c.After, c.After.Members); err != nil {
			logger.Warnf("bgp-attach: %v", err)
		}
	}
	return nil
}
