//go:build freebsd && cgo && abicheck && (amd64 || arm64)

package pps

import "testing"

// TestFreeBSDLayouts compares the hand-declared FreeBSD PPS ABI in
// pps_freebsd.go against <sys/timepps.h>. Header-only; see abi_linux_test.go.
func TestFreeBSDLayouts(t *testing.T) {
	for _, c := range cFreeBSDLayouts() {
		if c.got != c.want {
			t.Errorf("%s: Go %d (%#x), C %d (%#x)", c.name, c.got, c.got, c.want, c.want)
		}
	}
}
