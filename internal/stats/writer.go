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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"carillon/internal/engine"
)

const queueSize = 256

type dailyFile struct {
	day string
	f   *os.File
	w   *bufio.Writer
}

// Recorder accepts immutable engine snapshots without doing disk I/O in the
// caller. A full queue drops snapshots rather than delaying clock discipline;
// the writer logs the cumulative drop count on its next flush tick.
type Recorder struct {
	dir string
	log *slog.Logger
	ch  chan *engine.Status

	dropped atomic.Uint64
	files   map[string]*dailyFile

	lastUpdates int
	lastPulse   map[string]time.Time
	lastErrLog  time.Time
}

// New returns a recorder for an already-validated directory.
func New(dir string, log *slog.Logger) *Recorder {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Recorder{
		dir: dir, log: log, ch: make(chan *engine.Status, queueSize),
		files: make(map[string]*dailyFile), lastPulse: make(map[string]time.Time),
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
		case <-flush.C:
			r.flushAndReport()
			r.reportDrops()
		case <-ctx.Done():
			for {
				select {
				case st := <-r.ch:
					r.processAndReport(st)
				default:
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
	if st.Updates > 0 && st.Updates != r.lastUpdates {
		if err := r.writeLoop(st); err != nil {
			return err
		}
		if err := r.writeSources(st); err != nil {
			return err
		}
		r.lastUpdates = st.Updates
	}
	for name, info := range st.Infos {
		ref := info.Refclock
		if ref == nil || ref.LastPulse.IsZero() || ref.LastPulse.Equal(r.lastPulse[name]) {
			continue
		}
		line := strings.Join([]string{
			ref.LastPulse.UTC().Format(time.RFC3339Nano), escape(name), number(ref.LastOffset),
			strconv.FormatUint(uint64(ref.Sequence), 10),
		}, "\t") + "\n"
		if err := r.write("pps", ref.LastPulse, "time\tsource\toffset_seconds\tsequence\n", line); err != nil {
			return err
		}
		r.lastPulse[name] = ref.LastPulse
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
	df, err := r.file(kind, at.UTC().Format(time.DateOnly), header)
	if err != nil {
		return err
	}
	if _, err := df.w.WriteString(line); err != nil {
		return fmt.Errorf("stats: writing %s: %w", df.f.Name(), err)
	}
	return nil
}

func (r *Recorder) file(kind, day, header string) (*dailyFile, error) {
	if old := r.files[kind]; old != nil {
		if old.day == day {
			return old, nil
		}
		if err := closeDaily(old); err != nil {
			return nil, err
		}
		delete(r.files, kind)
	}
	path := filepath.Join(r.dir, kind+"."+day+".tsv")
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
}

func number(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func escape(v string) string {
	r := strings.NewReplacer("\\", "\\\\", "\t", "\\t", "\r", "\\r", "\n", "\\n")
	return r.Replace(v)
}
