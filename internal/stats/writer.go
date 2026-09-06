// Package stats records low-rate daily UTC TSV files for offline tuning and
// accuracy analysis. Disk I/O is isolated from the engine by a bounded queue.
package stats

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ptudor/carillon-time/internal/discipline"
	"github.com/ptudor/carillon-time/internal/engine"
	"github.com/ptudor/carillon-time/internal/ntp"
	ntpserver "github.com/ptudor/carillon-time/internal/server"
	"github.com/ptudor/carillon-time/internal/source"
)

const (
	queueSize = 256

	// eventMinInterval is the shortest gap between two rows for one subject
	// in events.tsv, so a flapping source cannot fill the disk.
	eventMinInterval = time.Second

	// pulseQueueSize buffers accepted reference-clock edges. A PPS delivers
	// one a second and the writer drains between them, so this is a large
	// margin; a full queue counts what it drops rather than blocking the
	// refclock goroutine.
	pulseQueueSize = 4096
)

// Config configures a Recorder.
type Config struct {
	// Dir is an already-validated directory for the daily files.
	Dir string

	// Server, when set, adds one server.YYYY-MM-DD.tsv row per address
	// family per minute. It is nil when the NTP listener is disabled.
	Server func() ntpserver.StatsSnapshot

	// Now reads the wall clock for the server rows. The loop and source
	// rows are timestamped from the engine snapshot instead.
	Now func() time.Time

	// KeepDays, when positive, removes day directories older than that many
	// days at each rotation. Zero keeps everything.
	KeepDays int

	Log *slog.Logger
}

type dailyFile struct {
	day string
	f   *os.File
	w   *bufio.Writer
}

// Recorder accepts immutable engine snapshots without doing disk I/O in the
// caller. A full queue drops snapshots rather than delaying clock discipline;
// the writer logs the cumulative drop count on its next flush tick.
type Recorder struct {
	dir    string
	log    *slog.Logger
	server func() ntpserver.StatsSnapshot
	now    func() time.Time
	ch     chan *engine.Status

	keepDays int

	dropped       atomic.Uint64
	pulses        chan source.Pulse
	pulsesDropped atomic.Uint64
	files         map[string]*dailyFile

	lastUpdates int

	// lastEvents is the previous snapshot's event-relevant state, and
	// lastEventAt throttles a flapping subject. See writeEvents.
	lastEvents  eventState
	lastEventAt map[string]time.Time
	lastErrLog  time.Time

	// horizon is the retention clock: the latest wall time seen while the
	// discipline vouched for it. Rows are still labelled with the time of
	// the observation they describe, but destructive retention only ever
	// runs from this, so a future RTC at boot — or a misdated source row —
	// cannot erase the archive (RA6X-046). It never moves backwards.
	horizon     time.Time
	haveHorizon bool
}

// New returns a recorder for an already-validated directory.
func New(cfg Config) *Recorder {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Recorder{
		dir: cfg.Dir, log: cfg.Log, server: cfg.Server, now: cfg.Now, keepDays: cfg.KeepDays,
		ch:          make(chan *engine.Status, queueSize),
		pulses:      make(chan source.Pulse, pulseQueueSize),
		files:       make(map[string]*dailyFile),
		lastEventAt: make(map[string]time.Time),
	}
}

// Record queues one immutable engine snapshot. It is safe to call from the
// engine goroutine and never blocks.
func (r *Recorder) Record(st *engine.Status) {
	if r == nil || st == nil {
		return
	}
	select {
	case r.ch <- st:
	default:
		r.dropped.Add(1)
	}
}

// Run writes queued snapshots, flushes once per minute, and drains the queue
// before closing files after ctx is cancelled.
func (r *Recorder) Run(ctx context.Context) {
	flush := time.NewTicker(time.Minute)
	defer flush.Stop()
	for {
		select {
		case st := <-r.ch:
			r.processAndReport(st)
		case p := <-r.pulses:
			r.recordPulseAndReport(p)
		case <-flush.C:
			r.recordServerAndReport()
			r.flushAndReport()
			r.reportDrops()
		case <-ctx.Done():
			for {
				select {
				case st := <-r.ch:
					r.processAndReport(st)
				case p := <-r.pulses:
					r.recordPulseAndReport(p)
				default:
					r.recordServerAndReport()
					r.flushAndReport()
					r.reportDrops()
					_ = r.closeFiles()
					return
				}
			}
		}
	}
}

