package bgp

import (
	"sort"
	"strings"
	"testing"

	"github.com/canonical/microovn/microovn/api/types"
)

// mustGroup builds a group with the given members already resolved. Its rack
// is named after the peer's ASN, so groups with different ASNs are different
// racks.
func mustGroup(t *testing.T, members []string, peer, networks string) AttachIntent {
	t.Helper()
	asn, _, _ := strings.Cut(strings.TrimSpace(peer), "@")
	g, err := ParseAttachIntent(types.SetupBgpAttachRequest{Rack: "rack-" + asn, Peer: peer, Networks: networks})
	if err != nil {
		t.Fatalf("group %s: %v", peer, err)
	}
	g.Members = append([]string(nil), members...)
	sort.Strings(g.Members)
	return g
}

func TestGroupForMemberAndFindGroup(t *testing.T) {
	groups := []AttachIntent{
		mustGroup(t, []string{"c0", "c1"}, "65101@192.0.2.17", "192.0.2.18/28"),
		mustGroup(t, []string{"c2"}, "65102@192.0.2.33", "192.0.2.34/28"),
	}
	if g := GroupForMember(groups, "c1"); g == nil || g.Key() != "65101@192.0.2.17" {
		t.Fatalf("c1 -> %v", g)
	}
	if g := GroupForMember(groups, "c9"); g != nil {
		t.Fatalf("c9 -> %v, want none", g)
	}
	if g := FindGroup(groups, "65102@192.0.2.33"); g == nil || g.Members[0] != "c2" {
		t.Fatalf("FindGroup -> %v", g)
	}
}

func TestCheckGroupConflicts(t *testing.T) {
	existing := []AttachIntent{mustGroup(t, []string{"c0", "c1"}, "65101@192.0.2.17", "192.0.2.18/28")}

	cases := []struct {
		name    string
		group   AttachIntent
		wantErr string
	}{
		{"separate leaf", mustGroup(t, []string{"c2"}, "65102@192.0.2.33", "192.0.2.34/28"), ""},
		{"replacing the same peer", mustGroup(t, []string{"c0"}, "65101@192.0.2.17", "192.0.2.18/28"), ""},
		{"member with another leaf", mustGroup(t, []string{"c1", "c2"}, "65102@192.0.2.33", "192.0.2.34/28"), "member c1 already peers with 65101@192.0.2.17"},
		{"same leaf address, other ASN", mustGroup(t, []string{"c2"}, "65102@192.0.2.17", "192.0.2.20/28"), "leaf address 192.0.2.17 is already used"},
		{"overlapping subnet", mustGroup(t, []string{"c2"}, "65102@192.0.2.21", "192.0.2.22/28"), "overlaps the subnet 192.0.2.16/28"},
		{"unnumbered next to numbered", mustGroup(t, []string{"c2"}, "65102@fe80::1", ""), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckGroupConflicts(c.group, existing)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, c.wantErr)
			}
		})
	}
}

func TestUnnumberedGroupsWithDifferentASNs(t *testing.T) {
	// Unnumbered leaves all answer on fe80::1; their ASNs tell the groups apart.
	a := mustGroup(t, []string{"c0"}, "65101@fe80::1", "")
	b := mustGroup(t, []string{"c1"}, "65102@fe80::1", "")
	if a.Key() == b.Key() {
		t.Fatal("different ASNs must give different groups")
	}
	if err := CheckGroupConflicts(b, []AttachIntent{a}); err != nil {
		t.Fatalf("unnumbered groups: %v", err)
	}
}

func TestGroupKeyFromPeer(t *testing.T) {
	key, err := GroupKeyFromPeer(" 65101@192.0.2.17 ")
	if err != nil || key != "65101@192.0.2.17" {
		t.Fatalf("got %q, %v", key, err)
	}
	if _, err := GroupKeyFromPeer("192.0.2.17"); err == nil {
		t.Fatal("a peer without an ASN must be rejected")
	}
}

func addrOf(g AttachIntent, m string) string { return g.Addresses[m].String() }

