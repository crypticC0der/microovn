package bgp

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/canonical/microovn/microovn/api/types"
)

func addrs(t *testing.T, m map[string]netip.Prefix) string {
	t.Helper()
	var parts []string
	for _, k := range sortedKeys(m) {
		parts = append(parts, k+"="+m[k].String())
	}
	return strings.Join(parts, " ")
}

func sortedKeys(m map[string]netip.Prefix) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

var (
	pool18 = netip.MustParsePrefix("192.0.2.18/28")
	leaf17 = netip.MustParseAddr("192.0.2.17")
)

func TestAllocateNextFreeInMemberOrder(t *testing.T) {
	got, err := allocateGroupAddresses(pool18, leaf17, []string{"c0", "c1", "c2"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "c0=192.0.2.18/28 c1=192.0.2.19/28 c2=192.0.2.20/28"; addrs(t, got) != want {
		t.Fatalf("got %s, want %s", addrs(t, got), want)
	}
}

func TestAllocateKeepsExistingAddresses(t *testing.T) {
	first, _ := allocateGroupAddresses(pool18, leaf17, []string{"c0", "c2"}, nil) // c0=.18 c2=.19
	// c1 joins: it sorts between them but must not shift c2.
	got, err := allocateGroupAddresses(pool18, leaf17, []string{"c0", "c1", "c2"}, first)
	if err != nil {
		t.Fatal(err)
	}
	if want := "c0=192.0.2.18/28 c1=192.0.2.20/28 c2=192.0.2.19/28"; addrs(t, got) != want {
		t.Fatalf("got %s, want %s", addrs(t, got), want)
	}
}

func TestAllocateReusesRemovedMembersAddress(t *testing.T) {
	first, _ := allocateGroupAddresses(pool18, leaf17, []string{"c0", "c1", "c2"}, nil) // .18 .19 .20
	// c1 leaves: c0 and c2 keep theirs.
	second, _ := allocateGroupAddresses(pool18, leaf17, []string{"c0", "c2"}, first)
	if want := "c0=192.0.2.18/28 c2=192.0.2.20/28"; addrs(t, second) != want {
		t.Fatalf("after removal got %s, want %s", addrs(t, second), want)
	}
	// c9 joins and takes the freed .19.
	third, _ := allocateGroupAddresses(pool18, leaf17, []string{"c0", "c2", "c9"}, second)
	if third["c9"].String() != "192.0.2.19/28" {
		t.Fatalf("c9 got %s, want the freed 192.0.2.19/28", third["c9"])
	}
}

func TestAllocateReassignsWhenPoolChanges(t *testing.T) {
	first, _ := allocateGroupAddresses(pool18, leaf17, []string{"c0"}, nil)
	// New pool in another subnet: the old address is no longer usable.
	got, err := allocateGroupAddresses(netip.MustParsePrefix("192.0.2.34/28"), netip.MustParseAddr("192.0.2.33"), []string{"c0"}, first)
	if err != nil {
		t.Fatal(err)
	}
	if got["c0"].String() != "192.0.2.34/28" {
		t.Fatalf("got %s, want 192.0.2.34/28", got["c0"])
	}
	// Same subnet, later start: an address below the start is given up.
	got, _ = allocateGroupAddresses(netip.MustParsePrefix("192.0.2.25/28"), leaf17, []string{"c0"}, first)
	if got["c0"].String() != "192.0.2.25/28" {
		t.Fatalf("got %s, want 192.0.2.25/28", got["c0"])
	}
}

func TestAllocatePoolTooSmall(t *testing.T) {
	// /29 from .2 with leaf .1: .2-.6 = 5 addresses.
	pool := netip.MustParsePrefix("192.0.2.2/29")
	leaf := netip.MustParseAddr("192.0.2.1")
	five := []string{"a", "b", "c", "d", "e"}
	if _, err := allocateGroupAddresses(pool, leaf, five, nil); err != nil {
		t.Fatalf("5 members fit: %v", err)
	}
	if _, err := allocateGroupAddresses(pool, leaf, append(five, "f"), nil); err == nil || !strings.Contains(err.Error(), "5 usable addresses but 6 members") {
		t.Fatalf("6 members must not fit, got %v", err)
	}
}

func TestAllocateSkipsLeafInsideRange(t *testing.T) {
	got, _ := allocateGroupAddresses(pool18, netip.MustParseAddr("192.0.2.19"), []string{"a", "b"}, nil)
	if want := "a=192.0.2.18/28 b=192.0.2.20/28"; addrs(t, got) != want {
		t.Fatalf("got %s, want %s", addrs(t, got), want)
	}
}

// A rack's pool is sized by its own member count, whatever the cluster member
// IDs are: a /27 holds 29 nodes (32 minus network, broadcast and leaf), a
// 30-node rack needs a /26.
func TestAllocateRackSizedPools(t *testing.T) {
	members := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("compute-%02d", i+61)
		}
		return out
	}
	leaf := netip.MustParseAddr("10.20.3.1")
	got, err := allocateGroupAddresses(netip.MustParsePrefix("10.20.3.0/27"), leaf, members(29), nil)
	if err != nil {
		t.Fatalf("29 members must fit a /27: %v", err)
	}
	seen := map[netip.Addr]bool{}
	for _, a := range got {
		if seen[a.Addr()] {
			t.Fatalf("duplicate %s", a)
		}
		seen[a.Addr()] = true
	}
	if _, err := allocateGroupAddresses(netip.MustParsePrefix("10.20.3.0/27"), leaf, members(30), nil); err == nil {
		t.Fatal("30 members must not fit a /27")
	}
	if _, err := allocateGroupAddresses(netip.MustParsePrefix("10.20.3.0/26"), leaf, members(30), nil); err != nil {
		t.Fatalf("30 members must fit a /26: %v", err)
	}
}

func TestAllocateGroupUnnumbered(t *testing.T) {
	g, err := ParseAttachIntent(types.SetupBgpAttachRequest{Rack: "rack1", Peer: "65101@fe80::1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := AllocateGroup(&g, nil); err != nil || g.Addresses != nil {
		t.Fatalf("unnumbered got %v, %v", g.Addresses, err)
	}
}