func (r *Recorder) processAndReport(st *engine.Status) {
	if err := r.process(st); err != nil {
		r.reportError(err)
		_ = r.closeFiles()
	}
}

func (r *Recorder) process(st *engine.Status) error {
	if st.Now.IsZero() {
		return nil
	}
	r.noteHorizon(st)
	if err := r.writeEvents(st); err != nil {
		return err
	}
	if st.Updates > 0 && st.Updates != r.lastUpdates {
		if err := r.writeLoop(st); err != nil {
			return err
		}
		if err := r.writeSources(st); err != nil {
			return err
		}
		r.lastUpdates = st.Updates
	}
	return nil
}

// Pulse queues one accepted reference-clock edge. It is called from the
// refclock's own goroutine and never blocks: a full queue drops the pulse and
// counts it, so a gap in pps.tsv is always accounted for.
//
// Pulse rows used to be derived from each source's latest Info at snapshot
// time, which silently coalesced every pulse that arrived between two
// publications (RA6X-048). Carrying the record itself means one row per
// accepted pulse, or an explicit count of what was lost.
func (r *Recorder) Pulse(p source.Pulse) {
	if r == nil || p.At.IsZero() {
		return
	}
	select {
	case r.pulses <- p:
	default:
		r.pulsesDropped.Add(1)
	}
}

func (r *Recorder) recordPulse(p source.Pulse) error {
	line := strings.Join([]string{
		p.At.UTC().Format(time.RFC3339Nano), escape(p.Source), number(p.Offset),
		strconv.FormatUint(uint64(p.Sequence), 10),
	}, "\t") + "\n"
	return r.write("pps", p.At, "time\tsource\toffset_seconds\tsequence\n", line)
}

func (r *Recorder) recordPulseAndReport(p source.Pulse) {
	if err := r.recordPulse(p); err != nil {
		r.reportError(err)
		_ = r.closeFiles()
	}
}

// eventState is the part of a snapshot that events.tsv reports transitions
// of. Comparing successive values is what makes the file a record of what
// changed rather than a sample of what was true.
type eventState struct {
	state        string
	systemSource string
	ppsQualified bool
	preferLost   bool
	sources      map[string]string // name -> "status/reachable"
}

const eventsHeader = "time\tkind\tsubject\tfrom\tto\n"

// writeEvents records material state, selection and source changes.
//
// loop.tsv and sources.tsv are sampled per *loop update*, which is the
// documented contract and stays exactly as it is — but it means the most
// useful health changes, during filter starvation, holdover entry and expiry,
// repeated failures or a source shutdown, may never be recorded at all
// (RA6X-047). events.tsv is a separate, explicitly identified stream, so no
// existing column changes meaning and no consumer has to be told that
// `Updates` now means something else.
//
// Volume is bounded twice: only transitions are written, and a subject that
// is flapping is recorded at most once a second.
func (r *Recorder) writeEvents(st *engine.Status) error {
	now := eventStateOf(st)
	if r.lastEvents.sources == nil {
		// First snapshot: record the starting point, not a transition from
		// nothing to everything.
		r.lastEvents = now
		return nil
	}
	type change struct{ kind, subject, from, to string }
	var changes []change
	if now.state != r.lastEvents.state {
		changes = append(changes, change{"state", "", r.lastEvents.state, now.state})
	}
	if now.systemSource != r.lastEvents.systemSource {
		changes = append(changes, change{"system_source", "", r.lastEvents.systemSource, now.systemSource})
	}
	if now.ppsQualified != r.lastEvents.ppsQualified {
		changes = append(changes, change{"pps_qualified", "", boolText(r.lastEvents.ppsQualified), boolText(now.ppsQualified)})
	}
	if now.preferLost != r.lastEvents.preferLost {
		changes = append(changes, change{"prefer_lost", "", boolText(r.lastEvents.preferLost), boolText(now.preferLost)})
	}
	for name, to := range now.sources {
		if from, ok := r.lastEvents.sources[name]; !ok || from != to {
			was := from
			if !ok {
				was = "absent"
			}
			changes = append(changes, change{"source", name, was, to})
		}
	}
	for name, from := range r.lastEvents.sources {
		if _, ok := now.sources[name]; !ok {
			changes = append(changes, change{"source", name, from, "absent"})
		}
	}
	r.lastEvents = now
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].kind != changes[j].kind {
			return changes[i].kind < changes[j].kind
		}
		return changes[i].subject < changes[j].subject
	})
	for _, c := range changes {
		key := c.kind + "\x00" + c.subject
		if last, ok := r.lastEventAt[key]; ok && st.Now.Sub(last) < eventMinInterval {
			continue
		}
		r.lastEventAt[key] = st.Now
		line := strings.Join([]string{
			st.Now.UTC().Format(time.RFC3339Nano), c.kind, escape(c.subject), escape(c.from), escape(c.to),
		}, "\t") + "\n"
		if err := r.write("events", st.Now, eventsHeader, line); err != nil {
			return err
		}
	}
	return nil
}

