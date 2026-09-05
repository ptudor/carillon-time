//go:build freebsd && cgo && abicheck

package clock

import "testing"

// TestTimexLayout compares the hand-declared timex struct and ntp_adjtime
// constants against <sys/timex.h> on a native FreeBSD host. The C side lives
// in abi_freebsd_cgo.go because Go rejects `import "C"` in a _test.go file.
// Header-only: nothing here calls ntp_adjtime or touches the clock.
func TestTimexLayout(t *testing.T) {
	for _, f := range cTimexFacts() {
		if f.got != f.want {
			t.Errorf("%s: Go %d (%#x), C %d (%#x)", f.name, f.got, f.got, f.want, f.want)
		}
	}
}
