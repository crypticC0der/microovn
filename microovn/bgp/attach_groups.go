package bgp

// Rules that span groups. One MicroOVN cluster covers every rack, so each
// group (one per leaf) must not step on another: a rack has one group, a
// member peers with one leaf, and numbered groups need separate transit
// subnets and leaf addresses.

import (
	"fmt"
	"slices"
	"sort"

	"github.com/canonical/microovn/microovn/api/types"
)

// GroupForMember returns the group that lists member, or nil if none does.
func GroupForMember(groups []AttachIntent, member string) *AttachIntent {
	for i := range groups {
		if groups[i].HasMember(member) {
			return &groups[i]
		}
	}
	return nil
}

// FindGroup returns the group with the given key (peer), or nil.
func FindGroup(groups []AttachIntent, key string) *AttachIntent {
	for i := range groups {
		if groups[i].Key() == key {
			return &groups[i]
		}
	}
	return nil
}

// CheckGroupConflicts checks a group against the other groups. A group with
// the same key (peer) is the one being replaced and is ignored.
func CheckGroupConflicts(group AttachIntent, existing []AttachIntent) error {
	for _, other := range existing {
		if other.Key() == group.Key() {
			continue
		}
		if group.Rack != "" && group.Rack == other.Rack {
			return fmt.Errorf("rack %s already peers with %s; a rack has one leaf (use 'disable --peer %s' first to change it)",
				group.Rack, other.Key(), other.Key())
		}
		for _, m := range group.Members {
			if other.HasMember(m) {
				return fmt.Errorf("member %s already peers with %s", m, other.Key())
			}
		}
		if !group.Numbered() || !other.Numbered() {
			continue
		}
		if group.Peer.Address == other.Peer.Address {
			return fmt.Errorf("leaf address %s is already used by group %s; each rack has its own leaf address", group.Peer.Address, other.Key())
		}
		if group.Pool.Masked().Overlaps(other.Pool.Masked()) {
			return fmt.Errorf("--networks %s overlaps the subnet %s of group %s; each rack needs its own transit subnet",
				group.Pool, other.Pool.Masked(), other.Key())
		}
	}
	return nil
}

// GroupKeyFromPeer normalises a "<asn>@<ip>" peer into a group key, so
// `disable --peer` matches however the address was spelled.
func GroupKeyFromPeer(peer string) (string, error) {
	peers, err := types.ParsePeers(peer)
	if err != nil {
		return "", fmt.Errorf("--peer: %w", err)
	}
	if len(peers) != 1 {
		return "", fmt.Errorf("--peer: exactly one peer is required, as <asn>@<ip>")
	}
	return fmt.Sprintf("%d@%s", peers[0].ASN, peers[0].Address), nil
}

// MergeGroup builds a group from its new settings and members (incoming)
// against its stored version (existing, nil if new). incoming's settings and
// members replace what is stored. Members that stay keep their address and
// their recorded uplink; new members get the lowest free address. A group may
// have no members (no member is labelled with its rack yet).
func MergeGroup(existing *AttachIntent, incoming AttachIntent) (AttachIntent, error) {
	merged := incoming
	merged.Members = slices.Clone(incoming.Members)
	sort.Strings(merged.Members)
	merged.Ports = keptPorts(existing, merged.Members)
	if err := AllocateGroup(&merged, existing); err != nil {
		return merged, err
	}
	return merged, nil
}

// MembershipChange lists the members a call adds and removes, sorted.
func MembershipChange(before *AttachIntent, after AttachIntent) (added []string, removed []string) {
	for _, m := range after.Members {
		if before == nil || !before.HasMember(m) {
			added = append(added, m)
		}
	}
	if before != nil {
		for _, m := range before.Members {
			if !after.HasMember(m) {
				removed = append(removed, m)
			}
		}
	}
	return added, removed
}

// keptPorts copies the stored port records of the members that remain.
func keptPorts(existing *AttachIntent, members []string) map[string]PortRecord {
	if existing == nil {
		return nil
	}
	var out map[string]PortRecord
	for _, m := range members {
		if rec, ok := existing.Ports[m]; ok {
			if out == nil {
				out = map[string]PortRecord{}
			}
			out[m] = rec
		}
	}
	return out
}

// DroppedPorts returns the port records of members that were in before but
// are not in after: their router ports must get their pre-attach values back.
func DroppedPorts(before *AttachIntent, after *AttachIntent) []PortRecord {
	if before == nil {
		return nil
	}
	var out []PortRecord
	for _, m := range before.Members {
		if after != nil && after.HasMember(m) {
			continue
		}
		if rec, ok := before.Ports[m]; ok {
			out = append(out, rec)
		}
	}
	return out
}

// SameSettings reports whether two versions of a group differ only in members
// (and so only added/removed members need to converge).
func SameSettings(a, b AttachIntent) bool {
	return a.Peer == b.Peer && a.Pool == b.Pool && a.AdvertiseTable == b.AdvertiseTable && a.UplinkPattern == b.UplinkPattern
}
