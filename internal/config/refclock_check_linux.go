//go:build linux

package config

import "fmt"

// checkRefclockPlatform enforces the Linux rule that GPS serial data and the
// N_PPS line discipline cannot share one tty: attaching N_PPS replaces the
// discipline that delivers the NMEA bytes.
//
// The comparison is by device identity, not path spelling. `pps ==
// r.Device` let an alias — /dev/serial/by-id/... against /dev/ttyUSB0, or
// any other name for the same node — walk straight past the check and take
// the NMEA stream away from itself (RA6X-039). Aliases stay usable; only the
// underlying device has to differ.
func checkRefclockPlatform(r *Refclock) error {
	if r.Type != "gps" || !r.HasPPS() {
		return nil
	}
	if r.PPS == "dcd" || r.PPS == "cts" {
		return fmt.Errorf("GPS serial data and Linux N_PPS cannot share one tty; set pps to a separate /dev/ppsN, PPS GPIO, or PPS-only tty")
	}
	if sameDevice(r.PPS, r.Device) {
		return fmt.Errorf("pps %s and device %s are the same device; GPS serial data and Linux N_PPS cannot share one tty", r.PPS, r.Device)
	}
	return nil
}

// sameDevice reports whether two paths name one device. A path that cannot
// be identified is compared literally, which is what the check did before and
// is still enough to catch the obvious mistake.
func sameDevice(a, b string) bool {
	if a == b {
		return true
	}
	ida, erra := deviceIdentity(a)
	idb, errb := deviceIdentity(b)
	return erra == nil && errb == nil && ida == idb
}
