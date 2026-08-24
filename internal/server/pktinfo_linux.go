//go:build linux

package server

import (
	"fmt"
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

func sourceControl(oob []byte, network string) []byte {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil
	}
	for _, m := range msgs {
		if network == "udp4" && m.Header.Level == unix.IPPROTO_IP && m.Header.Type == unix.IP_PKTINFO && len(m.Data) >= unix.SizeofInet4Pktinfo {
			got := *(*unix.Inet4Pktinfo)(unsafe.Pointer(&m.Data[0]))
			return unix.PktInfo4(&unix.Inet4Pktinfo{Ifindex: got.Ifindex, Spec_dst: got.Addr})
		}
		if network == "udp6" && m.Header.Level == unix.IPPROTO_IPV6 && m.Header.Type == unix.IPV6_PKTINFO && len(m.Data) >= unix.SizeofInet6Pktinfo {
			got := *(*unix.Inet6Pktinfo)(unsafe.Pointer(&m.Data[0]))
			return unix.PktInfo6(&got)
		}
	}
	return nil
}
