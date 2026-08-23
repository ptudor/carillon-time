//go:build !linux && !freebsd

package sockts

import (
	"syscall"
	"time"
)

// Enable is a no-op on platforms without a receive-timestamp backend.
func Enable(rc syscall.RawConn) error { return nil }

// Parse never finds a timestamp on platforms without a backend; callers
// read the clock themselves instead.
func Parse(oob []byte) (time.Time, bool) { return time.Time{}, false }
