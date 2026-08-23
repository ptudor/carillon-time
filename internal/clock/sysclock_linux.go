//go:build linux

package clock

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// The status bits in sysclock_common.go must match the kernel's. A mismatch
// makes one of these constants negative, which does not convert to uint.
const (
	_ uint = uint(unix.STA_PLL-staPLL) + uint(staPLL-unix.STA_PLL)
	_ uint = uint(unix.STA_PPSFREQ-staPPSFREQ) + uint(staPPSFREQ-unix.STA_PPSFREQ)
	_ uint = uint(unix.STA_PPSTIME-staPPSTIME) + uint(staPPSTIME-unix.STA_PPSTIME)
	_ uint = uint(unix.STA_FLL-staFLL) + uint(staFLL-unix.STA_FLL)
	_ uint = uint(unix.STA_INS-staINS) + uint(staINS-unix.STA_INS)
	_ uint = uint(unix.STA_DEL-staDEL) + uint(staDEL-unix.STA_DEL)
	_ uint = uint(unix.STA_UNSYNC-staUNSYNC) + uint(staUNSYNC-unix.STA_UNSYNC)
)

// timexInt is the set of integer widths unix.Timex fields have across
// architectures: 64-bit on amd64/arm64 and friends, 32-bit on arm/mips.
type timexInt interface {
	~int32 | ~int64
}

func setField[T timexInt](dst *T, v int64) { *dst = T(v) }

func getField[T timexInt](v T) int64 { return int64(v) }

// sysClock is the Linux backend: adjtimex(2) for frequency, status and
// atomic offset steps; clock_gettime(2) for reads.
type sysClock struct {
	precision int8
}

// New opens the Linux system clock. It measures the clock's resolution and
// proves the process may adjust the clock by writing the current frequency
// back unchanged, so a missing CAP_SYS_TIME is reported before any source
// starts rather than at the first correction.
func New() (Clock, error) {
	c := &sysClock{}
	c.precision = measurePrecision(c.Now)

	var cur unix.Timex
	if _, err := unix.Adjtimex(&cur); err != nil {
		return nil, fmt.Errorf("clock: adjtimex read: %w", err)
	}
	probe := unix.Timex{Modes: unix.ADJ_FREQUENCY}
	setField(&probe.Freq, getField(cur.Freq))
	if _, err := unix.Adjtimex(&probe); err != nil {
		if errors.Is(err, unix.EPERM) {
			return nil, fmt.Errorf("clock: adjtimex write: %w (carillon needs CAP_SYS_TIME: run as root or grant AmbientCapabilities=CAP_SYS_TIME)", err)
		}
		return nil, fmt.Errorf("clock: adjtimex write: %w", err)
	}
	return c, nil
}

// Now implements Clock.
func (c *sysClock) Now() time.Time {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_REALTIME, &ts); err != nil {
		// Only EINVAL (bad clock id) or EFAULT (bad pointer) are defined,
		// and neither is possible with a constant id and a stack address.
		panic(fmt.Sprintf("clock: clock_gettime(CLOCK_REALTIME): %v", err))
	}
	return time.Unix(int64(ts.Sec), int64(ts.Nsec)) // int32 fields on 32-bit Linux
}

// Monotonic implements Clock.
func (c *sysClock) Monotonic() float64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		panic(fmt.Sprintf("clock: clock_gettime(CLOCK_MONOTONIC): %v", err))
	}
	return float64(ts.Sec) + float64(ts.Nsec)*1e-9
}

// Frequency implements Clock. The kernel's TIME_ERROR return state only
// means the clock is flagged unsynchronized; the frequency word is valid.
func (c *sysClock) Frequency() (float64, error) {
	var tx unix.Timex
	if _, err := unix.Adjtimex(&tx); err != nil {
		return 0, fmt.Errorf("clock: adjtimex read frequency: %w", err)
	}
	return freqPPM(getField(tx.Freq)), nil
}

// SetFrequency implements Clock.
func (c *sysClock) SetFrequency(ppm float64) error {
	tx := unix.Timex{Modes: unix.ADJ_FREQUENCY}
	setField(&tx.Freq, freqWord(ppm))
	if _, err := unix.Adjtimex(&tx); err != nil {
		return fmt.Errorf("clock: adjtimex set frequency %.3f ppm: %w", clampFrequency(ppm), err)
	}
	return nil
}

// Step implements Clock using ADJ_SETOFFSET, which adds the offset to the
// clock atomically with no read-modify-write window.
func (c *sysClock) Step(delta time.Duration) error {
	sec, nsec := splitDuration(delta)
	tx := unix.Timex{Modes: unix.ADJ_SETOFFSET | unix.ADJ_NANO}
	setField(&tx.Time.Sec, sec)
	setField(&tx.Time.Usec, nsec) // nanoseconds under ADJ_NANO
	if _, err := unix.Adjtimex(&tx); err != nil {
		return fmt.Errorf("clock: adjtimex step %v: %w", delta, err)
	}
	return nil
}

// SetStatus implements Clock. The kernel's PLL, FLL and PPS bits are always
// cleared so the kernel never disciplines the clock behind the daemon's
// back; STA_UNSYNC and the leap bits follow s. maxerror and esterror are in
// microseconds regardless of STA_NANO.
func (c *sysClock) SetStatus(s Status) error {
	var cur unix.Timex
	if _, err := unix.Adjtimex(&cur); err != nil {
		return fmt.Errorf("clock: adjtimex read status: %w", err)
	}
	tx := unix.Timex{
		Modes:  unix.ADJ_STATUS | unix.ADJ_MAXERROR | unix.ADJ_ESTERROR,
		Status: statusWord(cur.Status, s),
	}
	setField(&tx.Maxerror, errorMicros(s.MaxError))
	setField(&tx.Esterror, errorMicros(s.EstError))
	if _, err := unix.Adjtimex(&tx); err != nil {
		return fmt.Errorf("clock: adjtimex set status (synced=%v leap=%v): %w", s.Synced, s.Leap, err)
	}
	return nil
}

// Precision implements Clock.
func (c *sysClock) Precision() int8 { return c.precision }

var _ Clock = (*sysClock)(nil)
