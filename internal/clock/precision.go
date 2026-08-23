package clock

import (
	"time"

	"carillon/internal/ntp"
)

// precisionSamples is the number of back-to-back clock reads used to
// measure the clock's resolution.
const precisionSamples = 1000

// measurePrecision estimates the resolution of the clock read by now as an
// RFC 5905 precision exponent: the log2 of the smallest non-zero difference
// between successive readings. A clock whose readings never differ over the
// sample window (which cannot happen on real hardware) reports the finest
// value the field allows.
func measurePrecision(now func() time.Time) int8 {
	var minDelta time.Duration
	prev := now()
	for i := 0; i < precisionSamples; i++ {
		cur := now()
		if d := cur.Sub(prev); d > 0 && (minDelta == 0 || d < minDelta) {
			minDelta = d
		}
		prev = cur
	}
	return ntp.PrecisionFromSeconds(minDelta.Seconds())
}
