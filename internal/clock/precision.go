package clock

import (
	"slices"
	"time"

	"carillon/internal/ntp"
)

const (
	// precisionSamples is the number of observed clock increments the
	// estimate is taken over.
	precisionSamples = 1000

	// precisionMaxReads bounds the total clock reads, so a coarse clock
	// (a VM whose CLOCK_REALTIME ticks in milliseconds) costs a bounded
	// few milliseconds at startup instead of spinning for its resolution
	// times precisionSamples.
	precisionMaxReads = 200000
)

// measurePrecision estimates the resolution of the clock read by now as an
// RFC 5905 precision exponent.
//
// It measures the clock's *typical increment* — the median gap between two
// readings that actually differ — which is what RFC 5905 §7.3 and ntpd mean
// by precision. The smallest observed difference is a different quantity:
// two back-to-back clock_gettime calls on a TSC-backed clock can differ by a
// single nanosecond, so a minimum reports 2^-30 on any modern host whatever
// its real resolution, and every floor derived from it — the filter's jitter
// floor, the loop's popcorn threshold, the negative-delay tolerance in an
// exchange, and the precision advertised to clients — becomes a number no
// hardware delivers.
//
// A clock whose readings never differ at all (which cannot happen on real
// hardware) reports the finest value the field allows.
func measurePrecision(now func() time.Time) int8 {
	deltas := make([]time.Duration, 0, precisionSamples)
	prev := now()
	for reads := 0; reads < precisionMaxReads && len(deltas) < precisionSamples; reads++ {
		cur := now()
		if d := cur.Sub(prev); d > 0 {
			deltas = append(deltas, d)
		}
		prev = cur
	}
	if len(deltas) == 0 {
		return ntp.PrecisionFromSeconds(0)
	}
	slices.Sort(deltas)
	return ntp.PrecisionFromSeconds(deltas[len(deltas)/2].Seconds())
}
