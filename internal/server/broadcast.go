package server

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// broadcastRefresh is how often the cached set of local IPv4 directed
// broadcast addresses is rebuilt. Interface changes are not observable
// portably, and this is cheap: a handful of syscalls once a minute, and only
// while datagrams are actually arriving.
const broadcastRefresh = time.Minute

// localBroadcasts is the set of IPv4 directed-broadcast addresses of this
// host's interfaces — 192.0.2.255 for a 192.0.2.0/24, and so on.
//
// It exists because a directed broadcast cannot be recognised from the
// destination address alone: 192.0.2.255 is an ordinary unicast address on a
// /23, and the limited broadcast 255.255.255.255 is the only universally
// recognisable one. Linux reports the delivery in recvmsg's flags word;
// FreeBSD does not — MSG_BCAST and MSG_MCAST are NetBSD/OpenBSD constants
// that appear in no FreeBSD header — so a datagram sent to the subnet's
// directed broadcast was answered, with the broadcast address copied into
// IP_SENDSRCADDR as the reply's source (RA6X-026). Comparing the reported
// destination against the interfaces' own broadcast configuration is the
// mechanism that works on both, and it uses no address-suffix heuristics and
// no constants that do not exist.
//
// **Policy when the metadata is unavailable:** if the interface list cannot
// be read, the address is treated as *not* a broadcast. Failing closed would
// stop the server answering anything at all, which is a worse outcome than
// the narrow case this guards, and the failure is logged by the caller.
type localBroadcasts struct {
	mu        sync.RWMutex
	addrs     map[netip.Addr]struct{}
	refreshed time.Time
	haveOnce  bool

	// enumerate is the interface lookup, replaced in tests.
	enumerate func() ([]net.Addr, error)
}

func newLocalBroadcasts() *localBroadcasts {
	return &localBroadcasts{enumerate: net.InterfaceAddrs}
}

// isBroadcast reports whether dst is one of this host's directed broadcast
// addresses, and whether the interface configuration is known at all.
func (b *localBroadcasts) isBroadcast(dst netip.Addr, now time.Time) (bcast, known bool) {
	dst = dst.Unmap()
	if !dst.Is4() {
		return false, true // IPv6 has no broadcast
	}
	b.mu.RLock()
	fresh := b.haveOnce && now.Sub(b.refreshed) < broadcastRefresh
	if fresh {
		_, ok := b.addrs[dst]
		b.mu.RUnlock()
		return ok, true
	}
	b.mu.RUnlock()

	b.mu.Lock()
	defer b.mu.Unlock()
	// Another goroutine may have refreshed while the lock was being taken.
	if b.haveOnce && now.Sub(b.refreshed) < broadcastRefresh {
		_, ok := b.addrs[dst]
		return ok, true
	}
	addrs, err := b.enumerate()
	if err != nil {
		if b.haveOnce {
			// Keep using the last known configuration rather than losing
			// the check entirely because one enumeration failed.
			_, ok := b.addrs[dst]
			return ok, true
		}
		return false, false
	}
	set := make(map[netip.Addr]struct{}, len(addrs))
	for _, a := range addrs {
		prefix, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := prefix.IP.To4()
		if ip == nil || len(prefix.Mask) != net.IPv4len {
			continue
		}
		ones, bits := prefix.Mask.Size()
		if bits != 32 || ones >= 31 {
			// A /31 or /32 has no broadcast address (RFC 3021).
			continue
		}
		var bcast [4]byte
		for i := 0; i < 4; i++ {
			bcast[i] = ip[i] | ^prefix.Mask[i]
		}
		set[netip.AddrFrom4(bcast)] = struct{}{}
	}
	b.addrs, b.refreshed, b.haveOnce = set, now, true
	_, ok := set[dst]
	return ok, true
}
