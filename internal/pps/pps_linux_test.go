//go:build linux

package pps

import "testing"

func TestLinuxPPSPath(t *testing.T) {
	for _, path := range []string{"/dev/pps0", "/dev/pps15", "pps2"} {
		if !linuxPPSPath(path) {
			t.Errorf("%q should be a PPS device", path)
		}
	}
	for _, path := range []string{"/dev/ttyS0", "/dev/pps", "/dev/pps-foo"} {
		if linuxPPSPath(path) {
			t.Errorf("%q should be a tty", path)
		}
	}
}