func eventStateOf(st *engine.Status) eventState {
	e := eventState{
		state:        st.State.String(),
		systemSource: st.SystemSource,
		ppsQualified: st.PPSQualified,
		preferLost:   st.PreferLost,
		sources:      make(map[string]string, len(st.Sources)),
	}
	for _, s := range st.Sources {
		reach := "unreachable"
		if s.Reach != 0 {
			reach = "reachable"
		}
		e.sources[s.Name] = s.Status.String() + "/" + reach
	}
	return e
}

func boolText(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func (r *Recorder) recordServerAndReport() {
	if err := r.recordServer(); err != nil {
		r.reportError(err)
		_ = r.closeFiles()
	}
}

// serverHeader names every column of server.YYYY-MM-DD.tsv. The counters are
// cumulative since the daemon started, so a reader takes differences between
// consecutive rows; a value that drops is a restart, not negative traffic.
const serverHeader = "time\tfamily\tserved\tunsynced\tkod\tdenied\tmartian\tratelimited\tbadauth\t" +
	"badversion\tnonclient\tmalformed\toversize\tno_kernel_timestamp\tkernel_drops\tclients\t" +
	"mode_control\tmode_private\tv1\tv2\tv3\tv4\n"

// recordServer appends one row per address family. Splitting them is what
// makes the file useful: the NTP pool scores IPv4 and IPv6 separately, and a
// v6-only outage does not move a combined total.
func (r *Recorder) recordServer() error {
	if r.server == nil {
		return nil
	}
	snapshot := r.server()
	at := r.now().UTC()
	for _, family := range []struct {
		name     string
		counters ntpserver.CounterSnapshot
	}{{"ipv4", snapshot.IPv4}, {"ipv6", snapshot.IPv6}} {
		c := family.counters
		fields := []string{
			at.Format(time.RFC3339Nano), family.name,
			count(c.Served), count(c.Unsynced), count(c.KoD),
			count(c.Denied), count(c.Martian), count(c.RateLimited), count(c.BadAuth),
			count(c.BadVersion), count(c.NonClient), count(c.Malformed), count(c.Oversize),
			count(c.NoKernelTS), count(c.KernelDrops), strconv.FormatInt(c.Clients, 10),
			count(c.Modes[ntp.ModeControl]), count(c.Modes[ntp.ModePrivate]),
			count(c.Versions[1]), count(c.Versions[2]), count(c.Versions[3]), count(c.Versions[4]),
		}
		if err := r.write("server", at, serverHeader, strings.Join(fields, "\t")+"\n"); err != nil {
			return err
		}
	}
	return nil
}

func (r *Recorder) writeLoop(st *engine.Status) error {
	poll := int8(0)
	for _, src := range st.Sources {
		if src.Name == st.SystemSource {
			poll = src.Poll
			break
		}
	}
	line := strings.Join([]string{
		st.Now.UTC().Format(time.RFC3339Nano), st.State.String(), escape(st.SystemSource),
		number(st.Offset), number(st.Frequency), number(st.Jitter),
		strconv.FormatInt(int64(poll), 10), number(st.Pending),
		strconv.FormatUint(uint64(st.Stratum), 10), number(st.RootDelay), number(st.RootDisp),
	}, "\t") + "\n"
	return r.write("loop", st.Now,
		"time\tstate\tsystem_source\toffset_seconds\tfrequency_ppm\tjitter_seconds\tpoll\tpending_slew_seconds\tstratum\troot_delay_seconds\troot_dispersion_seconds\n",
		line)
}

func (r *Recorder) writeSources(st *engine.Status) error {
	const header = "time\tsource\tstatus\treach\tpoll\toffset_seconds\tdelay_seconds\tdispersion_seconds\tjitter_seconds\troot_distance_seconds\tstratum\trefid\tleap\n"
	for _, src := range st.Sources {
		line := strings.Join([]string{
			st.Now.UTC().Format(time.RFC3339Nano), escape(src.Name), src.Status.String(),
			strconv.FormatUint(uint64(src.Reach), 10), strconv.FormatInt(int64(src.Poll), 10),
			number(src.Offset), number(src.Delay), number(src.Dispersion), number(src.Jitter), number(src.Distance),
			strconv.FormatUint(uint64(src.Stratum), 10), escape(src.RefID.String()), src.Leap.String(),
		}, "\t") + "\n"
		if err := r.write("sources", st.Now, header, line); err != nil {
			return err
		}
	}
	return nil
}

func (r *Recorder) write(kind string, at time.Time, header, line string) error {
	df, err := r.file(kind, at.UTC(), header)
	if err != nil {
		return err
	}
	if _, err := df.w.WriteString(line); err != nil {
		return fmt.Errorf("stats: writing %s: %w", df.f.Name(), err)
	}
	return nil
}

// file returns the open file for kind on the UTC day of at, rotating and
// pruning as the day changes.
//
// Files live in Dir/YYYY/MM/DD/<kind>.tsv. A flat directory would collect
// four files a day — about 1,500 a year, with pps.tsv alone at 86,400 rows a
// day — with no cheap way to age anything out; a dated hierarchy keeps each
// directory small and makes retention a per-day removal. Three zero-padded
// segments so the paths sort lexically.
func (r *Recorder) file(kind string, at time.Time, header string) (*dailyFile, error) {
	day := at.Format(time.DateOnly)
	if old := r.files[kind]; old != nil {
		if old.day == day {
			return old, nil
		}
		if err := closeDaily(old); err != nil {
			return nil, err
		}
		delete(r.files, kind)
	}
	dir := filepath.Join(r.dir, at.Format("2006"), at.Format("01"), at.Format("02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("stats: creating %s: %w", dir, err)
	}
	r.pruneTrusted()
	path := filepath.Join(dir, kind+".tsv")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("stats: opening %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stats: stat %s: %w", path, err)
	}
	w := bufio.NewWriterSize(f, 32<<10)
	if info.Size() == 0 {
		if _, err := w.WriteString(header); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("stats: writing header %s: %w", path, err)
		}
	}
	df := &dailyFile{day: day, f: f, w: w}
	r.files[kind] = df
	return df, nil
}

