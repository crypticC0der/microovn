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

func TestParsePeersNumbered(t *testing.T) {
	peers, err := ParsePeers("65111@10.0.0.2,65112@192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 2 {
		t.Fatalf("expected 2 peers, got %d", len(peers))
	}
	if peers[0].ASN != 65111 || peers[0].Address != "10.0.0.2" {
		t.Fatalf("unexpected first peer %+v", peers[0])
	}
	if peers[1].ASN != 65112 || peers[1].Address != "192.0.2.1" {
		t.Fatalf("unexpected second peer %+v", peers[1])
	}
}

func TestParsePeersRejectsInvalid(t *testing.T) {
	for _, raw := range []string{
		"64513",                       // missing @
		"0@fe80::1",                   // zero ASN
		"64513@fe80::1@extra",         // too many @
		"64513@fe80::1,64514@fe80::1", // duplicate address
		"64513@127.0.0.1",             // loopback peer
		"64513@169.254.0.1",           // IPv4 link-local (unnumbered uses v6)
		"64513@224.0.0.1",             // multicast peer
		"64513@2001:db8::1",           // global IPv6, not link-local
		"64513@fe80::1%eth0",          // zoned link-local
	} {
		if _, err := ParsePeers(raw); err == nil {
			t.Errorf("expected error for %q", raw)
		}
	}
}
