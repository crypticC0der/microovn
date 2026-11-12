package bgp

import (
	"strings"
	"testing"

	"github.com/canonical/microovn/microovn/api/types"
	"github.com/canonical/microovn/microovn/database"
)

// rackGroup builds an unresolved group for rack (no members yet), as a
// `setup bgp-attach --rack` call does.
func rackGroup(t *testing.T, rack, peer, networks string) AttachIntent {
	t.Helper()
	g, err := ParseAttachIntent(types.SetupBgpAttachRequest{Rack: rack, Peer: peer, Networks: networks})
	if err != nil {
		t.Fatalf("group %s: %v", peer, err)
	}
	return g
}

// resolve applies change to stored and returns the stored state afterwards
// plus the changes, the way UpdateAttachState does inside its transaction.
func resolve(t *testing.T, stored AttachState, fix PortFix, change func(st *AttachState)) (AttachState, []GroupChange, []PortRecord) {
	t.Helper()
	next := stored.clone()
	change(&next)
	writes, changes, restore, err := ResolveGroups(stored.Groups, next, fix)
	if err != nil {
		t.Fatal(err)
	}
	out := AttachState{Labels: next.Labels}
	for _, g := range stored.Groups {
		if w, ok := writes[g.Key()]; ok {
			if w != nil {
				out.Groups = append(out.Groups, *w)
			}
			continue
		}
		out.Groups = append(out.Groups, g)
	}
	for key, w := range writes {
		if w != nil && FindGroup(stored.Groups, key) == nil {
			out.SetGroup(*w)
		}
	}
	return out, changes, restore
}

func rack1rack2(t *testing.T) AttachState {
	st := AttachState{Labels: map[string]string{"c0": "rack1", "c1": "rack1", "c2": "rack2"}}
	st, _, _ = resolve(t, st, nil, func(n *AttachState) {
		n.SetGroup(rackGroup(t, "rack1", "65101@192.0.2.17", "192.0.2.18/28"))
		n.SetGroup(rackGroup(t, "rack2", "65102@192.0.2.33", "192.0.2.34/28"))
	})
	return st
}

