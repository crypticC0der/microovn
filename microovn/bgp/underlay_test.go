package bgp

import (
	"strings"
	"testing"

	"github.com/canonical/microovn/microovn/api/types"
)

func TestGenerateAsnFromNodeName(t *testing.T) {
	a := generateAsnFromNodeName("node-a")
	b := generateAsnFromNodeName("node-a")
	if a != b {
		t.Fatalf("generateAsnFromNodeName is not deterministic: %q vs %q", a, b)
	}
	for _, asn := range []string{a, b} {
		var n int64
		for _, c := range asn {
			n = n*10 + int64(c-'0')
		}
		if n < 4200000000 || n > 4294967294 {
			t.Fatalf("ASN %q outside RFC 6996 range", asn)
		}
	}
	if generateAsnFromNodeName("node-b") == a {
		t.Fatalf("expected node-b to (usually) hash differently; got equal ASNs")
	}
}

func TestParseUnderlayConfig(t *testing.T) {
	node := "my-host"

	// Defaults are filled from the node name; "lo" always exists.
	uc, err := parseUnderlayConfig(node, []string{"lo"}, nil)
	if err != nil {
		t.Fatalf("parseUnderlayConfig default: %v", err)
	}
	if uc.ASN != generateAsnFromNodeName(node) {
		t.Errorf("ASN = %q, want %q", uc.ASN, generateAsnFromNodeName(node))
	}
	if uc.RouterID != generateBGPRouterID(node) {
		t.Errorf("RouterID = %q, want %q", uc.RouterID, generateBGPRouterID(node))
	}

	// Overrides are respected.
	uc, err = parseUnderlayConfig(node, []string{"lo"}, map[string]string{
		"asn":       "4200000006",
		"router_id": "0.0.0.6",
		"ula":       "fd2b:5d1a:7c93::/48",
	})
	if err != nil {
		t.Fatalf("parseUnderlayConfig overrides: %v", err)
	}
	if uc.ASN != "4200000006" || uc.RouterID != "0.0.0.6" || uc.ULA != "fd2b:5d1a:7c93::/48" {
		t.Errorf("overrides not applied: %+v", uc)
	}
}

func TestParseUnderlayConfigInvalid(t *testing.T) {
	node := "my-host"
	cases := []struct {
		name    string
		uplinks []string
		raw     map[string]string
	}{
		{"no uplinks", nil, nil},
		{"empty uplink", []string{""}, nil},
		{"missing uplink", []string{"definitely-not-an-iface"}, nil},
		{"unknown key", []string{"lo"}, map[string]string{"bogus": "1"}},
		{"bad asn", []string{"lo"}, map[string]string{"asn": "not-an-asn"}},
		{"zero asn", []string{"lo"}, map[string]string{"asn": "0"}},
		{"bad router id", []string{"lo"}, map[string]string{"router_id": "nope"}},
		{"ipv6 router id", []string{"lo"}, map[string]string{"router_id": "fd00::1"}},
		{"bad ula", []string{"lo"}, map[string]string{"ula": "10.0.0.0/24"}},
		{"not /48 ula", []string{"lo"}, map[string]string{"ula": "fd2b::/64"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parseUnderlayConfig(node, c.uplinks, c.raw); err == nil {
				t.Fatalf("expected error for %s", c.name)
			}
		})
	}
}

func TestRenderUnderlay(t *testing.T) {
	uc := UnderlayConfig{
		Uplinks:  []string{"dpu1p1.100", "dpu2p1.100"},
		ASN:      "4200000006",
		RouterID: "0.0.0.6",
	}
	bird := renderUnderlay(uc)

	for _, want := range []string{
		underlayMarker,
		"router id 0.0.0.6;",
		"protocol device {}",
		"protocol bfd {",
		"strict bind yes;",
		`interface "dpu1p1.100" { min rx interval 100 ms; min tx interval 100 ms; multiplier 3; };`,
		`interface "dpu2p1.100" { min rx interval 100 ms; min tx interval 100 ms; multiplier 3; };`,
		"neighbor table peers;",
		"protocol radv {",
		"router discovery yes;",
		"neighbors { table peers; export all; };",
		"protocol bgp underlay_0 {",
		"protocol bgp underlay_1 {",
		"local as 4200000006;",
		"neighbor range fe80::/10 external;",
		`dynamic name "underlay_0_peer_";`,
		`dynamic name "underlay_1_peer_";`,
		"neighbors { table peers; };",
		`interface "dpu1p1.100";`,
		"export filter { if source = RTS_DEVICE then accept; reject; };",
		"bfd on;",
	} {
		if !strings.Contains(bird, want) {
			t.Errorf("renderUnderlay missing %q:\n%s", want, bird)
		}
	}
	if strings.Count(bird, "protocol bgp ") != 2 {
		t.Errorf("expected 2 bgp protocols, got:\n%s", bird)
	}
}

func TestAttachBirdConfigComposesUnderlay(t *testing.T) {
	cfg := attachConfig{
		RouterID:       "192.0.2.1",
		LocalASN:       65101,
		AdvertiseTable: 10,
		LearnTable:     10,
		Targets:        []attachTarget{{RouterPort: "external-port", Peers: []types.Peer{{Address: "fe80::1", ASN: 65002}}}},
		Underlay: &UnderlayConfig{
			Uplinks:  []string{"dpu1p1.100"},
			ASN:      "4200000006",
			RouterID: "0.0.0.6",
		},
	}
	bird := attachBirdConfig(cfg)

	// Attach router-id wins over the underlay router-id.
	if !strings.Contains(bird, "router id 192.0.2.1;") {
		t.Errorf("composed config must use the attach router id:\n%s", bird)
	}
	if strings.Contains(bird, "router id 0.0.0.6;") {
		t.Errorf("composed config must not emit the underlay router id:\n%s", bird)
	}
	for _, want := range []string{
		"neighbor table peers;",
		"protocol radv {",
		"router discovery yes;",
		"neighbors { table peers; export all; };",
		"protocol bgp underlay_0 {",
		"local as 4200000006;",
		`dynamic name "underlay_0_peer_";`,
		`interface "dpu1p1.100";`,
		"import filter { if net.len = 32 then accept; reject; };", // attach kernel (advertise==learn merged)
		"export filter { if source = RTS_DEVICE then accept; reject; };",
		"bfd on;",
		`interface "dpu1p1.100" { min rx interval 100 ms; min tx interval 100 ms; multiplier 3; };`,
	} {
		if !strings.Contains(bird, want) {
			t.Errorf("composed config missing %q:\n%s", want, bird)
		}
	}
	// The attach half is intact (uplink_0_peer_0 BGP + bfd passive).
	if !strings.Contains(bird, "protocol bgp uplink_0_peer_0 {") || !strings.Contains(bird, "bfd { passive yes; };") {
		t.Errorf("composed config lost the attach half:\n%s", bird)
	}
}
