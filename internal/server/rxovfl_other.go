//go:build !linux

package server

import "syscall"

// enableOverflowReporting is a no-op away from Linux. FreeBSD has no
// per-socket receive-overflow report; its drops are visible only in the
// system-wide "dropped due to full socket buffers" count that netstat -s
// prints, so carillon_server_kernel_drops_total stays zero there.
func enableOverflowReporting(syscall.RawConn) error { return nil }

func parseOverflow([]byte) (uint32, bool) { return 0, false }