// noteHorizon advances the retention clock. Only a snapshot the discipline
// reports as SYNCED counts: that is the state in which the daemon is
// asserting the wall clock is right, and it is reached only after settling,
// so it also excludes the interval around a large startup step. The horizon
// never retreats, so an unsynchronized excursion cannot pull retention
// backwards either.
func (r *Recorder) noteHorizon(st *engine.Status) {
	if st.State != discipline.StateSynced || st.Now.IsZero() {
		return
	}
	if !r.haveHorizon || st.Now.After(r.horizon) {
		r.horizon, r.haveHorizon = st.Now, true
	}
}

// pruneTrusted runs retention from the horizon rather than from the time of
// the row being written. Until the clock has been synchronized once, nothing
// is removed: an RTC reading 2099 at boot would otherwise delete every
// retained day on the first minute tick. Recording itself is never withheld —
// unsynchronized observations are still written, under their own dates.
//
// Because the horizon can lag the row's date, retention keeps at least
// keep_days and sometimes a little more. That is the safe direction.
func (r *Recorder) pruneTrusted() {
	if !r.haveHorizon {
		return
	}
	r.prune(r.horizon)
}

// prune removes day directories older than KeepDays, and any year and month
// directories left empty behind them. It runs at rotation, which is at most
// once a day per kind, so walking the tree costs nothing that matters.
// Callers reach it through pruneTrusted, which supplies a trusted time.
func (r *Recorder) prune(at time.Time) {
	if r.keepDays <= 0 {
		return
	}
	// Whole UTC days: keep_days = 1 keeps today and yesterday.
	cutoff := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -r.keepDays)
	years, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	for _, y := range years {
		if !y.IsDir() || !fourDigits(y.Name()) {
			continue
		}
		yearDir := filepath.Join(r.dir, y.Name())
		months, err := os.ReadDir(yearDir)
		if err != nil {
			continue
		}
		for _, m := range months {
			if !m.IsDir() || len(m.Name()) != 2 {
				continue
			}
			monthDir := filepath.Join(yearDir, m.Name())
			days, err := os.ReadDir(monthDir)
			if err != nil {
				continue
			}
			for _, d := range days {
				if !d.IsDir() || len(d.Name()) != 2 {
					continue
				}
				day, err := time.Parse(time.DateOnly, y.Name()+"-"+m.Name()+"-"+d.Name())
				if err != nil || !day.Before(cutoff) {
					continue
				}
				path := filepath.Join(monthDir, d.Name())
				if err := os.RemoveAll(path); err != nil {
					r.log.Warn("cannot remove an expired statistics day", "path", path, "error", err)
					continue
				}
				r.log.Info("removed expired statistics", "path", path, "keep_days", r.keepDays)
			}
			removeIfEmpty(monthDir)
		}
		removeIfEmpty(yearDir)
	}
}

