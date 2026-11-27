package bgp

import (
	"reflect"
	"testing"
)

func TestNeutronInterconnectNames(t *testing.T) {
	cases := []struct {
		name      string
		provider  string
		wantIC    string
		wantLRP   string
		wantLocal string
	}{
		{
			name:      "standard nat network id",
			provider:  "neutron-4a2b3c4d",
			wantIC:    "bgp-ls-interconnect-neutron-4a2b3c4d",
			wantLRP:   "bgp-lrp-bgp-lr-main-to-bgp-ls-interconnect-neutron-4a2b3c4d",
			wantLocal: "provnet-4a2b3c4d",
		},
		{
			name:      "uuid-like network id",
			provider:  "neutron-12345678-1234-1234-1234-123456789abc",
			wantIC:    "bgp-ls-interconnect-neutron-12345678-1234-1234-1234-123456789abc",
			wantLRP:   "bgp-lrp-bgp-lr-main-to-bgp-ls-interconnect-neutron-12345678-1234-1234-1234-123456789abc",
			wantLocal: "provnet-12345678-1234-1234-1234-123456789abc",
		},
	}
	for _, c := range cases {
		if got := providerInterconnectSwitchName(c.provider); got != c.wantIC {
			t.Errorf("%s: providerInterconnectSwitchName(%q) = %q, want %q", c.name, c.provider, got, c.wantIC)
		}
		if got := mainRouterInterconnectLrpName(c.provider); got != c.wantLRP {
			t.Errorf("%s: mainRouterInterconnectLrpName(%q) = %q, want %q", c.name, c.provider, got, c.wantLRP)
		}
		if got := providerLocalnetLspName(c.provider); got != c.wantLocal {
			t.Errorf("%s: providerLocalnetLspName(%q) = %q, want %q", c.name, c.provider, got, c.wantLocal)
		}
	}
}

func TestNeutronFlatSwitchFindCondition(t *testing.T) {
	// The flat-switch lookup runs as `ovn-nbctl find Logical_Switch <cond>`. The
	// condition is non-obvious on two counts that each bit once: it needs the
	// "external_ids:" column prefix, and the key's colon must be backslash-escaped
	// (ovn-nbctl's column:key parser otherwise splits on the unescaped colon).
	cond := "external_ids:" + neutronFlatNetworkExtKey + "=" + neutronFlatNetworkType
	want := `external_ids:neutron\:provnet-network-type=flat`
	if cond != want {
		t.Fatalf("flat switch find condition = %q, want %q", cond, want)
	}
}

func TestInterconnectFlows(t *testing.T) {
	got := interconnectFlows(2, 28, "02:00:00:00:00:01")
	want := []string{
		"cookie=0x6d6963726f706f63,priority=10,in_port=2,actions=mod_dl_dst:02:00:00:00:00:01,output:28",
		"cookie=0x6d6963726f706f63,priority=10,in_port=28,actions=output:2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("interconnectFlows(2, 28) = %v, want %v", got, want)
	}
}

func TestInterconnectMapping(t *testing.T) {
	cases := []struct {
		name    string
		current string
		physnet string
		want    string
	}{
		{"add to empty", "", "physnet1", "physnet1:microovn-bgp-interconnect-bridge"},
		{"add alongside unrelated", "physnet1:br-provider", "physnet2",
			"physnet1:br-provider,physnet2:microovn-bgp-interconnect-bridge"},
		{"remove on empty physnet", "physnet1:microovn-bgp-interconnect-bridge", "", ""},
		{"replace on physnet change", "physnet1:microovn-bgp-interconnect-bridge,datacentre:br-ex", "physnet2",
			"datacentre:br-ex,physnet2:microovn-bgp-interconnect-bridge"},
		{"no-op when already present", "physnet1:microovn-bgp-interconnect-bridge", "physnet1",
			"physnet1:microovn-bgp-interconnect-bridge"},
		{"preserves malformed entries", "physnet1:microovn-bgp-interconnect-bridge,leftover", "physnet2",
			"leftover,physnet2:microovn-bgp-interconnect-bridge"},
	}
	for _, c := range cases {
		if got := interconnectMapping(c.current, c.physnet); got != c.want {
			t.Errorf("%s: interconnectMapping(%q, %q) = %q, want %q", c.name, c.current, c.physnet, got, c.want)
		}
	}
}

func TestNormalizeMAC(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"02:00:00:00:00:01", "02:00:00:00:00:01", true},
		{"AA:BB:CC:DD:EE:FF", "aa:bb:cc:dd:ee:ff", true},
		{" 02:00:00:00:00:01 ", "02:00:00:00:00:01", true},
		{"01:00:00:00:00:00", "", false}, // multicast bit set
		{"ff:ff:ff:ff:ff:ff", "", false}, // broadcast
		{"garbage", "", false},
		{"", "", false},
		{"02:00:00:00:00", "", false}, // too short
	}
	for _, c := range cases {
		got, ok := normalizeMAC(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("normalizeMAC(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
