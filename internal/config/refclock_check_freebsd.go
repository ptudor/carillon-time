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
	return nil
}
