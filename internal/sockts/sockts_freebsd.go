//go:build freebsd

package sockts

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Enable requests receive timestamps on the socket: SO_TIMESTAMP turns them
// on and SO_TS_CLOCK selects SO_TS_REALTIME so the kernel delivers a
// nanosecond CLOCK_REALTIME timespec in an SCM_REALTIME control message
// rather than the default microsecond timeval.
func Enable(rc syscall.RawConn) error {
	var serr error
	err := rc.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_TIMESTAMP, 1)
		if serr != nil {
			serr = fmt.Errorf("setsockopt SO_TIMESTAMP: %w", serr)
			return
		}
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_TS_CLOCK, unix.SO_TS_REALTIME)
		if serr != nil {
			serr = fmt.Errorf("setsockopt SO_TS_CLOCK: %w", serr)
		}
	})
	if err != nil {
		return fmt.Errorf("sockts: control: %w", err)
	}
	if serr != nil {
		return fmt.Errorf("sockts: %w", serr)
	}
	return nil
}

// Parse extracts the SCM_REALTIME timestamp from oob. The payload is a
// struct timespec, which is unix.Timespec on both FreeBSD targets (amd64 and
// arm64 have a 64-bit time_t).
func Parse(oob []byte) (time.Time, bool) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return time.Time{}, false
	}
	for _, m := range msgs {
		if m.Header.Level != unix.SOL_SOCKET || m.Header.Type != unix.SCM_REALTIME {
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
