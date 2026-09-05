//go:build freebsd && cgo && abicheck

// Package-level C layout probe for the hand-declared FreeBSD timex struct.
//
// timex is declared by hand in timex_freebsd.go because the daemon is built
// with CGO_ENABLED=0. This file is the only place <sys/timex.h> is consulted.
// Go rejects `import "C"` inside a _test.go file, so the C side lives here and
// the comparison lives in abi_freebsd_test.go.
//
// The abicheck tag is deliberately distinct from hwtest: reading header
// offsets touches no clock, so this check must not require enabling the tests
// that call ntp_adjtime. Run natively with:
//
//	CGO_ENABLED=1 go test -tags abicheck ./internal/clock/ -run TestTimexLayout

package clock

/*
#include <sys/timex.h>
*/
import "C"

import "unsafe"

// cFact is one C-header fact: a size, an offset, or a constant.
type cFact struct {
	name string
	got  int64 // the hand-declared Go value
	want int64 // the value the C header says
}

// cTimexFacts returns every checked fact about <sys/timex.h>.
func cTimexFacts() []cFact {
	var g timex
	var c C.struct_timex
	off := func(name string, goOff, cOff uintptr) cFact {
		return cFact{name, int64(goOff), int64(cOff)}
	}
	return []cFact{
		{"sizeof timex", int64(unsafe.Sizeof(g)), int64(unsafe.Sizeof(c))},
		off("timex.modes", unsafe.Offsetof(g.Modes), unsafe.Offsetof(c.modes)),
		off("timex.offset", unsafe.Offsetof(g.Offset), unsafe.Offsetof(c.offset)),
		off("timex.freq", unsafe.Offsetof(g.Freq), unsafe.Offsetof(c.freq)),
		off("timex.maxerror", unsafe.Offsetof(g.Maxerror), unsafe.Offsetof(c.maxerror)),
		off("timex.esterror", unsafe.Offsetof(g.Esterror), unsafe.Offsetof(c.esterror)),
		off("timex.status", unsafe.Offsetof(g.Status), unsafe.Offsetof(c.status)),
		off("timex.constant", unsafe.Offsetof(g.Constant), unsafe.Offsetof(c.constant)),
		off("timex.precision", unsafe.Offsetof(g.Precision), unsafe.Offsetof(c.precision)),
		off("timex.tolerance", unsafe.Offsetof(g.Tolerance), unsafe.Offsetof(c.tolerance)),
		off("timex.ppsfreq", unsafe.Offsetof(g.Ppsfreq), unsafe.Offsetof(c.ppsfreq)),
		off("timex.jitter", unsafe.Offsetof(g.Jitter), unsafe.Offsetof(c.jitter)),
		off("timex.shift", unsafe.Offsetof(g.Shift), unsafe.Offsetof(c.shift)),
		off("timex.stabil", unsafe.Offsetof(g.Stabil), unsafe.Offsetof(c.stabil)),
		off("timex.jitcnt", unsafe.Offsetof(g.Jitcnt), unsafe.Offsetof(c.jitcnt)),
		off("timex.calcnt", unsafe.Offsetof(g.Calcnt), unsafe.Offsetof(c.calcnt)),
		off("timex.errcnt", unsafe.Offsetof(g.Errcnt), unsafe.Offsetof(c.errcnt)),
		off("timex.stbcnt", unsafe.Offsetof(g.Stbcnt), unsafe.Offsetof(c.stbcnt)),

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
}
