//go:build linux && cgo && abicheck

package pps

import "testing"

// TestLinuxLayouts compares the hand-declared Linux PPS ABI in pps_linux.go
// against <linux/pps.h>. The C side lives in abi_linux_cgo.go because Go
// rejects `import "C"` in a _test.go file. Header-only: no device is opened
// and no clock is touched, which is why this uses the abicheck tag and not
// hwtest.
func TestLinuxLayouts(t *testing.T) {
	for _, c := range cLinuxLayouts() {
		if c.got != c.want {
			t.Errorf("%s: Go %d (%#x), C %d (%#x)", c.name, c.got, c.got, c.want, c.want)
		}
	}
}
