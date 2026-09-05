package refclock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"carillon/internal/clock"
	"carillon/internal/discipline"
	"carillon/internal/ntp"
	"carillon/internal/serial"
	"carillon/internal/source"
)

const (
	nmeaPoll        = int8(4)
	nmeaWindow      = 16
	nmeaReadTimeout = 500 * time.Millisecond
)

type serialReader interface {
	ReadTimeout([]byte, time.Duration) (int, error)
	Close() error
}

// PulseTracker shares the most recent kernel PPS timestamp with the NMEA
// logical source without putting either source on the other's hot path.
type PulseTracker struct{ unixNano atomic.Int64 }

// Observe records one accepted PPS edge.
func (p *PulseTracker) Observe(t time.Time) {
	if p != nil && !t.IsZero() {
		p.unixNano.Store(t.UnixNano())
	}
}

func (p *PulseTracker) latest() time.Time {
	if p == nil {
		return time.Time{}
	}
	n := p.unixNano.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// NMEAConfig configures the serial, sentence-bearing half of a GPS receiver.
type NMEAConfig struct {
	Name      string
	Device    string
	Baud      int
	Offset    float64
	Sentences []string
	BuildTime time.Time
	Pulse     *PulseTracker

	// Generation returns the engine's measurement epoch. It is captured
	// with the arrival timestamp of a sentence's '$' and checked again
	// before that sentence's offset enters the window, because the offset
	// is the sentence's own time minus that arrival reading. Nil disables
	// the check.
	Generation func() uint64
}

// NMEA converts checksum-valid RMC/ZDA sentences into a robust GPS time
// source. Mutable framing/filter state belongs to Run; Info is atomic.
type NMEA struct {
	cfg NMEAConfig
	clk clock.Clock
	log *slog.Logger

	reader serialReader
	opener func() (serialReader, error)

	resetRequested atomic.Bool
	info           atomic.Pointer[source.Info]

	reach       uint8
	offsets     []float64
	lags        []float64
	line        []byte
	lineWall    time.Time
	lineMono    float64
	lineGen     uint64
	staleSeen   uint64
	haveLine    bool
	haveSlot    bool
	slotAt      float64
	lastStamp   time.Time
	lastZDASeen float64
}

// NewNMEA validates cfg and opens the receiver before any source goroutine is
// started, making serial configuration and permission failures fatal early.
func NewNMEA(cfg NMEAConfig, clk clock.Clock, log *slog.Logger) (*NMEA, error) {
	if err := defaultAndValidateNMEA(&cfg, clk); err != nil {
		return nil, err
	}
	opener := func() (serialReader, error) { return serial.OpenNMEA(cfg.Device, cfg.Baud) }
	r, err := opener()
	if err != nil {
		return nil, fmt.Errorf("refclock %q: %w", cfg.Name, err)
	}
	return newNMEA(cfg, clk, log, r, opener), nil
}

func newNMEA(cfg NMEAConfig, clk clock.Clock, log *slog.Logger, r serialReader, opener func() (serialReader, error)) *NMEA {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	n := &NMEA{
		cfg: cfg, clk: clk, log: log.With("source", cfg.Name), reader: r, opener: opener,
		offsets: make([]float64, 0, nmeaWindow), lags: make([]float64, 0, nmeaWindow),
		line: make([]byte, 0, 128),
	}
	n.info.Store(&source.Info{
		Name: cfg.Name, Address: cfg.Device, Poll: nmeaPoll,
		Refclock: &source.RefclockInfo{Type: "gps-nmea", Device: cfg.Device},
	})
	return n
}

func defaultAndValidateNMEA(cfg *NMEAConfig, clk clock.Clock) error {
	if cfg.Name == "" {
		return errors.New("NMEA refclock name is empty")
	}
	if cfg.Device == "" {
		return fmt.Errorf("refclock %q: device is empty", cfg.Name)
	}
	switch cfg.Baud {
	case 4800, 9600, 19200, 38400, 57600, 115200:
	default:
		return fmt.Errorf("refclock %q: unsupported baud %d", cfg.Name, cfg.Baud)
	}
	if !finite(cfg.Offset) {
		return fmt.Errorf("refclock %q: NMEA offset must be finite", cfg.Name)
	}
	if len(cfg.Sentences) == 0 {
		cfg.Sentences = []string{"RMC", "ZDA"}
	}
	for _, kind := range cfg.Sentences {
		if kind != "RMC" && kind != "ZDA" {
			return fmt.Errorf("refclock %q: unsupported NMEA sentence %q", cfg.Name, kind)
		}
	}
	if clk == nil {
		return fmt.Errorf("refclock %q: nil clock", cfg.Name)
	}
	return nil
}

// generation reads the engine's measurement epoch, or 0 when the source was
// built without one.
func (n *NMEA) generation() uint64 {
	if n.cfg.Generation == nil {
		return 0
	}
	return n.cfg.Generation()
}

func (n *NMEA) Name() string      { return n.cfg.Name }
func (n *NMEA) Info() source.Info { return *n.info.Load() }
func (n *NMEA) Reset()            { n.resetRequested.Store(true) }

// Close releases the receiver before Run starts, for constructor unwinding.
func (n *NMEA) Close() error {
	if n == nil || n.reader == nil {
		return nil
	}
	err := n.reader.Close()
	n.reader = nil
	return err
}

func (n *NMEA) Run(ctx context.Context, out chan<- discipline.Measurement) error {
	defer n.closeReader()
	buf := make([]byte, 2048)
	for {
		if n.resetRequested.Swap(false) {
			n.resetWindow()
		}
		read, err := n.reader.ReadTimeout(buf, nmeaReadTimeout)
		switch {
		case err == nil:
			wall, mono := n.clk.Now(), n.clk.Monotonic()
			for _, m := range n.consume(buf[:read], wall, mono) {
				if !n.emit(ctx, out, m) {
					return nil
				}
			}
			if m, ok := n.tick(mono); ok && !n.emit(ctx, out, m) {
				return nil
			}
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, serial.ErrTimeout):
			if m, ok := n.tick(n.clk.Monotonic()); ok && !n.emit(ctx, out, m) {
				return nil
			}
		default:
			n.updateInfo(func(i *source.Info, _ *source.RefclockInfo) { i.LastError = err.Error() })
			n.log.Warn("GPS serial device unavailable", "error", err)
			if err := n.reopen(ctx); err != nil {
				return err
			}
		}
	}
}

