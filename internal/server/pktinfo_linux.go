//go:build linux

package server

import (
	"fmt"
	"net/netip"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func enablePacketInfo(raw syscall.RawConn, network string) error {
	var sockErr error
	err := raw.Control(func(fd uintptr) {
		if network == "udp4" {
			sockErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_PKTINFO, 1)
		} else {
			sockErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_RECVPKTINFO, 1)
		}
	})
	if err != nil {
		return fmt.Errorf("socket control: %w", err)
	}
	if sockErr != nil {
		return fmt.Errorf("setsockopt packet info: %w", sockErr)
	}
	return nil
}

// martianReceiveFlags reports whether recvmsg's flags word says the datagram
// arrived by broadcast or multicast. Linux does not report this; the
// directed-broadcast case is caught in destination instead.
func martianReceiveFlags(int) bool { return false }

// destination returns the address the client addressed the request to and the
// control message that sends the reply back from it, so a multi-homed host
// answers on the address it was asked on. An invalid address means the kernel
// reported none and must choose the reply's source itself. martian reports a
// destination this server must not answer at all.
func destination(oob []byte, network string) (dst netip.Addr, replyOOB []byte, martian bool) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return netip.Addr{}, nil, false
	}
	for _, m := range msgs {
		if network == "udp4" && m.Header.Level == unix.IPPROTO_IP && m.Header.Type == unix.IP_PKTINFO && len(m.Data) >= unix.SizeofInet4Pktinfo {
			got := *(*unix.Inet4Pktinfo)(unsafe.Pointer(&m.Data[0]))
			// ipi_addr is the destination in the header; ipi_spec_dst is the
			// local address. They are equal for a unicast request. For a
			// datagram sent to the subnet's directed broadcast address
			// (192.168.1.255) ipi_addr is the broadcast address and
			// ipi_spec_dst the interface address, and answering it would
			// make one forged datagram ask every NTP server on the segment
			// to answer the victim at once.
			if got.Addr != got.Spec_dst {
				return netip.Addr{}, nil, true
			}
			// The reply's source comes from ipi_spec_dst, never ipi_addr: a
			// non-local source is refused by the route lookup on Linux and
			// accepted as a broadcast source on FreeBSD.
			return netip.AddrFrom4(got.Addr), unix.PktInfo4(&unix.Inet4Pktinfo{Ifindex: got.Ifindex, Spec_dst: got.Spec_dst}), false
		}
		if network == "udp6" && m.Header.Level == unix.IPPROTO_IPV6 && m.Header.Type == unix.IPV6_PKTINFO && len(m.Data) >= unix.SizeofInet6Pktinfo {
			// IPv6 has no broadcast; multicast is covered by
			// martianDestination.
			got := *(*unix.Inet6Pktinfo)(unsafe.Pointer(&m.Data[0]))
			return netip.AddrFrom16(got.Addr), unix.PktInfo6(&got), false
		}
	}
	return netip.Addr{}, nil, false
}
