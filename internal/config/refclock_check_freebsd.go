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
	if r.Type == "gps" {
		if !r.HasPPS() {
			return nil
		}
		if r.PPS != "dcd" && r.PPS != "cts" {
			return fmt.Errorf("FreeBSD GPS PPS must be dcd or cts on the receiver tty")
		}
	}
	base := filepath.Base(r.Device)
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
	if r.Type == "gps" {
		want := uint32(2)
		if r.PPS == "cts" {
			want = 1
		}
		if mode&3 != want {
			return fmt.Errorf("%s selects the wrong GPS PPS pin for pps=%q", name, r.PPS)
		}
	}
	return nil
}