func (n *NMEA) reopen(ctx context.Context) error {
	n.closeReader()
	if n.opener == nil {
		return fmt.Errorf("refclock %q: serial reader stopped and cannot be reopened", n.cfg.Name)
	}
	backoff := time.Second
	for {
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		r, err := n.opener()
		if err == nil {
			n.reader = r
			n.reach = 0
			n.haveLine, n.haveSlot = false, false
			n.resetWindow()
			n.updateInfo(func(i *source.Info, _ *source.RefclockInfo) { i.LastError, i.Reach = "", 0 })
			n.log.Info("GPS serial device reopened")
			return nil
		}
		n.updateInfo(func(i *source.Info, _ *source.RefclockInfo) { i.LastError = err.Error() })
		if backoff < time.Minute {
			backoff *= 2
			if backoff > time.Minute {
				backoff = time.Minute
			}
		}
	}
}

func (n *NMEA) closeReader() {
	if n.reader != nil {
		_ = n.reader.Close()
		n.reader = nil
	}
}

// consume frames sentences while preserving the wall/monotonic timestamp of
// the read that delivered each '$' start character.
func (n *NMEA) consume(chunk []byte, wall time.Time, mono float64) []discipline.Measurement {
	var out []discipline.Measurement
	for _, b := range chunk {
		switch {
		case b == '$':
			n.line = append(n.line[:0], b)
			n.lineWall, n.lineMono, n.haveLine = wall, mono, true
			n.lineGen = n.generation()
		case !n.haveLine:
			continue
		case b == '\n' || b == '\r':
			if len(n.line) != 0 {
				if m, ok := n.acceptLine(string(n.line), n.lineWall, n.lineMono); ok {
					out = append(out, m)
				}
			}
			n.haveLine = false
		case len(n.line) >= maxNMEALine:
			n.haveLine = false
			n.reject("NMEA sentence exceeds maximum length")
		default:
			n.line = append(n.line, b)
		}
	}
	return out
}

