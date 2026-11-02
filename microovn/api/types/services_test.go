package types

import (
	"testing"
)

func TestParsePeers(t *testing.T) {
	peers, err := ParsePeers("64513@fe80::1,64514@fe80::2")
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 2 {
		t.Fatalf("expected 2 peers, got %d", len(peers))
	}
	if peers[0].ASN != 64513 || peers[0].Address != "fe80::1" {
		t.Fatalf("unexpected first peer %+v", peers[0])
	}
	if peers[1].ASN != 64514 || peers[1].Address != "fe80::2" {
		t.Fatalf("unexpected second peer %+v", peers[1])
	}
}

func TestParsePeersRejectsInvalid(t *testing.T) {
	for _, raw := range []string{
		"64513",                       // missing @
		"0@fe80::1",                   // zero ASN
		"64513@192.0.2.1",             // not link-local v6
		"64513@fe80::1@extra",         // too many @
		"64513@fe80::1,64514@fe80::1", // duplicate address
	} {
		if _, err := ParsePeers(raw); err == nil {
			t.Errorf("expected error for %q", raw)
		}
	}
}
