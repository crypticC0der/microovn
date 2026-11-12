package bgp

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/canonical/microovn/microovn/api/types"
)

func TestParseUUIDSet(t *testing.T) {
	cases := map[string][]string{
		"[]":                       nil,
		"":                         nil,
		"1b2c-uuid":                {"1b2c-uuid"},
		"[1b2c-uuid, 3d4e-uuid]\n": {"1b2c-uuid", "3d4e-uuid"},
	}
	for in, want := range cases {
		if got := parseUUIDSet(in); !slices.Equal(got, want) {
			t.Errorf("parseUUIDSet(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestMatchUplinkNames(t *testing.T) {
	names := []string{
		"bgp-lrp-bgp-lr-c0-to-bgp-ls-c0-br-bgp",
		"bgp-lrp-bgp-lr-c0-to-bgp-lr-main",
		"lrp-other",
	}
	got := matchUplinkNames(types.DefaultBgpAttachUplinkPattern, names)
	if !slices.Equal(got, []string{"bgp-lrp-bgp-lr-c0-to-bgp-ls-c0-br-bgp"}) {
		t.Fatalf("default pattern matched %v", got)
	}
	if got := matchUplinkNames("*", names); len(got) != 3 {
		t.Fatalf("* matched %v", got)
	}
}

func TestPickUplink(t *testing.T) {
	if _, err := pickUplink("c0", "*br-bgp", nil); err == nil || !strings.Contains(err.Error(), "no router port matching") {
		t.Fatalf("want a no-match error, got %v", err)
	}
	if got, err := pickUplink("c0", "*br-bgp", []string{"a"}); err != nil || got != "a" {
		t.Fatalf("single = %q, %v", got, err)
	}
	if _, err := pickUplink("c0", "*", []string{"a", "b"}); err == nil || !strings.Contains(err.Error(), "narrow --uplink-pattern") {
		t.Fatalf("want a multiple-match error, got %v", err)
	}
}

func TestJournalRoundTrip(t *testing.T) {
	in := lrpPrior{Networks: []string{"192.0.2.3/31"}, MaintainVrf: "true", Peers: "65101@192.0.2.2"}
	enc, err := encodeJournal(in)
	if err != nil {
		t.Fatal(err)
	}
	// Must be safe as a bare OVS value: no quotes, commas, spaces or '='.
	if strings.ContainsAny(enc, "\"', =") {
		t.Fatalf("journal value %q needs quoting", enc)
	}
	var out lrpPrior
	if err := decodeJournal(enc, &out); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.Networks, in.Networks) || out.MaintainVrf != in.MaintainVrf || out.Attach != in.Attach || out.Peers != in.Peers {
		t.Fatalf("round trip: %+v -> %+v", in, out)
	}
	if err := decodeJournal("not base64!", &out); err == nil {
		t.Fatal("corrupt journal must fail")
	}
}

func TestNetworksArg(t *testing.T) {
	if got := networksArg([]string{"192.0.2.3/28"}); got != `networks="192.0.2.3/28"` {
		t.Fatalf("got %s", got)
	}
	if got := networksArg([]string{"192.0.2.3/28", "fe80::1/64"}); got != `networks="192.0.2.3/28","fe80::1/64"` {
		t.Fatalf("got %s", got)
	}
}

func TestLrpSetArgs(t *testing.T) {
	intent, err := ParseAttachIntent(types.SetupBgpAttachRequest{Rack: "rack1", Peer: "65101@192.0.2.17", Networks: "192.0.2.18/28"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(lrpSetArgs("lrp", desiredLrpFields(intent, netip.MustParsePrefix("192.0.2.18/28")), true), " ")
	want := `set Logical_Router_Port lrp networks="192.0.2.18/28" options:dynamic-routing-maintain-vrf=true external_ids:microovn-bgp-attach=true external_ids:microovn-bgp-peers="65101@192.0.2.17"`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}

	// Unnumbered never touches networks.
	unnumbered, _ := ParseAttachIntent(types.SetupBgpAttachRequest{Rack: "rack1", Peer: "65101@fe80::1"})
	if got := strings.Join(lrpSetArgs("lrp", desiredLrpFields(unnumbered, netip.Prefix{}), false), " "); strings.Contains(got, "networks=") {
		t.Fatalf("unnumbered wrote networks: %s", got)
	}
}

func TestLrpRestoreArgs(t *testing.T) {
	// A fresh Neutron port: our keys come off, networks is cleared.
	got := joinNbCommands(lrpRestoreArgs("lrp", lrpPrior{}))
	want := "--if-exists clear Logical_Router_Port lrp networks -- " +
		"--if-exists remove Logical_Router_Port lrp options dynamic-routing-maintain-vrf -- " +
		"--if-exists remove Logical_Router_Port lrp external_ids microovn-bgp-attach -- " +
		"--if-exists remove Logical_Router_Port lrp external_ids microovn-bgp-peers"
	if strings.Join(got, " ") != want {
		t.Fatalf("got  %s\nwant %s", strings.Join(got, " "), want)
	}

	// Values set by hand before attach are put back as they were.
	got = joinNbCommands(lrpRestoreArgs("lrp", lrpPrior{Networks: []string{"192.0.2.3/31"}, MaintainVrf: "true", Peers: "65101@192.0.2.2"}))
	joined := strings.Join(got, " ")
	for _, part := range []string{
		`--if-exists set Logical_Router_Port lrp networks="192.0.2.3/31"`,
		`--if-exists set Logical_Router_Port lrp options:dynamic-routing-maintain-vrf="true"`,
		"--if-exists remove Logical_Router_Port lrp external_ids microovn-bgp-attach",
		`--if-exists set Logical_Router_Port lrp external_ids:microovn-bgp-peers="65101@192.0.2.2"`,
	} {
		if !strings.Contains(joined, part) {
			t.Errorf("missing %q in %s", part, joined)
		}
	}
}

func TestGroupPortCommandsOneTransaction(t *testing.T) {
	g, err := MergeGroup(nil, mustGroup(t, []string{"c0", "c1", "c2"}, "65101@192.0.2.17", "192.0.2.18/28"))
	if err != nil {
		t.Fatal(err)
	}
	g.Ports = map[string]PortRecord{
		"c0": {RouterPort: "lrp-c0"},
		"c1": {RouterPort: "lrp-c1"},
		// c2's uplink was not found: it is skipped, not guessed.
	}
	args := joinNbCommands(groupPortCommands(g, []string{"c0", "c1", "c2"}))
	joined := strings.Join(args, " ")
	if strings.Count(joined, "set Logical_Router_Port") != 2 || strings.Count(joined, " -- ") != 1 {
		t.Fatalf("want two commands in one call, got: %s", joined)
	}
	for _, part := range []string{`lrp-c0 networks="192.0.2.18/28"`, `lrp-c1 networks="192.0.2.19/28"`} {
		if !strings.Contains(joined, part) {
			t.Errorf("missing %q in %s", part, joined)
		}
	}
	// Only the members asked for.
	if got := groupPortCommands(g, []string{"c1"}); len(got) != 1 || got[0][2] != "lrp-c1" {
		t.Fatalf("subset: %v", got)
	}
}

func TestDroppedPorts(t *testing.T) {
	before := AttachIntent{Members: []string{"c0", "c1", "c2"}, Ports: map[string]PortRecord{
		"c0": {RouterPort: "lrp-c0"}, "c1": {RouterPort: "lrp-c1"}, "c2": {RouterPort: "lrp-c2"},
	}}
	after := AttachIntent{Members: []string{"c0", "c2"}}
	got := DroppedPorts(&before, &after)
	if len(got) != 1 || got[0].RouterPort != "lrp-c1" {
		t.Fatalf("got %v", got)
	}
	if got := DroppedPorts(&before, nil); len(got) != 3 {
		t.Fatalf("deleting the group must restore every port, got %v", got)
	}
	if got := DroppedPorts(nil, &after); got != nil {
		t.Fatalf("nothing stored before: got %v", got)
	}
}
