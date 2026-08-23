//go:build freebsd && cgo && hwtest

package clock

/*
#include <sys/timex.h>
*/
import "C"

import (
	"testing"
	"unsafe"
)

// TestTimexLayout compares the hand-declared timex struct against the C
// header on a real FreeBSD host. Run with:
//
//	CGO_ENABLED=1 go test -tags hwtest ./internal/clock/ -run TestTimexLayout
func TestTimexLayout(t *testing.T) {
	var g timex
	var c C.struct_timex
	if unsafe.Sizeof(g) != unsafe.Sizeof(c) {
		t.Fatalf("sizeof timex: Go %d, C %d", unsafe.Sizeof(g), unsafe.Sizeof(c))
	}
	check := func(name string, goOff, cOff uintptr) {
		t.Helper()
		if goOff != cOff {
			t.Errorf("offset of %s: Go %d, C %d", name, goOff, cOff)
		}
	}
	check("modes", unsafe.Offsetof(g.Modes), unsafe.Offsetof(c.modes))
	check("offset", unsafe.Offsetof(g.Offset), unsafe.Offsetof(c.offset))
	check("freq", unsafe.Offsetof(g.Freq), unsafe.Offsetof(c.freq))
	check("maxerror", unsafe.Offsetof(g.Maxerror), unsafe.Offsetof(c.maxerror))
	check("esterror", unsafe.Offsetof(g.Esterror), unsafe.Offsetof(c.esterror))
	check("status", unsafe.Offsetof(g.Status), unsafe.Offsetof(c.status))
	check("constant", unsafe.Offsetof(g.Constant), unsafe.Offsetof(c.constant))
	check("precision", unsafe.Offsetof(g.Precision), unsafe.Offsetof(c.precision))
	check("tolerance", unsafe.Offsetof(g.Tolerance), unsafe.Offsetof(c.tolerance))
	check("ppsfreq", unsafe.Offsetof(g.Ppsfreq), unsafe.Offsetof(c.ppsfreq))
	check("jitter", unsafe.Offsetof(g.Jitter), unsafe.Offsetof(c.jitter))
	check("shift", unsafe.Offsetof(g.Shift), unsafe.Offsetof(c.shift))
	check("stabil", unsafe.Offsetof(g.Stabil), unsafe.Offsetof(c.stabil))
	check("jitcnt", unsafe.Offsetof(g.Jitcnt), unsafe.Offsetof(c.jitcnt))
	check("calcnt", unsafe.Offsetof(g.Calcnt), unsafe.Offsetof(c.calcnt))
	check("errcnt", unsafe.Offsetof(g.Errcnt), unsafe.Offsetof(c.errcnt))
	check("stbcnt", unsafe.Offsetof(g.Stbcnt), unsafe.Offsetof(c.stbcnt))

	consts := []struct {
		name string
		g, c int64
	}{
		{"MOD_OFFSET", modOffset, C.MOD_OFFSET},
		{"MOD_FREQUENCY", modFrequency, C.MOD_FREQUENCY},
		{"MOD_MAXERROR", modMaxerror, C.MOD_MAXERROR},
		{"MOD_ESTERROR", modEsterror, C.MOD_ESTERROR},
		{"MOD_STATUS", modStatus, C.MOD_STATUS},
		{"MOD_TIMECONST", modTimeconst, C.MOD_TIMECONST},
		{"MOD_NANO", modNano, C.MOD_NANO},
		{"STA_PLL", staPLL, C.STA_PLL},
		{"STA_PPSFREQ", staPPSFREQ, C.STA_PPSFREQ},
		{"STA_PPSTIME", staPPSTIME, C.STA_PPSTIME},
		{"STA_FLL", staFLL, C.STA_FLL},
		{"STA_INS", staINS, C.STA_INS},
		{"STA_DEL", staDEL, C.STA_DEL},
		{"STA_UNSYNC", staUNSYNC, C.STA_UNSYNC},
		{"STA_NANO", staNano, C.STA_NANO},
	}
	for _, k := range consts {
		if k.g != k.c {
			t.Errorf("%s: Go %#x, C %#x", k.name, k.g, k.c)
		}
	}
}
