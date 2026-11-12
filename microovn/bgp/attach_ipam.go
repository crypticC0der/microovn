package bgp

// Node address allocation for numbered bgp-attach groups.
//
//	subnet = the pool's prefix (192.0.2.18/28 -> 192.0.2.16/28)
//	usable = subnet addresses from the pool's address upward, minus
//	         {network, broadcast, peer}, in order
//
// Allocation runs once, centrally, when a group is set, and the result is
// stored with the group:
//
//  1. a member keeps the address it already has, if it is still usable;
//  2. every other member gets the lowest free usable address, in member-name
//     order.
//
// So a pool only needs one address per member of its own group (a /27 holds 29
// members: 32 minus network, broadcast and leaf), addresses never move while a
// member stays in its group, and a removed member's address becomes free for
// the next one added.

import (
	"fmt"
	"net/netip"
)

// attachPoolUsable lists the addresses that can be given to nodes: those of the
// pool's subnet from the pool's address upward, except the network and
// broadcast addresses (for subnets shorter than /31, which have no such
// distinction) and the peer.
func attachPoolUsable(pool netip.Prefix, peer netip.Addr) []netip.Addr {
	subnet := pool.Masked()
	var usable []netip.Addr
	for a := pool.Addr(); a.IsValid() && subnet.Contains(a); a = a.Next() {
		if a == peer || isIPv4NetOrBroadcast(a, subnet) {
			continue
		}
		usable = append(usable, a)
	}
	return usable
}

// allocateGroupAddresses assigns an address to every member of a numbered
// group, keeping previous assignments that are still usable. members must be
// sorted (ParseAttachIntent sorts them), which makes the result deterministic.
func allocateGroupAddresses(pool netip.Prefix, peer netip.Addr, members []string, previous map[string]netip.Prefix) (map[string]netip.Prefix, error) {
	usable := attachPoolUsable(pool, peer)
	if len(usable) < len(members) {
		return nil, fmt.Errorf("pool %s has %d usable addresses but %d members need one; use a larger pool",
			pool, len(usable), len(members))
	}
	isUsable := map[netip.Addr]bool{}
	for _, a := range usable {
		isUsable[a] = true
	}

	out := map[string]netip.Prefix{}
	taken := map[netip.Addr]bool{}

	// 1. Keep what members already have.
	for _, m := range members {
		prev, ok := previous[m]
		if !ok || prev.Bits() != pool.Bits() || !isUsable[prev.Addr()] || taken[prev.Addr()] {
			continue
		}
		out[m] = prev
		taken[prev.Addr()] = true
	}

	// 2. Give the rest the lowest free address.
	next := 0
	for _, m := range members {
		if _, ok := out[m]; ok {
			continue
		}
		for taken[usable[next]] {
			next++
		}
		out[m] = netip.PrefixFrom(usable[next], pool.Bits())
		taken[usable[next]] = true
	}
	return out, nil
}

// AllocateGroup fills in a numbered group's member addresses, keeping those of
// previous (the stored version of the same group, if any). Unnumbered groups
// get none.
func AllocateGroup(intent *AttachIntent, previous *AttachIntent) error {
	if !intent.Numbered() {
		intent.Addresses = nil
		return nil
	}
	var prev map[string]netip.Prefix
	if previous != nil {
		prev = previous.Addresses
	}
	addrs, err := allocateGroupAddresses(intent.Pool, peerAddr(*intent), intent.Members, prev)
	if err != nil {
		return err
	}
	intent.Addresses = addrs
	return nil
}