func TestValidateRackName(t *testing.T) {
	for _, ok := range []string{"rack1", "R-01", "stg-reproducer-hassahma-maas", "dc1:row2.rack_3"} {
		if err := ValidateRackName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", " ", "rack 1", "-rack", "rack/1", strings.Repeat("a", 129)} {
		if err := ValidateRackName(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestGroupMembersComeFromLabels(t *testing.T) {
	st := rack1rack2(t)
	g1 := FindGroup(st.Groups, "65101@192.0.2.17")
	g2 := FindGroup(st.Groups, "65102@192.0.2.33")
	if strings.Join(g1.Members, ",") != "c0,c1" || strings.Join(g2.Members, ",") != "c2" {
		t.Fatalf("rack1 %v rack2 %v", g1.Members, g2.Members)
	}
	if addrOf(*g1, "c0") != "192.0.2.18/28" || addrOf(*g1, "c1") != "192.0.2.19/28" || addrOf(*g2, "c2") != "192.0.2.34/28" {
		t.Fatalf("addresses %v %v", g1.Addresses, g2.Addresses)
	}
}

func TestLabellingAMemberAddsItAndKeepsOthers(t *testing.T) {
	st := rack1rack2(t)
	after, changes, _ := resolve(t, st, nil, func(n *AttachState) { n.Labels["c3"] = "rack1" })
	if len(changes) != 1 || strings.Join(changes[0].Added(), ",") != "c3" || len(changes[0].Removed()) != 0 {
		t.Fatalf("changes %+v", changes)
	}
	if changes[0].SettingsChanged() {
		t.Fatal("only members changed: only c3 must converge")
	}
	g1 := FindGroup(after.Groups, "65101@192.0.2.17")
	if addrOf(*g1, "c0") != "192.0.2.18/28" || addrOf(*g1, "c1") != "192.0.2.19/28" || addrOf(*g1, "c3") != "192.0.2.20/28" {
		t.Fatalf("addresses %v", g1.Addresses)
	}
}

func TestRelabelMovesMemberInOneStep(t *testing.T) {
	st := rack1rack2(t)
	g1 := FindGroup(st.Groups, "65101@192.0.2.17")
	g1.Ports = map[string]PortRecord{"c0": {RouterPort: "lrp-c0"}, "c1": {RouterPort: "lrp-c1"}}

	after, changes, restore := resolve(t, st, nil, func(n *AttachState) { n.Labels["c1"] = "rack2" })
	if len(changes) != 2 {
		t.Fatalf("a move changes both groups, got %+v", changes)
	}
	if got := strings.Join(changes[0].Removed(), ","); changes[0].Key != "65101@192.0.2.17" || got != "c1" {
		t.Fatalf("rack1 change %s removed %s", changes[0].Key, got)
	}
	if got := strings.Join(changes[1].Added(), ","); changes[1].Key != "65102@192.0.2.33" || got != "c1" {
		t.Fatalf("rack2 change %s added %s", changes[1].Key, got)
	}
	if len(restore) != 0 {
		t.Fatalf("c1 keeps its uplink in rack2, so nothing is restored, got %v", restore)
	}
	g2 := FindGroup(after.Groups, "65102@192.0.2.33")
	if g2.Ports["c1"].RouterPort != "lrp-c1" {
		t.Fatalf("c1's uplink record must move with it, got %v", g2.Ports)
	}
	if addrOf(*g2, "c1") != "192.0.2.35/28" {
		t.Fatalf("c1 must get an address from rack2's pool, got %v", g2.Addresses)
	}
	if addrOf(*FindGroup(after.Groups, "65101@192.0.2.17"), "c0") != "192.0.2.18/28" {
		t.Fatal("c0 must keep its address")
	}
}

func TestClearingALabelRemovesTheMemberAndFreesItsAddress(t *testing.T) {
	st := rack1rack2(t)
	FindGroup(st.Groups, "65101@192.0.2.17").Ports = map[string]PortRecord{"c0": {RouterPort: "lrp-c0"}}

	after, changes, restore := resolve(t, st, nil, func(n *AttachState) { delete(n.Labels, "c0") })
	if len(changes) != 1 || strings.Join(changes[0].Removed(), ",") != "c0" {
		t.Fatalf("changes %+v", changes)
	}
	if len(restore) != 1 || restore[0].RouterPort != "lrp-c0" {
		t.Fatalf("restore %v", restore)
	}
	// c0's .18 is free again and goes to the next member labelled.
	after, _, _ = resolve(t, after, nil, func(n *AttachState) { n.Labels["c9"] = "rack1" })
	g1 := FindGroup(after.Groups, "65101@192.0.2.17")
	if addrOf(*g1, "c9") != "192.0.2.18/28" || addrOf(*g1, "c1") != "192.0.2.19/28" {
		t.Fatalf("addresses %v", g1.Addresses)
	}
}

func TestResolvingAgainChangesNothing(t *testing.T) {
	st := rack1rack2(t)
	_, changes, restore := resolve(t, st, nil, func(n *AttachState) {
		n.SetGroup(rackGroup(t, "rack1", "65101@192.0.2.17", "192.0.2.18/28"))
		n.Labels["c0"] = "rack1"
	})
	if len(changes) != 0 || len(restore) != 0 {
		t.Fatalf("repeating a call must change nothing, got %+v %v", changes, restore)
	}
}

func TestGroupWithNoLabelledMembers(t *testing.T) {
	after, changes, _ := resolve(t, AttachState{Labels: map[string]string{}}, nil, func(n *AttachState) {
		n.SetGroup(rackGroup(t, "rack1", "65101@192.0.2.17", "192.0.2.18/28"))
	})
	if len(changes) != 1 || len(after.Groups) != 1 || len(after.Groups[0].Members) != 0 {
		t.Fatalf("a group must be stored even before its members are labelled: %+v", after)
	}
	// Labels arriving later fill it in.
	after, _, _ = resolve(t, after, nil, func(n *AttachState) { n.Labels["c0"] = "rack1" })
	if strings.Join(after.Groups[0].Members, ",") != "c0" {
		t.Fatalf("members %v", after.Groups[0].Members)
	}
}

func TestLabelWithoutGroupIsKept(t *testing.T) {
	st := rack1rack2(t)
	after, changes, _ := resolve(t, st, nil, func(n *AttachState) { n.Labels["c5"] = "rack9" })
	if len(changes) != 0 || after.Labels["c5"] != "rack9" {
		t.Fatalf("changes %+v labels %v", changes, after.Labels)
	}
}

func TestOneGroupPerRack(t *testing.T) {
	st := rack1rack2(t)
	next := st.clone()
	next.SetGroup(rackGroup(t, "rack1", "65103@192.0.2.49", "192.0.2.50/28"))
	if _, _, _, err := ResolveGroups(st.Groups, next, nil); err == nil || !strings.Contains(err.Error(), "rack rack1 already peers with") {
		t.Fatalf("got %v", err)
	}
}

func TestLabelThatOverfillsThePoolIsRefused(t *testing.T) {
	// /30 from .1 with the leaf at .1: only .2 is usable.
	st, _, _ := resolve(t, AttachState{Labels: map[string]string{"c0": "rack1"}}, nil, func(n *AttachState) {
		n.SetGroup(rackGroup(t, "rack1", "65101@192.0.2.1", "192.0.2.1/30"))
	})
	next := st.clone()
	next.Labels["c1"] = "rack1"
	if _, _, _, err := ResolveGroups(st.Groups, next, nil); err == nil || !strings.Contains(err.Error(), "use a larger pool") {
		t.Fatalf("got %v", err)
	}
}

func TestDisablingAGroupRestoresItsPortsAndKeepsLabels(t *testing.T) {
	st := rack1rack2(t)
	FindGroup(st.Groups, "65101@192.0.2.17").Ports = map[string]PortRecord{"c0": {RouterPort: "lrp-c0"}, "c1": {RouterPort: "lrp-c1"}}
	after, changes, restore := resolve(t, st, nil, func(n *AttachState) { n.RemoveGroup("65101@192.0.2.17") })
	if len(changes) != 1 || changes[0].After != nil || strings.Join(changes[0].Removed(), ",") != "c0,c1" {
		t.Fatalf("changes %+v", changes)
	}
	if len(restore) != 2 {
		t.Fatalf("both ports must be restored, got %v", restore)
	}
	if after.Labels["c0"] != "rack1" || FindGroup(after.Groups, "65101@192.0.2.17") != nil {
		t.Fatalf("labels must stay and the group must go: %+v", after)
	}
}

func TestPortFixRecordsNewUplinks(t *testing.T) {
	st := rack1rack2(t)
	FindGroup(st.Groups, "65101@192.0.2.17").Ports = map[string]PortRecord{"c0": {RouterPort: "lrp-old"}}
	fix := func(g *AttachIntent) []PortRecord {
		if !g.HasMember("c0") {
			return nil
		}
		old := g.Ports["c0"]
		g.Ports = map[string]PortRecord{"c0": {RouterPort: "lrp-new"}}
		return []PortRecord{old}
	}
	// Nothing else changes, but the new uplink must still be recorded.
	after, changes, restore := resolve(t, st, fix, func(*AttachState) {})
	if len(changes) != 1 || changes[0].Key != "65101@192.0.2.17" {
		t.Fatalf("changes %+v", changes)
	}
	if got := FindGroup(after.Groups, "65101@192.0.2.17").Ports["c0"].RouterPort; got != "lrp-new" {
		t.Fatalf("port %s", got)
	}
	if len(restore) != 1 || restore[0].RouterPort != "lrp-old" {
		t.Fatalf("the replaced uplink must be restored, got %v", restore)
	}
}

func TestDepartedLabels(t *testing.T) {
	labels := map[string]string{"c0": "rack1", "c1": "rack1", "gone": "rack2"}
	got := departedLabels(labels, map[string]int64{"c0": 1, "c1": 2})
	if strings.Join(got, ",") != "gone" {
		t.Fatalf("got %v", got)
	}
}

func TestDecodeLabels(t *testing.T) {
	labels := decodeLabels([]database.ConfigItem{
		{Key: types.BgpAttachRackKeyPrefix + "c0", Value: "rack1"},
		{Key: types.BgpAttachGroupKeyPrefix + "65101@fe80::1", Value: "{}"},
		{Key: "microovn-bgp-asn", Value: "65000"},
	})
	if len(labels) != 1 || labels["c0"] != "rack1" {
		t.Fatalf("got %v", labels)
	}
}

// A member moving between groups keeps its uplink record (its values from
// before any attach), and its port is not restored: the new group now owns it.
func TestMoveKeepsPortRecordAndDoesNotRestoreIt(t *testing.T) {
	st := AttachState{Labels: map[string]string{"c0": "rack1"}}
	st, _, _ = resolve(t, st, nil, func(n *AttachState) {
		n.SetGroup(rackGroup(t, "rack1", "65101@fe80::1", ""))
		n.SetGroup(rackGroup(t, "rack2", "65101@192.0.2.17", "192.0.2.18/28"))
	})
	orig := PortRecord{RouterPort: "lrp-c0", Prior: lrpPrior{Networks: []string{"fe80::a4d4:ff:feb1:3ceb/64"}}}
	FindGroup(st.Groups, "65101@fe80::1").Ports = map[string]PortRecord{"c0": orig}

	after, changes, restore := resolve(t, st, nil, func(n *AttachState) { n.Labels["c0"] = "rack2" })
	if len(changes) != 2 {
		t.Fatalf("changes %+v", changes)
	}
	if len(restore) != 0 {
		t.Fatalf("the port moved with the member and must not be restored, got %v", restore)
	}
	got := FindGroup(after.Groups, "65101@192.0.2.17").Ports["c0"]
	if got.RouterPort != "lrp-c0" || !got.Prior.equal(orig.Prior) {
		t.Fatalf("record %+v, want the original %+v", got, orig)
	}
	if addrOf(*FindGroup(after.Groups, "65101@192.0.2.17"), "c0") != "192.0.2.18/28" {
		t.Fatal("c0 must get an address from the numbered pool")
	}

	// Leaving for good puts the original values back.
	_, _, restore = resolve(t, after, nil, func(n *AttachState) { delete(n.Labels, "c0") })
	if len(restore) != 1 || !restore[0].Prior.equal(orig.Prior) {
		t.Fatalf("restore %v, want the original values", restore)
	}
}
