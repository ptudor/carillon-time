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

	// nmeaEraJump is the largest forward step in receiver-reported time that
	// is still ordinary progression rather than a change of era. A receiver
	// reporting once a second cannot legitimately advance by an hour between
	// two sentences.
	nmeaEraJump = time.Hour

	// nmeaEraConfirm is how many consecutive, self-consistent sentences a
	// disagreeing era must produce before it is believed.
	nmeaEraConfirm = 4
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

	reach    uint8
	offsets  []float64
	lags     []float64
	line     []byte
	lineWall time.Time
	lineMono float64
	lineGen  uint64

	// eraCandidate and eraCount accumulate evidence for a receiver time era
	// that disagrees with lastStamp. See chronologyOK.
	eraCandidate time.Time
	eraCount     int

	// pendingInvalidate is armed by resetWindow and carried on the next
	// emitted measurement, so the selector drops the estimate this source
	// no longer has.
	pendingInvalidate bool
	staleSeen         uint64
	haveLine          bool
	haveSlot          bool
	slotAt            float64
	lastStamp         time.Time
	lastZDASeen       float64
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
		if ctx.Err() != nil {
			return nil
		}
		// Belt and braces behind reopen's contract: the loop must never
		// dereference a reader it does not have (RA6X-018).
		if n.reader == nil {
			return fmt.Errorf("refclock %q: no serial reader", n.cfg.Name)
		}
		if n.resetRequested.Swap(false) {
			n.resetWindow()
		}
		read, err := n.reader.ReadTimeout(buf, nmeaReadTimeout)
		switch {
		case err == nil:
			// The epoch is captured around the clock read that fixes the
			// arrival time, not later at byte framing: reading the arrival
			// first and the epoch afterwards let a step land in between and
			// stamp a pre-step arrival with the post-step epoch (RA6X-006).
			// A settled epoch that did not change across the read is the
			// only one this chunk may be attributed to.
			gen := n.generation()
			wall, mono := n.clk.Now(), n.clk.Monotonic()
			if after := n.generation(); after != gen || (gen != 0 && !source.StableEpoch(gen)) {
				n.discardChunk(after)
				continue
			}
			for _, m := range n.consume(buf[:read], wall, mono, gen) {
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
			// A hard device error is a loss, not a quiet receiver: report it
			// before disappearing into the reconnect loop, or an old NMEA
			// estimate keeps numbering a live PPS while the GPS is
			// unplugged (RA6X-004).
			n.updateInfo(func(i *source.Info, _ *source.RefclockInfo) { i.LastError = err.Error() })
			n.log.Warn("GPS serial device unavailable", "error", err)
			n.deviceLost()
			if !n.emit(ctx, out, n.emptyMeasurement()) {
				return nil
			}
			if err := n.reopen(ctx); err != nil {
				// Cancellation is a clean stop, not a source failure.
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

// deviceLost revokes everything derived from a device that has gone away:
// reach empties, the estimate is invalidated, and the framing, slot and
// chronology state is cleared so the replacement device has to prime from
// scratch. LastError is left as the caller set it, so health keeps explaining
// the outage while the reconnect loop runs.
func (n *NMEA) deviceLost() {
	n.reach = 0
	n.haveLine, n.haveSlot = false, false
	n.lastZDASeen = 0
	n.resetWindow()
	n.updateInfo(func(i *source.Info, _ *source.RefclockInfo) { i.Reach = 0 })
}

// reopen closes the current reader and retries the opener with exponential
// backoff until it succeeds or ctx is cancelled. It returns nil only when
// n.reader holds a new, open device: returning nil after cancellation left
// the reader nil and made Run's next iteration dereference it, panicking in
// a source goroutine and taking the whole daemon down without restoring the
// base frequency or flushing statistics (RA6X-018).
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
			return ctx.Err()
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

// discardChunk drops a read whose arrival timestamp spans a clock
// discontinuity, along with the partial sentence and the offset window built
// against the old epoch.
func (n *NMEA) discardChunk(now uint64) {
	n.staleSeen++
	n.haveLine = false
	n.resetWindow()
	n.updateInfo(func(i *source.Info, _ *source.RefclockInfo) { i.Stale++ })
	n.log.Info("discarding serial data that spans a clock step", "count", n.staleSeen, "generation", now)
}

// consume frames sentences while preserving the wall/monotonic timestamp of
// the read that delivered each '$' start character, and the clock epoch that
// read was taken in.
func (n *NMEA) consume(chunk []byte, wall time.Time, mono float64, gen uint64) []discipline.Measurement {
	var out []discipline.Measurement
	for _, b := range chunk {
		switch {
		case b == '$':
			n.line = append(n.line[:0], b)
			n.lineWall, n.lineMono, n.haveLine = wall, mono, true
			n.lineGen = gen
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
	if !n.chronologyOK(s.Timestamp) {
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
	// time.Time.Sub saturates at ±(1<<63 - 1) ns rather than reporting an
	// overflow, so a sentence dated far enough from the local clock would
	// yield an offset of about ±292 years that reads like a real
	// measurement. Detect the saturation and drop the sentence (RA6X-041).
	lag := s.Timestamp.Sub(arrival)
	if lag == math.MaxInt64 || lag == math.MinInt64 {
		n.reject("GPS timestamp is too far from the local clock to measure")
		return discipline.Measurement{}, false
	}
	offset := lag.Seconds() + n.cfg.Offset

	// Reach and freshness are updated before this sample joins the window,
	// so a sentence arriving after a long gap drops the historical window
	// and then primes the fresh one, rather than being appended to a window
	// that is discarded a moment later (RA6X-019).
	n.noteArrival(mono)
	n.lastStamp = s.Timestamp
	n.eraCandidate, n.eraCount = time.Time{}, 0

	n.appendValue(&n.offsets, offset)
	median, mad := medianMAD(n.offsets)
	sigma := mad * 1.4826
	precision := ntp.Log2Seconds(n.clk.Precision())

	if pulse := n.cfg.Pulse.latest(); !pulse.IsZero() {
		sentenceLag := arrival.Sub(pulse).Seconds()
		if sentenceLag >= 0 && sentenceLag < 2 {
			n.appendValue(&n.lags, sentenceLag)
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
	// A sentence after a long gap skips the slots itself, with no tick in
	// between. Freshness is lost here just as surely as it is on a timeout,
	// so the window has to go before this sentence is counted (RA6X-019).
	if slots >= reachBits {
		n.staleWindow("a gap longer than the reach register")
	}
	shiftReach(&n.reach, slots)
	n.reach |= 1
	if slots > 1 {
		n.addTimeouts(slots - 1)
	}
	n.slotAt = now
}

// chronologyOK is the anti-replay watermark. GPS time advances at 1 Hz, so a
// sentence is in sequence when it is after the last accepted one and no more
// than nmeaEraJump ahead of it.
//
// Anything else — a repeat, a replay, or a jump too large to be normal
// progression — is refused, and crucially is *not* committed to the
// watermark. A single checksum-valid glitch dating a sentence in 2099 used to
// become the watermark before the engine had accepted or refused the
// correction, after which every later correct timestamp was silently dropped
// and recovery needed a daemon restart (RA6X-020).
//
// A genuinely new era — a receiver replaced, or one that has corrected itself
// after a week rollover — still has to be adoptable. It proves itself with
// nmeaEraConfirm consecutive, self-consistent sentences, at which point the
// window built in the old era is dropped and the new era becomes the
// watermark. That is bounded repeated evidence, not an open door: an
// arbitrary old replay has to sustain a consistent 1 Hz sequence to be
// believed, and the build-date guard still rejects week-rollover dates.
func (n *NMEA) chronologyOK(stamp time.Time) bool {
	if n.lastStamp.IsZero() {
		return true
	}
	if stamp.After(n.lastStamp) && stamp.Sub(n.lastStamp) <= nmeaEraJump {
		n.eraCandidate, n.eraCount = time.Time{}, 0
		return true
	}
	if !n.noteEra(stamp) {
		return false
	}
	n.log.Warn("NMEA time era changed; adopting it after corroboration",
		"previous", n.lastStamp.UTC().Format(time.RFC3339), "now", stamp.UTC().Format(time.RFC3339),
		"sentences", n.eraCount)
	n.staleWindow("the receiver's time era changed")
	return true
}

// noteEra accumulates evidence for a timestamp era that disagrees with the
// current watermark and reports whether enough consecutive, self-consistent
// sentences have corroborated it.
func (n *NMEA) noteEra(stamp time.Time) bool {
	consistent := !n.eraCandidate.IsZero() &&
		stamp.After(n.eraCandidate) &&
		stamp.Sub(n.eraCandidate) <= nmeaEraJump
	if consistent {
		n.eraCount++
	} else {
		n.eraCount = 1
	}
	n.eraCandidate = stamp
	return n.eraCount >= nmeaEraConfirm
}

// staleWindow drops an observation window that no longer describes the
// receiver, so requalification needs the ordinary minimum of fresh accepted
// sentences rather than one sentence joining a historical majority. Ordinary
// short packet loss does not reach it: the window survives until reach
// actually empties.
func (n *NMEA) staleWindow(reason string) {
	if len(n.offsets) == 0 {
		return
	}
	n.resetWindow()
	n.log.Info("NMEA window dropped after losing freshness", "reason", reason)
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
	if n.reach == 0 {
		// Every sample in the window predates the outage. One fresh
		// sentence must not be able to requalify a mostly historical
		// window and stamp its median as current: the host may have slewed
		// or drifted meanwhile, making that estimate wrong and falsely
		// precise (RA6X-019). PPS does the same on an unreachable fetch.
		n.staleWindow("no sentence within the reach register")
	}
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

// resetWindow empties the offset and lag windows and arms an invalidation.
// Every local reset revokes the estimate the selector is holding, so the next
// measurement has to say so rather than leaving a stale median in place
// (RA6X-005, RA6X-019).
func (n *NMEA) resetWindow() {
	n.pendingInvalidate = true
	// Re-priming re-establishes timestamp ordering too. The watermark
	// belongs to the window's era: retaining it across a reset, a device
	// replacement or a successful reconnect is what made a single bad date
	// unrecoverable (RA6X-020).
	n.lastStamp = time.Time{}
	n.eraCandidate, n.eraCount = time.Time{}, 0
	n.offsets = n.offsets[:0]
	n.lags = n.lags[:0]
	n.updateInfo(func(_ *source.Info, r *source.RefclockInfo) {
		r.WindowSamples, r.WindowJitter, r.Stable = 0, 0, false
		r.MeasuredLag, r.LagSamples = 0, 0
	})
}

func (n *NMEA) emptyMeasurement() discipline.Measurement {
	return discipline.Measurement{
		Source: n.cfg.Name, Now: n.clk.Monotonic(), Reach: n.reach, Poll: nmeaPoll,
		Generation: n.generation(),
	}
}

func (n *NMEA) emit(ctx context.Context, out chan<- discipline.Measurement, m discipline.Measurement) bool {
	if n.pendingInvalidate {
		// Carry exactly one invalidation per reset.
		m.Invalidate = true
		n.pendingInvalidate = false
	}
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
