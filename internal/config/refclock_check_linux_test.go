//go:build linux

package config

import (
	"strings"
	"testing"
)

// TestLinuxSharedTTYIsAllowed covers the lifted N_PPS restriction. pps_ldisc
// inherits the N_TTY operations, so a receiver may carry NMEA and its pulse on
// one port; refusing pps = "dcd" forced an ldattach(8) or a second port for
// nothing.
func TestLinuxSharedTTYIsAllowed(t *testing.T) {
	for _, pps := range []string{"dcd", "/dev/ttyS0", "/dev/pps0"} {
		r := &Refclock{Name: "gps", Type: "gps", Device: "/dev/ttyS0", PPS: pps}
		if err := checkRefclockPlatform(r); err != nil {
			t.Errorf("pps %q on the NMEA tty was refused: %v", pps, err)
		}
	}
}

// TestLinuxRejectsCTS keeps the one real restriction: pps_ldisc hooks
// dcd_change and has no equivalent for CTS.
func TestLinuxRejectsCTS(t *testing.T) {
	r := &Refclock{Name: "gps", Type: "gps", Device: "/dev/ttyS0", PPS: "cts"}
	err := checkRefclockPlatform(r)
	if err == nil {
		t.Fatal("pps = \"cts\" was accepted on Linux")
	}
	if !strings.Contains(err.Error(), "DCD") {
		t.Errorf("the CTS refusal does not name the pin Linux does capture: %v", err)
	}
}

// TestLinuxIgnoresNonPPSRefclocks checks the platform rule stays scoped to a
// GPS block that has actually enabled PPS.
func TestLinuxIgnoresNonPPSRefclocks(t *testing.T) {
	for _, r := range []*Refclock{
		{Name: "gps", Type: "gps", Device: "/dev/ttyS0", PPS: "none"},
		{Name: "pps", Type: "pps", Device: "/dev/ttyS0"},
	} {
		if err := checkRefclockPlatform(r); err != nil {
			t.Errorf("refclock %+v was refused: %v", r, err)
		}
	}
}
