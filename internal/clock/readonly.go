package clock

import (
	"errors"
	"fmt"
	"time"

	"github.com/ptudor/carillon-time/internal/ntp"
)

// SynchronizedUTC reads the kernel NTP state with a modes-zero query. During
// first-install preparation the incumbent daemon must report TIME_OK. An
// unsynchronized clock or an in-progress leap cannot authorize a cold cache.
func SynchronizedUTC() (time.Time, error) {
	state, err := kernelTimeState()
	if err != nil {
		return time.Time{}, fmt.Errorf("clock: read kernel synchronization state: %w", err)
	}
	if state != 0 {
		return time.Time{}, fmt.Errorf("clock: existing time service must report TIME_OK (kernel state %d)", state)
	}
	return time.Now().UTC(), nil
}

// ErrReadOnly is returned by the mutating methods of a read-only clock.
var ErrReadOnly = errors.New("clock: read-only clock cannot be adjusted")

// ReadOnly returns a Clock that reads the real system clock but refuses to
// adjust it. It exists for `carillon query` and for running the client side
// on hosts without a backend (the development Mac); the engine never uses
// it. Its precision is measured, like the real backends.
func ReadOnly() Clock {
	return &readOnly{precision: measurePrecision(time.Now), start: time.Now()}
}

type readOnly struct {
	precision int8
	start     time.Time
}

func (r *readOnly) Now() time.Time              { return time.Now() }
func (r *readOnly) Monotonic() float64          { return time.Since(r.start).Seconds() }
func (r *readOnly) Frequency() (float64, error) { return 0, ErrReadOnly }
func (r *readOnly) SetFrequency(float64) error  { return ErrReadOnly }
func (r *readOnly) Step(time.Duration) error    { return ErrReadOnly }
func (r *readOnly) SetStatus(Status) error      { return ErrReadOnly }
func (r *readOnly) Precision() int8             { return r.precision }

var _ Clock = (*readOnly)(nil)

// precisionOf is a convenience for callers that only have a log2 precision
// and want it in seconds.
func precisionOf(p int8) float64 { return ntp.Log2Seconds(p) }
