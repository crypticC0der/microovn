package bgp

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/canonical/microovn/microovn/api/types"
)

// req builds a request for rack1, the common case in these tests.
func req(peer, networks string) types.SetupBgpAttachRequest {
	return types.SetupBgpAttachRequest{Rack: "rack1", Peer: peer, Networks: networks}
}

func TestParseAttachIntent(t *testing.T) {
	cases := []struct {
		name    string
		req     types.SetupBgpAttachRequest
		wantErr string // substring; empty means valid
	}{
		{name: "numbered", req: req("65101@192.0.2.1", "192.0.2.0/28")},
		{name: "numbered /31 pool", req: req("65101@192.0.2.2", "192.0.2.2/31")},
		{name: "pool with a start address", req: req("65101@192.0.2.17", "192.0.2.18/28")},
		{name: "peer below the start but in the subnet", req: req("65101@192.0.2.1", "192.0.2.3/28")},
		{name: "unnumbered", req: req("65101@fe80::1", "")},
		{name: "no peer", req: req("", ""), wantErr: "exactly one peer"},
		{name: "two peers", req: req("65101@192.0.2.1,65102@192.0.2.2", "192.0.2.0/28"), wantErr: "exactly one peer"},
		{name: "bad peer", req: req("192.0.2.1", ""), wantErr: "<asn>@<address>"},
		{name: "numbered without pool", req: req("65101@192.0.2.1", ""), wantErr: "--networks is required"},
		{name: "pool not CIDR", req: req("65101@192.0.2.1", "192.0.2.0"), wantErr: "invalid CIDR"},
		{name: "pool too wide", req: req("65101@10.0.0.1", "10.0.0.0/8"), wantErr: "between /16 and /31"},
		{name: "pool /32", req: req("65101@192.0.2.1", "192.0.2.1/32"), wantErr: "between /16 and /31"},
		{name: "IPv6 pool", req: req("65101@192.0.2.1", "2001:db8::/64"), wantErr: "must be IPv4"},
		{name: "peer outside pool", req: req("65101@192.0.2.17", "192.0.2.0/28"), wantErr: "outside the --networks pool"},
		{name: "peer outside the subnet of a start address", req: req("65101@192.0.2.33", "192.0.2.18/28"), wantErr: "outside the --networks pool"},
		{name: "peer is network address", req: req("65101@192.0.2.16", "192.0.2.16/28"), wantErr: "network/broadcast"},
		{name: "unnumbered with pool", req: req("65101@fe80::1", "192.0.2.0/28"), wantErr: "only used with an IPv4 peer"},
		{name: "global IPv6 peer", req: req("65101@2001:db8::1", ""), wantErr: "invalid address"},
		{name: "reserved table", req: types.SetupBgpAttachRequest{Rack: "rack1", Peer: "65101@fe80::1", AdvertiseTable: 254}, wantErr: "reserved kernel table"},
		{name: "bad pattern", req: types.SetupBgpAttachRequest{Rack: "rack1", Peer: "65101@fe80::1", UplinkPattern: "[br-bgp"}, wantErr: "not a valid glob"},
		{name: "no rack", req: types.SetupBgpAttachRequest{Peer: "65101@fe80::1"}, wantErr: "--rack: a rack name is required"},
		{name: "blank rack", req: types.SetupBgpAttachRequest{Rack: "  ", Peer: "65101@fe80::1"}, wantErr: "--rack: a rack name is required"},
		{name: "bad rack name", req: types.SetupBgpAttachRequest{Rack: "rack 1", Peer: "65101@fe80::1"}, wantErr: "invalid rack name"},
		{name: "rack name from MAAS", req: types.SetupBgpAttachRequest{Rack: "stg-reproducer-hassahma-maas", Peer: "65101@fe80::1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseAttachIntent(c.req)
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

func TestParseAttachIntentDefaults(t *testing.T) {
	intent, err := ParseAttachIntent(types.SetupBgpAttachRequest{Rack: " rack1 ", Peer: " 65101@192.0.2.17 ", Networks: "192.0.2.18/28"})
	if err != nil {
		t.Fatal(err)
	}
	if intent.AdvertiseTable != 42 || intent.UplinkPattern != "*br-bgp" {
		t.Fatalf("defaults = table %d pattern %q", intent.AdvertiseTable, intent.UplinkPattern)
	}
	if intent.Rack != "rack1" || len(intent.Members) != 0 {
		t.Fatalf("rack %q members %v, want trimmed rack and no members yet", intent.Rack, intent.Members)
	}
	if intent.Key() != "65101@192.0.2.17" || intent.Request().Networks != "192.0.2.18/28" {
		t.Fatalf("key %q networks %q", intent.Key(), intent.Request().Networks)
	}
}

func TestGroupStorageRoundTrip(t *testing.T) {
	in, err := ParseAttachIntent(types.SetupBgpAttachRequest{
		Rack: "rack1", Peer: "65101@192.0.2.17", Networks: "192.0.2.18/28", AdvertiseTable: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	in.Members = []string{"c0", "c1"}
	if err := AllocateGroup(&in, nil); err != nil {
		t.Fatal(err)
	}
	value, err := encodeGroup(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := decodeGroup(types.BgpAttachGroupKeyPrefix+"65101@192.0.2.17", value)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Equal(in) {
		t.Fatalf("round trip changed the group: %+v vs %+v", out, in)
	}
	if out.Addresses["c0"].String() != "192.0.2.18/28" || out.Addresses["c1"].String() != "192.0.2.19/28" {
		t.Fatalf("addresses not kept: %v", out.Addresses)
	}
	if _, err := decodeGroup(types.BgpAttachGroupKeyPrefix+"65102@192.0.2.33", value); err == nil {
		t.Fatal("a record stored under another peer's key must be rejected")
	}
	if _, err := decodeGroup(types.BgpAttachGroupKeyPrefix+"65101@192.0.2.17", "{not json"); err == nil {
		t.Fatal("corrupt JSON must be rejected")
	}
}

// A group stored by the earlier build (members listed by hand, no rack) still
// loads, so it keeps working and can be disabled.
func TestDecodeGroupWithoutRack(t *testing.T) {
	old := `{"members":["c1","c0"],"peer":"65101@fe80::1","advertise_table":42,"uplink_pattern":"*br-bgp"}`
	g, err := decodeGroup(types.BgpAttachGroupKeyPrefix+"65101@fe80::1", old)
	if err != nil {
		t.Fatalf("old group must still load: %v", err)
	}
	if g.Rack != "" || strings.Join(g.Members, ",") != "c0,c1" {
		t.Fatalf("rack %q members %v", g.Rack, g.Members)
	}
	// Resolving keeps its members as they are: no label can change them.
	_, changes, _, err := ResolveGroups([]AttachIntent{g}, AttachState{Groups: []AttachIntent{g}, Labels: map[string]string{"c2": "rack1"}}, nil)
	if err != nil || len(changes) != 0 {
		t.Fatalf("changes %v, err %v; want none", changes, err)
	}
}

func TestAttachPoolUsable(t *testing.T) {
	// Network address start: everything but .16 (net), .17 (peer), .31 (bcast).
	a := attachPoolUsable(netip.MustParsePrefix("192.0.2.16/28"), netip.MustParseAddr("192.0.2.17"))
	if len(a) != 13 || a[0].String() != "192.0.2.18" || a[len(a)-1].String() != "192.0.2.30" {
		t.Fatalf("usable = %v", a)
	}
	// Start address .20: .16-.19 are left alone.
	a = attachPoolUsable(netip.MustParsePrefix("192.0.2.20/28"), netip.MustParseAddr("192.0.2.17"))
	if len(a) != 11 || a[0].String() != "192.0.2.20" {
		t.Fatalf("usable from .20 = %v", a)
	}
	// /31: no network/broadcast, only the peer is skipped.
	a = attachPoolUsable(netip.MustParsePrefix("192.0.2.2/31"), netip.MustParseAddr("192.0.2.2"))
	if len(a) != 1 || a[0].String() != "192.0.2.3" {
		t.Fatalf("usable /31 = %v", a)
	}
}
