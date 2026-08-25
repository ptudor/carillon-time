package server

import "net/netip"

// v4Broadcast is the IPv4 limited broadcast address, 255.255.255.255.
var v4Broadcast = netip.AddrFrom4([4]byte{255, 255, 255, 255})

// martianSource reports whether a datagram's source address and port make it
// impossible for a real client to be waiting for the answer.
//
// This check is independent of the [serve] ACL on purpose. An operator who
// serves the public internet writes allow = ["0.0.0.0/0"], which makes the
// ACL match every martian too, and answering one turns a single spoofed
// datagram into a packet aimed at a multicast group or a broadcast address.
// ntpd and chrony both drop these regardless of their own access rules.
//
// Loopback sources are deliberately allowed: a host legitimately queries its
// own server over 127.0.0.1, and the kernel already refuses loopback-sourced
// packets arriving on a non-loopback interface.
func martianSource(from netip.AddrPort) bool {
	addr := from.Addr().Unmap()
	switch {
	case !addr.IsValid():
		return true
	case from.Port() == 0:
		// A reply would be undeliverable; only a forged header gets here.
		return true
	case addr.IsUnspecified(), addr.IsMulticast(), addr.IsLinkLocalMulticast(), addr.IsInterfaceLocalMulticast():
		return true
	case addr == v4Broadcast:
		return true
	case addr.Is4() && addr.As4()[0] == 0:
		// 0.0.0.0/8, "this network", is never a valid source (RFC 6890).
		return true
	}
	return false
}

// martianDestination reports whether a datagram was addressed to something
// this server must not answer as if it were a unicast request.
//
// The reply is sent from the address the client wrote in the header, so that
// a multi-homed host answers on the address it was asked on. A broadcast or
// multicast destination would make that source address illegal: the send
// fails on Linux and FreeBSD alike, and one datagram to a directed broadcast
// would otherwise ask every host on a subnet to answer at once.
func martianDestination(dst netip.Addr) bool {
	dst = dst.Unmap()
	if !dst.IsValid() {
		// No destination cmsg: the kernel picks the source address itself.
		return false
	}
	return dst.IsUnspecified() || dst.IsMulticast() || dst == v4Broadcast
}
