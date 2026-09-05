package clock

import (
	"sync"
	"time"
)

// Fake is a deterministic simulated clock for tests. It models a local
// clock with an intrinsic frequency error (Drift) on top of which the
// daemon's SetFrequency corrections are applied, and it records every step
// and frequency change so tests can assert on the actuator's behaviour.
//
// Time only moves when Advance is called; Advance's argument is *true*
// elapsed time, and the fake's wall clock advances by that scaled by
// (1 + (Drift + freq) ppm).
type Fake struct {
	mu        sync.Mutex
	trueTime  time.Time
	wall      time.Time
	mono      float64
	drift     float64 // intrinsic error, ppm
	freq      float64 // applied correction, ppm
	precision int8
	status    Status

	// Steps and Frequencies record every actuator call in order.
	Steps       []time.Duration
	Frequencies []float64
	Statuses    []Status
}

// NewFake returns a fake whose true and local time both start at start.
func NewFake(start time.Time) *Fake {
	return &Fake{trueTime: start, wall: start, precision: -20}
}

// SetDrift sets the intrinsic frequency error of the simulated hardware in
// ppm (positive = runs fast).
func (f *Fake) SetDrift(ppm float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drift = ppm
}

// SetLocalOffset moves the local clock so that it is behind true time by
// offset (a positive offset means the local clock reads earlier than true
// time, matching the sign convention of Measurement.Offset).
func (f *Fake) SetLocalOffset(offset time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wall = f.trueTime.Add(-offset)
}

// Advance moves true time forward by d and the local clock by d scaled by
// its current total frequency error.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trueTime = f.trueTime.Add(d)
	scaled := time.Duration(float64(d) * (1 + (f.drift+f.freq)*1e-6))
	f.wall = f.wall.Add(scaled)
	f.mono += d.Seconds()
}

// TrueTime returns the simulated true time.
func (f *Fake) TrueTime() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.trueTime
}

// Offset returns true time minus local time in seconds.
func (f *Fake) Offset() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.trueTime.Sub(f.wall).Seconds()
}

// Now implements Clock.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wall
}

// Monotonic implements Clock.
func (f *Fake) Monotonic() float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mono
}

// Frequency implements Clock.
func (f *Fake) Frequency() (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.freq, nil
}

// SetFrequency implements Clock. Like the real backends it refuses a
// non-finite word, so a test that lets one through fails here rather than
// recording an arbitrary frequency.
func (f *Fake) SetFrequency(ppm float64) error {
	if err := CheckFrequency(ppm); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if ppm > 500 {
		ppm = 500
	} else if ppm < -500 {
		ppm = -500
	}
	f.freq = ppm
	f.Frequencies = append(f.Frequencies, ppm)
	return nil
}

// Step implements Clock.
func (f *Fake) Step(delta time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wall = f.wall.Add(delta)
	f.Steps = append(f.Steps, delta)
	return nil
}

// SetStatus implements Clock.
func (f *Fake) SetStatus(s Status) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = s
	f.Statuses = append(f.Statuses, s)
	return nil
}

// Status returns the most recently set status.
func (f *Fake) Status() Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

// Precision implements Clock.
func (f *Fake) Precision() int8 { return f.precision }

// SetPrecision overrides the reported precision.
func (f *Fake) SetPrecision(p int8) { f.precision = p }

var _ Clock = (*Fake)(nil)