func (n *NMEA) acceptLine(line string, arrival time.Time, mono float64) (discipline.Measurement, bool) {
	kind := sentenceKind(line)
	if kind != "RMC" && kind != "ZDA" && kind != "GGA" {
		return discipline.Measurement{}, false
	}
	s, err := parseNMEA(line)
	if err != nil {
		n.reject(err.Error())
		return discipline.Measurement{}, false
	}
	n.updateInfo(func(i *source.Info, r *source.RefclockInfo) {
		r.Sentence, r.LastSentence = s.Kind, arrival
		i.LastError = ""
		if s.Kind == "RMC" {
			r.FixKnown, r.FixValid = true, s.Valid
		}
		if s.Kind == "GGA" {
			r.FixKnown, r.FixValid = true, s.Valid
			r.FixQuality, r.Satellites = s.FixQuality, s.Satellites
		}
	})
	if s.Kind == "GGA" {
		return discipline.Measurement{}, false
	}
	if !n.accepts(s.Kind) || !s.Valid {
		if !s.Valid {
			n.reject("GPS fix is invalid")
		}
		return discipline.Measurement{}, false
	}
	if s.Fractional {
		n.reject("GPS time sentence is not aligned to a whole second")
		return discipline.Measurement{}, false
	}
	if !n.cfg.BuildTime.IsZero() {
		buildDate := time.Date(n.cfg.BuildTime.Year(), n.cfg.BuildTime.Month(), n.cfg.BuildTime.Day(), 0, 0, 0, 0, time.UTC)
		if s.Timestamp.Before(buildDate) {
			n.reject("GPS date predates this build; suspected GPS week rollover")
			return discipline.Measurement{}, false
		}
	}
	if s.Kind == "ZDA" {
		n.lastZDASeen = mono
	} else if n.accepts("ZDA") && n.lastZDASeen != 0 && mono-n.lastZDASeen < 2.5 {
		return discipline.Measurement{}, false
	}
	if !n.lastStamp.IsZero() && !s.Timestamp.After(n.lastStamp) {
		return discipline.Measurement{}, false
	}
	if now := n.generation(); n.lineGen != 0 && now != n.lineGen {
		// The clock was stepped between the '$' that fixed `arrival` and
		// this sentence being complete, so the offset below would be wrong
		// by the step. Drop the sentence and the window built with it.
		n.staleSeen++
		n.resetWindow()
		n.updateInfo(func(i *source.Info, _ *source.RefclockInfo) { i.Stale++ })
		n.log.Info("discarding a sentence that spans a clock step", "count", n.staleSeen, "generation", now)
		return discipline.Measurement{}, false
	}
	n.lastStamp = s.Timestamp

	offset := s.Timestamp.Sub(arrival).Seconds() + n.cfg.Offset
	n.appendValue(&n.offsets, offset)
	median, mad := medianMAD(n.offsets)
	sigma := mad * 1.4826
	precision := ntp.Log2Seconds(n.clk.Precision())
	n.noteArrival(mono)

	if pulse := n.cfg.Pulse.latest(); !pulse.IsZero() {
		lag := arrival.Sub(pulse).Seconds()
		if lag >= 0 && lag < 2 {
			n.appendValue(&n.lags, lag)
			lagMedian, _ := medianMAD(n.lags)
			n.updateInfo(func(_ *source.Info, r *source.RefclockInfo) {
				r.MeasuredLag, r.LagSamples = lagMedian, len(n.lags)
			})
		}
	}

	m := discipline.Measurement{
		Source: n.cfg.Name, Now: mono, Reach: n.reach, Poll: nmeaPoll,
		Generation: n.lineGen,
		Valid:      len(n.offsets) >= 4, At: mono, Offset: median, Delay: 0,
		Dispersion: sigma + precision, Jitter: math.Max(sigma, precision),
		Leap: ntp.LeapNone, Stratum: 0, RefID: ntp.RefIDFromString("GPS"),
		SourceRefID: ntp.RefIDFromString("GPS"), Precision: n.clk.Precision(), RefTime: s.Timestamp,
	}
	n.updateInfo(func(i *source.Info, r *source.RefclockInfo) {
		i.Reach, i.LastRx, i.LastError, i.Received = n.reach, arrival, "", i.Received+1
		i.Offset, i.Delay, i.Dispersion, i.Jitter = median, 0, m.Dispersion, m.Jitter
		i.Stratum, i.RefID, i.Leap = 0, m.RefID, m.Leap
		r.WindowSamples, r.WindowJitter, r.Stable = len(n.offsets), sigma, m.Valid
		r.Samples++
	})
	return m, true
}

