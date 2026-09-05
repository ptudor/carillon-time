//go:build freebsd

package config

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// checkRefclockPlatform catches the common FreeBSD uart setup error before
// startup. USB ucom devices use a global loader tunable and are checked by
// PPS_GETCAP when opened instead.
func checkRefclockPlatform(r *Refclock) error {
	// device is the tty whose pps_mode sysctl governs the capture, and pin
	// is the modem-control line it must be set to, when we know it.
	device, pin := r.Device, ""
	if r.Type == "gps" {
		if !r.HasPPS() {
			return nil
		}
		switch {
		case r.PPS == "dcd" || r.PPS == "cts":
			pin = r.PPS
		case filepath.IsAbs(r.PPS):
			// A second callout tty carrying the pulse only — the natural
			// wiring for a USB GPS plus a UART PPS, which main.go already
			// supports and which the Linux check accepts. DESIGN.md §5.3
			// says only that the *same* tty may be used on FreeBSD, not
			// that a separate one is forbidden. Check that device's unit.
			device = r.PPS
		default:
			return fmt.Errorf("FreeBSD GPS pps must be dcd, cts, or an absolute device path")
		}
	}
	base := filepath.Base(device)
	if !strings.HasPrefix(base, "cuau") {
		return nil
	}
	unit := strings.TrimPrefix(base, "cuau")
	if _, err := strconv.Atoi(unit); err != nil {
		return nil
	}
	name := "dev.uart." + unit + ".pps_mode"
	mode, err := unix.SysctlUint32(name)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	if mode == 0 {
		return fmt.Errorf("%s is 0; enable CTS or DCD PPS capture before starting carillon", name)
	}
	if pin != "" {
		want := uint32(2)
		if pin == "cts" {
			want = 1
		}
		if mode&3 != want {
			return fmt.Errorf("%s selects the wrong GPS PPS pin for pps=%q", name, pin)
		}
	}
	return nil
}
