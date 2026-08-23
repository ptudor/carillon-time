// Package clock is the actuator boundary between the daemon and the kernel.
// The discipline loop decides; a Clock applies. Real implementations exist
// for Linux and FreeBSD, a stub returns ErrUnsupportedPlatform elsewhere, and
// Fake is a deterministic simulated clock for tests.
package clock

import (
	"errors"
	"time"

	"carillon/internal/ntp"
)

// ErrUnsupportedPlatform is returned by New on operating systems without a
// real clock backend. Every other feature of the daemon still works there.
var ErrUnsupportedPlatform = errors.New("clock: no system clock backend for this platform")

// Status is what the daemon tells the kernel about the clock's health. On
// both Linux and FreeBSD this drives the STA_UNSYNC flag (which gates the
// kernel's periodic RTC write-back), the leap-second flags, and the error
// bounds reported by ntp_gettime.
type Status struct {
	Synced   bool
	Leap     ntp.Leap
	MaxError time.Duration
	EstError time.Duration
}

// Clock is the system clock actuator. All methods are safe to call from a
// single goroutine; the engine is that goroutine.
type Clock interface {
	// Now returns the current CLOCK_REALTIME with nanosecond resolution.
	Now() time.Time

	// Monotonic returns seconds on a monotonic scale, for measuring
	// intervals. Its zero point is arbitrary.
	Monotonic() float64

	// Frequency returns the kernel's current frequency correction in ppm.
	Frequency() (float64, error)

	// SetFrequency sets the kernel frequency correction in ppm. Positive
	// values speed the clock up. Values beyond ±MaxFrequency are clamped by
	// the kernel; callers clamp first so they know what was applied.
	SetFrequency(ppm float64) error

	// Step adds delta to the clock. It is the only discontinuous operation
	// and is used solely under the configured step policy.
	Step(delta time.Duration) error

	// SetStatus updates the kernel's view of synchronization state.
	SetStatus(s Status) error

	// Precision is the measured clock resolution as a log2 exponent
	// (RFC 5905 precision), e.g. -20 for one microsecond.
	Precision() int8
}
