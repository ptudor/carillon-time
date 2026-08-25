//go:build linux

package server

import (
	"encoding/binary"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// enableOverflowReporting asks the kernel to report, alongside each received
// datagram, how many datagrams it has dropped on this socket because the
// receive queue was full. Without it a server that cannot keep up looks
// identical to a quiet one.
func enableOverflowReporting(raw syscall.RawConn) error {
	var sockErr error
	err := raw.Control(func(fd uintptr) {
		sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RXQ_OVFL, 1)
	})
	if err != nil {
		return fmt.Errorf("socket control: %w", err)
	}
	if sockErr != nil {
		return fmt.Errorf("setsockopt SO_RXQ_OVFL: %w", sockErr)
	}
	return nil
}

// parseOverflow returns the socket's cumulative drop counter, which the
// kernel keeps as a 32-bit value that wraps.
func parseOverflow(oob []byte) (uint32, bool) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return 0, false
	}
	for _, m := range msgs {
		if m.Header.Level == unix.SOL_SOCKET && m.Header.Type == unix.SO_RXQ_OVFL && len(m.Data) >= 4 {
			return binary.NativeEndian.Uint32(m.Data[:4]), true
		}
	}
	return 0, false
}
