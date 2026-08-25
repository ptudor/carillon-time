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

// destination returns the address the client addressed the request to and the
// control message that sends the reply back from it, so a multi-homed host
// answers on the address it was asked on. An invalid address means the kernel
// reported none and must choose the reply's source itself.
func destination(oob []byte, network string) (netip.Addr, []byte) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return netip.Addr{}, nil
	}
	for _, m := range msgs {
		if network == "udp4" && m.Header.Level == unix.IPPROTO_IP && m.Header.Type == unix.IP_PKTINFO && len(m.Data) >= unix.SizeofInet4Pktinfo {
			got := *(*unix.Inet4Pktinfo)(unsafe.Pointer(&m.Data[0]))
			return netip.AddrFrom4(got.Addr), unix.PktInfo4(&unix.Inet4Pktinfo{Ifindex: got.Ifindex, Spec_dst: got.Addr})
		}
		if network == "udp6" && m.Header.Level == unix.IPPROTO_IPV6 && m.Header.Type == unix.IPV6_PKTINFO && len(m.Data) >= unix.SizeofInet6Pktinfo {
			got := *(*unix.Inet6Pktinfo)(unsafe.Pointer(&m.Data[0]))
			return netip.AddrFrom16(got.Addr), unix.PktInfo6(&got)
		}
	}
	return netip.Addr{}, nil
}
