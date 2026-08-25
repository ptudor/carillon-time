//go:build freebsd

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
			sockErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_RECVDSTADDR, 1)
		} else {
			sockErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_RECVPKTINFO, 1)
		}
	})
	if err != nil {
		return fmt.Errorf("socket control: %w", err)
	}
	if sockErr != nil {
		return fmt.Errorf("setsockopt destination address: %w", sockErr)
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
		if network == "udp4" && m.Header.Level == unix.IPPROTO_IP && m.Header.Type == unix.IP_RECVDSTADDR && len(m.Data) >= 4 {
			return netip.AddrFrom4([4]byte(m.Data[:4])), marshalControl(unix.IPPROTO_IP, unix.IP_SENDSRCADDR, m.Data[:4])
		}
		if network == "udp6" && m.Header.Level == unix.IPPROTO_IPV6 && m.Header.Type == unix.IPV6_PKTINFO && len(m.Data) >= unix.SizeofInet6Pktinfo {
			got := *(*unix.Inet6Pktinfo)(unsafe.Pointer(&m.Data[0]))
			return netip.AddrFrom16(got.Addr), marshalControl(unix.IPPROTO_IPV6, unix.IPV6_PKTINFO, m.Data[:unix.SizeofInet6Pktinfo])
		}
	}
	return netip.Addr{}, nil
}

func marshalControl(level, typ int, data []byte) []byte {
	b := make([]byte, unix.CmsgSpace(len(data)))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level = int32(level)
	h.Type = int32(typ)
	h.SetLen(unix.CmsgLen(len(data)))
	copy(b[unix.CmsgLen(0):], data)
	return b
}