func mustMerge(t *testing.T, existing *AttachIntent, members []string, networks string) AttachIntent {
	t.Helper()
	g, err := MergeGroup(existing, mustGroup(t, members, "65101@192.0.2.17", networks))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// The senior's example: stored c0,c1,c2; Sunbeam sends c0,c2,c3.
func TestMergeGroupSyncsToTheList(t *testing.T) {
	stored := mustMerge(t, nil, []string{"c0", "c1", "c2"}, "192.0.2.18/28") // .18 .19 .20
	got := mustMerge(t, &stored, []string{"c0", "c2", "c3"}, "192.0.2.18/28")

	if strings.Join(got.Members, ",") != "c0,c2,c3" {
		t.Fatalf("members %v, want exactly the list sent", got.Members)
	}
	// Members that stay keep their address.
	if addrOf(got, "c0") != "192.0.2.18/28" || addrOf(got, "c2") != "192.0.2.20/28" {
		t.Fatalf("c0/c2 moved: %v", got.Addresses)
	}
	// c1 left, so its .19 is free and goes to c3.
	if addrOf(got, "c3") != "192.0.2.19/28" {
		t.Fatalf("c3 got %s, want the freed 192.0.2.19/28", addrOf(got, "c3"))
	}
	if _, ok := got.Addresses["c1"]; ok {
		t.Fatal("c1 was not listed and must be gone")
	}

	added, removed := MembershipChange(&stored, got)
	if strings.Join(added, ",") != "c3" || strings.Join(removed, ",") != "c1" {
		t.Fatalf("added %v removed %v", added, removed)
	}
}

func TestMergeGroupRepeatChangesNothing(t *testing.T) {
	first := mustMerge(t, nil, []string{"c0", "c1"}, "192.0.2.18/28")
	again := mustMerge(t, &first, []string{"c1", "c0"}, "192.0.2.18/28")
	if !again.Equal(first) {
		t.Fatalf("repeating the call changed the group: %+v -> %+v", first, again)
	}
	added, removed := MembershipChange(&first, again)
	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("added %v removed %v, want none", added, removed)
	}
}

func TestMergeGroupKeepsAddressesWhenNodeAddedInTheMiddle(t *testing.T) {
	first := mustMerge(t, nil, []string{"c0", "c2"}, "192.0.2.18/28") // c0=.18 c2=.19
	second := mustMerge(t, &first, []string{"c0", "c1", "c2"}, "192.0.2.18/28")
	if addrOf(second, "c0") != addrOf(first, "c0") || addrOf(second, "c2") != addrOf(first, "c2") {
		t.Fatalf("existing addresses moved: %v -> %v", first.Addresses, second.Addresses)
	}
	if addrOf(second, "c1") != "192.0.2.20/28" {
		t.Fatalf("c1 got %s, want the next free 192.0.2.20/28", addrOf(second, "c1"))
	}
}

func TestMergeGroupNewSettingsApply(t *testing.T) {
	first := mustMerge(t, nil, []string{"c0"}, "192.0.2.18/28")
	got := mustMerge(t, &first, []string{"c0", "c1"}, "192.0.2.24/28")
	if got.Request().Networks != "192.0.2.24/28" || SameSettings(first, got) {
		t.Fatalf("the call's pool must apply: %s", got.Request().Networks)
	}
	// c0's .18 is below the new start, so it moves.
	if addrOf(got, "c0") != "192.0.2.24/28" || addrOf(got, "c1") != "192.0.2.25/28" {
		t.Fatalf("addresses %v", got.Addresses)
	}
}

func TestMergeGroupPoolFull(t *testing.T) {
	// /30 from .1 with the leaf at .1: only .2 is usable (.3 is broadcast).
	if _, err := MergeGroup(nil, mustGroup(t, []string{"c0", "c1"}, "65101@192.0.2.1", "192.0.2.1/30")); err == nil {
		t.Fatal("two members in a one-address pool must fail")
	}
}

func TestMergeGroupKeepsPortRecordsOfMembersThatStay(t *testing.T) {
	g := mustMerge(t, nil, []string{"c0", "c1"}, "192.0.2.18/28")
	g.Ports = map[string]PortRecord{"c0": {RouterPort: "lrp-c0"}, "c1": {RouterPort: "lrp-c1"}}

	got := mustMerge(t, &g, []string{"c0", "c2"}, "192.0.2.18/28")
	if got.Ports["c0"].RouterPort != "lrp-c0" {
		t.Fatalf("c0's port record lost: %v", got.Ports)
	}
	if _, ok := got.Ports["c1"]; ok {
		t.Fatal("c1 left; its record must not be kept in the group")
	}
	dropped := DroppedPorts(&g, &got)
	if len(dropped) != 1 || dropped[0].RouterPort != "lrp-c1" {
		t.Fatalf("c1's port must be restored, got %v", dropped)
	}
}

func TestMembershipChangeFromNothing(t *testing.T) {
	g := mustMerge(t, nil, []string{"c1", "c0"}, "192.0.2.18/28")
	added, removed := MembershipChange(nil, g)
	if strings.Join(added, ",") != "c0,c1" || removed != nil {
		t.Fatalf("added %v removed %v", added, removed)
	}
}

func TestPortRecordsSurviveStorage(t *testing.T) {
	g := mustMerge(t, nil, []string{"c0"}, "192.0.2.18/28")
	g.Ports = map[string]PortRecord{"c0": {RouterPort: "lrp-c0", Prior: lrpPrior{Networks: []string{"192.0.2.3/31"}, Peers: "65101@192.0.2.2"}}}
	value, err := encodeGroup(g)
	if err != nil {
		t.Fatal(err)
	}
	out, err := decodeGroup(types.BgpAttachGroupKeyPrefix+g.Key(), value)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Equal(g) {
		t.Fatalf("round trip changed the ports: %+v vs %+v", out.Ports, g.Ports)
	}
}
