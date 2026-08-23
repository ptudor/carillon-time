//go:build freebsd

package clock

import (
	"errors"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// sysClock is the FreeBSD backend: ntp_adjtime(2) for frequency and status,
// clock_gettime(2)/clock_settime(2) for reads and steps.
type sysClock struct {
	precision int8
}

// ntpAdjtime is ntp_adjtime(2), which golang.org/x/sys/unix does not wrap
// on FreeBSD. It returns the kernel clock state (TIME_OK, TIME_ERROR, ...).
func ntpAdjtime(tx *timex) (state int, err error) {
	r1, _, errno := unix.Syscall(unix.SYS_NTP_ADJTIME, uintptr(unsafe.Pointer(tx)), 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(r1), nil
}

// clockSettime is clock_settime(2) for CLOCK_REALTIME, which
// golang.org/x/sys/unix does not wrap on FreeBSD; settimeofday(2) would lose
// the nanoseconds.
func clockSettime(ts *unix.Timespec) error {
	if _, _, errno := unix.Syscall(unix.SYS_CLOCK_SETTIME, uintptr(unix.CLOCK_REALTIME), uintptr(unsafe.Pointer(ts)), 0); errno != 0 {
		return errno
	}
	return nil
}

// New opens the FreeBSD system clock. It measures the clock's resolution and
// proves the process may adjust the clock by writing the current frequency
// back unchanged, so a missing privilege is reported before any source
// starts rather than at the first correction.
func New() (Clock, error) {
	c := &sysClock{}
	c.precision = measurePrecision(c.Now)

	var cur timex
	if _, err := ntpAdjtime(&cur); err != nil {
		return nil, fmt.Errorf("clock: ntp_adjtime read: %w", err)
	}
	probe := timex{Modes: modFrequency, Freq: cur.Freq}
	if _, err := ntpAdjtime(&probe); err != nil {
		if errors.Is(err, unix.EPERM) {
			return nil, fmt.Errorf("clock: ntp_adjtime write: %w (carillon needs PRIV_NTP_ADJTIME: run as root, or load mac_ntpd(4) with security.mac.ntpd.uid set to carillon's uid)", err)
		}
		return nil, fmt.Errorf("clock: ntp_adjtime write: %w", err)
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

// Frequency implements Clock. A TIME_ERROR return state only means the
// clock is flagged unsynchronized; the frequency word is valid.
func (c *sysClock) Frequency() (float64, error) {
	var tx timex
	if _, err := ntpAdjtime(&tx); err != nil {
		return 0, fmt.Errorf("clock: ntp_adjtime read frequency: %w", err)
	}
	return freqPPM(tx.Freq), nil
}

// SetFrequency implements Clock.
func (c *sysClock) SetFrequency(ppm float64) error {
	tx := timex{Modes: modFrequency, Freq: freqWord(ppm)}
	if _, err := ntpAdjtime(&tx); err != nil {
		return fmt.Errorf("clock: ntp_adjtime set frequency %.3f ppm: %w", clampFrequency(ppm), err)
	}
	return nil
}

// Step implements Clock. FreeBSD has no atomic add-offset call, so the clock
// is read and written back-to-back; the few hundred nanoseconds between the
// two are absorbed by the discipline loop.
func (c *sysClock) Step(delta time.Duration) error {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_REALTIME, &ts); err != nil {
		return fmt.Errorf("clock: clock_gettime before step: %w", err)
	}
	sec, nsec := splitDuration(delta)
	ts.Sec += sec
	ts.Nsec += nsec
	if ts.Nsec >= 1e9 {
		ts.Nsec -= 1e9
		ts.Sec++
	}
	if err := clockSettime(&ts); err != nil {
		return fmt.Errorf("clock: clock_settime step %v: %w", delta, err)
	}
	return nil
}

// SetStatus implements Clock. The kernel's PLL, FLL and PPS bits are always
// cleared so the kernel never disciplines the clock behind the daemon's
// back; STA_UNSYNC and the leap bits follow s. maxerror and esterror are in
// microseconds.
func (c *sysClock) SetStatus(s Status) error {
	var cur timex
	if _, err := ntpAdjtime(&cur); err != nil {
		return fmt.Errorf("clock: ntp_adjtime read status: %w", err)
	}
	tx := timex{
		Modes:    modStatus | modMaxerror | modEsterror,
		Status:   statusWord(cur.Status, s),
		Maxerror: errorMicros(s.MaxError),
		Esterror: errorMicros(s.EstError),
	}
	if _, err := ntpAdjtime(&tx); err != nil {
		return fmt.Errorf("clock: ntp_adjtime set status (synced=%v leap=%v): %w", s.Synced, s.Leap, err)
	}
	return nil
}

// Precision implements Clock.
func (c *sysClock) Precision() int8 { return c.precision }

var _ Clock = (*sysClock)(nil)