func sentenceKind(line string) string {
	line = strings.TrimSpace(line)
	if len(line) < 6 || line[0] != '$' {
		return ""
	}
	return line[3:6]
}

func (n *NMEA) accepts(kind string) bool { return slices.Contains(n.cfg.Sentences, kind) }

func (n *NMEA) noteArrival(now float64) {
	if !n.haveSlot {
		n.reach, n.haveSlot, n.slotAt = 1, true, now
		return
	}
	slots := int(math.Floor(now - n.slotAt + 0.5))
	if slots < 1 {
		n.reach |= 1
		return
	}
	shiftReach(&n.reach, slots)
	n.reach |= 1
	if slots > 1 {
		n.addTimeouts(slots - 1)
	}
	n.slotAt = now
}

func (n *NMEA) tick(now float64) (discipline.Measurement, bool) {
	if !n.haveSlot {
		n.haveSlot, n.slotAt = true, now
		return discipline.Measurement{}, false
	}
	slots := int(math.Floor(now - n.slotAt - 0.5))
	if slots < 1 {
		return discipline.Measurement{}, false
	}
	shiftReach(&n.reach, slots)
	n.slotAt += float64(slots)
	n.addTimeouts(slots)
	n.updateInfo(func(i *source.Info, _ *source.RefclockInfo) {
		i.Reach = n.reach
		i.LastError = "NMEA timeout"
	})
	return n.emptyMeasurement(), true
}

func (n *NMEA) addTimeouts(count int) {
	n.updateInfo(func(i *source.Info, r *source.RefclockInfo) {
		i.Timeouts += uint64(count)
		r.Timeouts += uint64(count)
	})
}

func (n *NMEA) reject(message string) {
	n.updateInfo(func(i *source.Info, _ *source.RefclockInfo) {
		i.Bogus++
		i.LastError = message
	})
}

func (n *NMEA) appendValue(dst *[]float64, value float64) {
	if len(*dst) == nmeaWindow {
		copy(*dst, (*dst)[1:])
		(*dst)[len(*dst)-1] = value
		return
	}
	*dst = append(*dst, value)
}

func (n *NMEA) resetWindow() {
	n.offsets = n.offsets[:0]
	n.updateInfo(func(_ *source.Info, r *source.RefclockInfo) {
		r.WindowSamples, r.WindowJitter, r.Stable = 0, 0, false
	})
}

func (n *NMEA) emptyMeasurement() discipline.Measurement {
	return discipline.Measurement{
		Source: n.cfg.Name, Now: n.clk.Monotonic(), Reach: n.reach, Poll: nmeaPoll,
		Generation: n.generation(),
	}
}

func (n *NMEA) emit(ctx context.Context, out chan<- discipline.Measurement, m discipline.Measurement) bool {
	select {
	case out <- m:
		return true
	case <-ctx.Done():
		return false
	}
}

func (n *NMEA) updateInfo(f func(*source.Info, *source.RefclockInfo)) {
	cur := *n.info.Load()
	r := *cur.Refclock
	f(&cur, &r)
	cur.Refclock = &r
	n.info.Store(&cur)
}
