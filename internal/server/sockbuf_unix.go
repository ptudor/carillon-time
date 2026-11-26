//go:build linux || freebsd || darwin

package server

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// receiveBufferSize returns the socket's effective SO_RCVBUF. Linux reports
// twice what was requested because it accounts for bookkeeping overhead, and
// FreeBSD reports the granted request directly.
func receiveBufferSize(raw syscall.RawConn) (int, error) {
	var size int
	var sockErr error
	err := raw.Control(func(fd uintptr) {
		size, sockErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF)
	})
	if err != nil {
		return 0, fmt.Errorf("socket control: %w", err)
	}
	if sockErr != nil {
		return 0, fmt.Errorf("getsockopt SO_RCVBUF: %w", sockErr)
	}
	return size, nil
}
