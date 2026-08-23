package clock

import (
	"time"

	"carillon/internal/ntp"
)

// Kernel clock status bits. The values are identical in Linux <linux/timex.h>
// and FreeBSD <sys/timex.h>; the Linux backend asserts that at compile time
// against golang.org/x/sys/unix.
const (
	staPLL     = 0x0001 // enable PLL updates (kernel discipline) — always cleared
	staPPSFREQ = 0x0002 // enable PPS frequency discipline — never set
	staPPSTIME = 0x0004 // enable PPS time discipline — never set
	staFLL     = 0x0008 // select FLL mode — never set
	staINS     = 0x0010 // insert leap second at end of day
	staDEL     = 0x0020 // delete leap second at end of day
	staUNSYNC  = 0x0040 // clock unsynchronized
)

// disciplineBits are the status bits the daemon owns outright: they are
// cleared and rebuilt on every SetStatus so a previous daemon's kernel PLL
// or PPS settings cannot linger.
const disciplineBits = staPLL | staPPSFREQ | staPPSTIME | staFLL | staINS | staDEL | staUNSYNC

const (
	// maxFrequencyPPM is the largest frequency correction either kernel
	// accepts (MAXFREQ, 500 ppm).
	maxFrequencyPPM = 500.0

	// freqScale converts ppm to the kernel's scaled-ppm frequency word
	// (SHIFT_USEC = 16).
	freqScale = 65536.0

	// maxErrorMicros is the ceiling both kernels apply to maxerror and
	// esterror (MAXPHASE, 16 s in microseconds).
	maxErrorMicros = 16_000_000
)

// clampFrequency bounds a frequency correction to what the kernel accepts,
// so the caller knows exactly what was applied.
func clampFrequency(ppm float64) float64 {
	switch {
	case ppm > maxFrequencyPPM:
		return maxFrequencyPPM
	case ppm < -maxFrequencyPPM:
		return -maxFrequencyPPM
	}
	return ppm
}

// freqWord converts ppm to the kernel's scaled-ppm representation.
func freqWord(ppm float64) int64 {
	return int64(clampFrequency(ppm)*freqScale + 0.5*sign(ppm))
}

// freqPPM converts the kernel's scaled-ppm word to ppm.
func freqPPM(word int64) float64 {
	return float64(word) / freqScale
}

func sign(x float64) float64 {
	if x < 0 {
		return -1
	}
	return 1
}

// errorMicros converts an error bound to the kernel's microsecond units,
// clamped to [0, 16 s].
func errorMicros(d time.Duration) int64 {
	if d < 0 {
		return 0
	}
	us := d.Microseconds()
	if us > maxErrorMicros {
		return maxErrorMicros
	}
	return us
}

// statusWord rebuilds the discipline-owned status bits from s on top of the
// kernel's current status word, leaving the read-only and unrelated bits
// as they are.
func statusWord(current int32, s Status) int32 {
	w := current &^ disciplineBits
	if !s.Synced || s.Leap == ntp.LeapUnsync {
		w |= staUNSYNC
	}
	switch s.Leap {
	case ntp.LeapInsert:
		w |= staINS
	case ntp.LeapDelete:
		w |= staDEL
	}
	return w
}

// splitDuration returns d as whole seconds and a non-negative nanosecond
// remainder in [0, 1e9), the normal form both kernels expect.
func splitDuration(d time.Duration) (sec, nsec int64) {
	ns := d.Nanoseconds()
	sec = ns / 1e9
	nsec = ns % 1e9
	if nsec < 0 {
		nsec += 1e9
		sec--
	}
	return sec, nsec
}
