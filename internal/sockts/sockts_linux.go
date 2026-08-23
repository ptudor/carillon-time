//go:build linux

package sockts

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Enable requests SO_TIMESTAMPNS delivery on the socket. Every received
// datagram then carries an SCM_TIMESTAMPNS control message with the
// CLOCK_REALTIME time at which the kernel accepted it.
func Enable(rc syscall.RawConn) error {
	var serr error
	err := rc.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_TIMESTAMPNS, 1)
	})
	if err != nil {
		return fmt.Errorf("sockts: control: %w", err)
	}
	if serr != nil {
		return fmt.Errorf("sockts: setsockopt SO_TIMESTAMPNS: %w", serr)
	}
	return nil
}

// Parse extracts the SCM_TIMESTAMPNS timestamp from oob.
//
// SO_TIMESTAMPNS is the "old" variant of the option on every architecture:
// the kernel delivers a timespec laid out with the C `long` of the ABI, which
// is exactly what golang.org/x/sys/unix.Timespec is for the current GOARCH
// (two int32 on 32-bit ports such as mipsle and arm, two int64 elsewhere).
// Interpreting the payload as unix.Timespec is therefore correct on all of
// them. On 32-bit kernels this format rolls over in 2038; SO_TIMESTAMPNS_NEW
// avoids that at the cost of requiring Linux 5.1, and can replace this when
// 32-bit targets are still in service by then.
func Parse(oob []byte) (time.Time, bool) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return time.Time{}, false
	}
	for _, m := range msgs {
		if m.Header.Level != unix.SOL_SOCKET || m.Header.Type != unix.SCM_TIMESTAMPNS {
			continue
		}
		if len(m.Data) < int(unsafe.Sizeof(unix.Timespec{})) {
			return time.Time{}, false
		}
		ts := *(*unix.Timespec)(unsafe.Pointer(&m.Data[0]))
		return time.Unix(int64(ts.Sec), int64(ts.Nsec)), true
	}
	return time.Time{}, false
}