func fourDigits(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func removeIfEmpty(dir string) {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
		_ = os.Remove(dir)
	}
}

func (r *Recorder) flushAndReport() {
	if err := r.flush(); err != nil {
		r.reportError(err)
		_ = r.closeFiles()
	}
}

func (r *Recorder) flush() error {
	var errs []error
	for _, df := range r.files {
		if err := df.w.Flush(); err != nil {
			errs = append(errs, fmt.Errorf("stats: flushing %s: %w", df.f.Name(), err))
		}
	}
	return errors.Join(errs...)
}

func (r *Recorder) closeFiles() error {
	var errs []error
	for kind, df := range r.files {
		if err := closeDaily(df); err != nil {
			errs = append(errs, err)
		}
		delete(r.files, kind)
	}
	return errors.Join(errs...)
}

func closeDaily(df *dailyFile) error {
	flushErr := df.w.Flush()
	closeErr := df.f.Close()
	if flushErr != nil {
		return fmt.Errorf("stats: flushing %s: %w", df.f.Name(), flushErr)
	}
	if closeErr != nil {
		return fmt.Errorf("stats: closing %s: %w", df.f.Name(), closeErr)
	}
	return nil
}

func (r *Recorder) reportError(err error) {
	now := time.Now()
	if r.lastErrLog.IsZero() || now.Sub(r.lastErrLog) >= time.Minute {
		r.log.Warn("statistics output unavailable", "error", err)
		r.lastErrLog = now
	}
}

func (r *Recorder) reportDrops() {
	if n := r.dropped.Swap(0); n != 0 {
		r.log.Warn("statistics snapshots dropped", "count", n)
	}
	if n := r.pulsesDropped.Swap(0); n != 0 {
		// Reported separately: a snapshot drop costs a sampled row, a pulse
		// drop costs a distinct recorded event.
		r.log.Warn("accepted pulses not recorded", "count", n)
	}
}

func number(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func count(v uint64) string { return strconv.FormatUint(v, 10) }

func escape(v string) string {
	r := strings.NewReplacer("\\", "\\\\", "\t", "\\t", "\r", "\\r", "\n", "\\n")
	return r.Replace(v)
}
