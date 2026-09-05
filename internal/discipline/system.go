package discipline

import (
	"math"

	"carillon/internal/ntp"
)

// State is the synchronization state of the daemon.
type State int

const (
	StateUnsynced State = iota // nothing usable yet, or lost for too long
	StateSettling              // corrections are being applied; not yet trusted
	StateSynced                // the clock is disciplined and served
	StateHoldover              // all sources lost; frequency held
)

func (s State) String() string {
	switch s {
	case StateUnsynced:
		return "unsynced"
	case StateSettling:
		return "settling"
	case StateSynced:
		return "synced"
	case StateHoldover:
		return "holdover"
	default:
		return "unknown"
	}
}

// Config configures a System.
type Config struct {
	Loop LoopConfig

	// MinSurvivors is the number of survivors required to synchronize.
	MinSurvivors int

	// HoldoverMax is how long (seconds) the clock stays in holdover with no
	// sources before the daemon declares itself unsynchronized.
	HoldoverMax float64

	// SettleUpdates is how many measurements for the system source must
	// arrive after a step before the clock is trusted (SETTLING → SYNCED),
	// on top of the mandatory post-step loop update and the requirement
	// that another step is no longer on the table. Zero selects 1.
	SettleUpdates int

	// LocalRefIDs is the set of RFC 5905 §7.3 reference identifiers that
	// name this host: one per local unicast address. Selection refuses a
	// source that is this daemon, or whose reference points back at it, so
	// two mutually configured instances cannot start feeding each other
	// their own retained time after losing a real upstream (RA6X-038).
	// Empty disables the check.
	LocalRefIDs map[ntp.RefID]bool
}

// EventKind classifies a notable thing that happened during an update.
type EventKind int

const (
	EventStep              EventKind = iota // the clock was stepped; Value = seconds
	EventPanicRefused                       // offset beyond the panic threshold; Value = seconds
	EventPopcorn                            // a spike was ignored; Value = seconds
	EventStateChange                        // From → To
	EventFalseticker                        // Source became a falseticker
	EventTruechimer                         // Source is no longer a falseticker
	EventPreferLost                         // the prefer source is not a survivor
	EventPreferRegained                     // the prefer source is back
	EventSystemSource                       // Source became the system source
	EventUnknownSource                      // a measurement arrived for an unregistered source
	EventPPSUnqualified                     // PPS is stable but no numbering source survives
	EventPPSQualified                       // PPS has a numbering source again
	EventTimingLoop                         // Source is this daemon, or is synchronized to it
	EventTimingLoopCleared                  // Source is no longer a timing loop
)

// Event is a notable occurrence, for the engine to log and count.
type Event struct {
	Kind   EventKind
	Source string
	Value  float64
	From   State
	To     State
}

// Result is what the engine must do after Update or Tick.
type Result struct {
	Actions []Action
	Events  []Event
}

// Status is a snapshot of the system for the server and control socket.
type Status struct {
	State     State
	Stratum   uint8
	RefID     ntp.RefID
	Leap      ntp.Leap
	RootDelay float64
	RootDisp  float64

	Offset    float64
	Frequency float64
	FreqKnown bool
	Jitter    float64
	Pending   float64

	SystemSource string
	PreferLost   bool
	PPSQualified bool
	LastUpdate   float64 // monotonic; zero if never
	Updates      int
	Steps        int
	Sources      []SourceStatus
}

// SourceStatus is one source's line in the status snapshot.
type SourceStatus struct {
	Name       string
	Status     SelectStatus
	Prefer     bool
	NoSelect   bool
	Reach      uint8
	Poll       int8
	Offset     float64
	Delay      float64
	Dispersion float64
	Jitter     float64
	Distance   float64
	Stratum    uint8
	RefID      ntp.RefID
	Leap       ntp.Leap
	Updated    float64

	// DisagreesWith and Disagreement are set on a PPS source whose offset
	// is too far from the numbering source that should vouch for it.
	DisagreesWith string
	Disagreement  float64
}

// System ties the sources, selection and loop together and owns the state
// machine. It is not safe for concurrent use; the engine goroutine owns it.
type System struct {
	cfg     Config
	sources map[string]*SourceState
	order   []string
	loop    *Loop

	state State

	// sinceStep counts measurements received for the system source since
	// the last step, and postStepUpdates the loop updates that have run
	// since it. SETTLING progress is measured in the former, not in loop
	// updates: on a low-jitter path the filter's first sample is often the
	// lowest-delay one it will see for many polls, so counting loop updates
	// kept a restarted host answering LI=3 for minutes.
	sinceStep       int
	postStepUpdates int

	// resyncing is set by Resync: the next loop update restores SYNCED
	// directly instead of passing through SETTLING.
	resyncing bool

	// everSynced reports whether synchronization has been established and
	// still applies to the clock the daemon is holding. It is set on
	// entering SYNCED and cleared by a step, which moves the clock out from
	// under whatever was established before it. Only a state with
	// still-applicable synchronization may enter serviceable holdover.
	everSynced bool

	// unsyncedReason records why the daemon is UNSYNCED, so the refid it
	// advertises tells the operator which of the three causes it is.
	unsyncedReason ntp.RefID

	sel           Selection
	sysName       string
	lastUpdate    float64
	haveUpdate    bool
	offset        float64
	rootDelay     float64
	rootDisp      float64
	stratum       uint8
	refID         ntp.RefID
	leap          ntp.Leap
	holdoverSince float64
	preferLost    bool
}

// New returns a System starting from the given frequency (ppm) and whether
// that value is trusted.
func New(cfg Config, freq float64, freqKnown bool) *System {
	if cfg.MinSurvivors < 1 {
		cfg.MinSurvivors = 1
	}
	if cfg.SettleUpdates < 1 {
		cfg.SettleUpdates = 1
	}
	return &System{
		cfg:            cfg,
		sources:        make(map[string]*SourceState),
		loop:           NewLoop(cfg.Loop, freq, freqKnown),
		stratum:        16,
		refID:          ntp.KissINIT,
		leap:           ntp.LeapUnsync,
		unsyncedReason: ntp.KissINIT,
	}
}

// AddSource registers a source. Registering an existing name replaces its
// options and clears its state.
func (s *System) AddSource(name string, o Options) {
	if _, ok := s.sources[name]; !ok {
		s.order = append(s.order, name)
	}
	s.sources[name] = &SourceState{Name: name, Options: o}
}

// RemoveSource forgets a source, as when its goroutine has exited.
func (s *System) RemoveSource(name string, now float64) Result {
	if _, ok := s.sources[name]; !ok {
		return Result{}
	}
	delete(s.sources, name)
	for i, n := range s.order {
		if n == name {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return s.reselect(now)
}

// InvalidateSources discards every pre-boundary estimate while retaining
// reach and loop frequency. The engine uses it after a leap transition, just
// as source-local Reset discards each producer's sample window.
func (s *System) InvalidateSources(now float64) Result {
	for _, src := range s.all() {
		src.invalidate()
	}
	return s.reselect(now)
}

// Resync discards every pre-boundary estimate the way InvalidateSources does,
// but without treating the loss as a loss of synchronization. The engine uses
// it after a leap transition: a leap moves the clock by a whole second and
// changes neither the frequency nor the residual phase error, so there is
// nothing to settle. Dropping to SETTLING there answers LI=3 / stratum 16
// until the state machine works its way back — minutes, at exactly the moment
// a leap second makes a good server most valuable, and long enough for every
// downstream instance that prefers this host to fall back to its public
// survivors.
func (s *System) Resync(now float64) Result {
	for _, src := range s.all() {
		src.invalidate()
	}
	s.resyncing = s.state == StateSynced || s.state == StateHoldover
	return s.reselect(now)
}

// State returns the current synchronization state.
func (s *System) State() State { return s.state }

// Frequency returns the loop's current frequency correction in ppm.
func (s *System) Frequency() float64 { return s.loop.Freq }

// FreqKnown reports whether the frequency has been measured or loaded.
func (s *System) FreqKnown() bool { return s.loop.FreqKnown }

// Applied returns the frequency word the actuator was last given and whether
// one was given at all. See Loop.Applied.
func (s *System) Applied() (float64, bool) { return s.loop.Applied() }

// Pending returns the residual phase, in seconds, the loop has not slewed yet.
func (s *System) Pending() float64 { return s.loop.Pending }

func (s *System) all() []*SourceState {
	out := make([]*SourceState, 0, len(s.order))
	for _, n := range s.order {
		out = append(out, s.sources[n])
	}
	return out
}

// Update processes a measurement from a source.
func (s *System) Update(m Measurement) Result {
	src, ok := s.sources[m.Source]
	if !ok {
		return Result{Events: []Event{{Kind: EventUnknownSource, Source: m.Source}}}
	}
	src.apply(m)
	// Settling counts *acquisitions* from the system source, not every
	// event that names it. A timeout, a bad MAC, a rejected packet or an
	// invalidation notice is a transport heartbeat, not the post-step
	// evidence the state machine is waiting for; a good reply whose filter
	// winner is unchanged is an acquisition and does count (RA6X-010).
	if m.Source == s.sysName && m.IsAcquisition() {
		s.sinceStep++
	}
	return s.reselect(m.Now)
}

func (s *System) setState(to State, res *Result) {
	if s.state == to {
		return
	}
	if to == StateSynced {
		s.everSynced = true
	}
	res.Events = append(res.Events, Event{Kind: EventStateChange, From: s.state, To: to})
	s.state = to
}

func (s *System) reselect(now float64) Result {
	var res Result
	all := s.all()
	prev := make(map[string]SelectStatus, len(all))
	for _, src := range all {
		prev[src.Name] = src.Status
	}
	sel := SelectAt(all, now, s.cfg.MinSurvivors, SelectOptions{
		LocalRefIDs:  s.cfg.LocalRefIDs,
		AppliedSince: s.loop.AppliedSince,
	})
	res.Events = append(res.Events, sel.Events...)
	for _, src := range all {
		was, is := prev[src.Name], src.Status
		if is == StatusFalseticker && was != StatusFalseticker {
			res.Events = append(res.Events, Event{Kind: EventFalseticker, Source: src.Name, Value: src.Offset})
		}
		if was == StatusFalseticker && is != StatusFalseticker && is != StatusUnreachable {
			res.Events = append(res.Events, Event{Kind: EventTruechimer, Source: src.Name, Value: src.Offset})
		}
		if src.PPS {
			if is == StatusUnqualified && was != StatusUnqualified {
				res.Events = append(res.Events, Event{Kind: EventPPSUnqualified, Source: src.Name})
			} else if was == StatusUnqualified && is != StatusUnqualified && is != StatusUnreachable && is != StatusInvalid && is != StatusFalseticker {
				res.Events = append(res.Events, Event{Kind: EventPPSQualified, Source: src.Name})
			}
		}
	}
	if sel.PreferLost != s.preferLost {
		kind := EventPreferRegained
		if sel.PreferLost {
			kind = EventPreferLost
		}
		res.Events = append(res.Events, Event{Kind: kind})
		s.preferLost = sel.PreferLost
	}
	s.sel = sel

	// Protocol metadata the survivors agree on is published as soon as it
	// is accepted, not only after the system source contributes a new
	// filter output. A leap warning announced by other survivors while the
	// system source's own winner is unchanged used to be ignored entirely,
	// because this assignment sat below the early returns (RA6X-022).
	s.leap = majorityLeap(sel.Survivors)

	if sel.System == nil {
		s.sysName = ""
		switch {
		case s.state == StateSynced:
			s.holdoverSince = now
			s.setState(StateHoldover, &res)
		case s.state == StateSettling && s.everSynced:
			// Settling *after* a spell of synchronization — the filter
			// withheld updates, or a leap resync is in progress — still has
			// applicable synchronization to coast on.
			s.holdoverSince = now
			s.setState(StateHoldover, &res)
		case s.state == StateSettling:
			// Never synchronized in this epoch, and now there is nothing to
			// synchronize from. HOLDOVER is served as synchronized by both
			// the wire and the kernel; entering it here would mean losing
			// the last source *increased* the trust placed in a clock the
			// daemon never finished settling — including immediately after
			// a step (RA6X-009).
			s.unsyncedReason = ntp.KissINIT
			s.setState(StateUnsynced, &res)
		}
		return res
	}

	if sel.System.Name != s.sysName {
		s.sysName = sel.System.Name
		res.Events = append(res.Events, Event{Kind: EventSystemSource, Source: s.sysName})
	}
	// Run the loop only when the system source has a sample it has not
	// already used; updates from other sources just refresh the selection.
	// Consumption is tracked per source: a single system-wide watermark
	// reset on every source switch, so switching away from a source and
	// back again re-applied an observation the loop had already integrated
	// (RA6X-003, RA6X-001).
	if sel.System.At <= sel.System.usedAt {
		if s.state == StateHoldover {
			s.setState(StateSettling, &res)
		}
		// Settling progress is counted in measurements, not loop updates,
		// so it must also be *checked* without one. A clock filter that is
		// withholding updates — an early low-delay sample can do that for
		// up to the Allan intercept — would otherwise pin the daemon in
		// SETTLING for the whole drought, which is the outage this counter
		// was changed to remove.
		if s.state == StateSettling && s.settleDone(s.offset) {
			s.setState(StateSynced, &res)
		}
		return res
	}
	sel.System.usedAt = sel.System.At

	u := s.loop.Update(sel.Offset, sel.System.Poll, now, s.state == StateSynced, s.mayStep(&sel))
	res.Actions = append(res.Actions, u.Actions...)
	switch {
	case u.Deferred:
		// The offset warrants a step but there is not enough post-step
		// evidence yet. Leave the loop untouched and wait for the next
		// sample; the source's usedAt has already advanced, so it will run
		// again.
		return res
	case u.PanicRefused:
		res.Events = append(res.Events, Event{Kind: EventPanicRefused, Value: sel.Offset})
		s.unsyncedReason = ntp.KissPANC
		s.setState(StateUnsynced, &res)
		return res
	case u.Ignored:
		res.Events = append(res.Events, Event{Kind: EventPopcorn, Value: sel.Offset})
		return res
	case u.Stepped:
		res.Events = append(res.Events, Event{Kind: EventStep, Value: sel.Offset})
		for _, src := range all {
			src.invalidate()
		}
		s.offset = 0
		// Reset the settling evidence on *every* step, not only the first:
		// setState is a no-op when the state is already SETTLING, so a
		// second step inside the startup window used to leave the count
		// standing and declare SYNCED one update early.
		s.sinceStep, s.postStepUpdates, s.resyncing = 0, 0, false
		// The clock has just moved: whatever synchronization preceded the
		// step no longer applies to it, so losing the last source before
		// settling completes must not be served as holdover (RA6X-009).
		s.everSynced = false
		s.setState(StateSettling, &res)
	default:
		s.offset = sel.Offset
		s.postStepUpdates++
		switch s.state {
		case StateUnsynced, StateHoldover, StateSettling:
			if s.resyncing || s.settleDone(sel.Offset) {
				s.resyncing = false
				s.setState(StateSynced, &res)
			} else {
				s.setState(StateSettling, &res)
			}
		}
	}

	// System variables from the system source (RFC 5905 §11.2.3).
	sys := sel.System
	if sys.Stratum >= 15 {
		s.stratum = 16
	} else {
		s.stratum = sys.Stratum + 1
	}
	s.refID = sys.SourceRefID
	s.rootDelay = sys.RootDelay + sys.Delay
	s.rootDisp = sys.RootDisp + sys.Dispersion + sel.Jitter
	s.lastUpdate = now
	s.haveUpdate = true
	return res
}

// settleDone decides SETTLING → SYNCED. The clock is trusted once a fresh
// loop update has run against a post-step sample and another step is no
// longer on the table — either the budget is spent or the last offset was
// under the step threshold. Progress is counted in measurements arriving for
// the system source rather than in loop updates, because a loop update
// requires a *new lowest-delay* filter sample: on a quiet LAN the first reply
// is often the best the filter sees for dozens of polls, so three loop
// updates could take three quarters of an hour while the server answered
// LI=3 and every client rejected it.
func (s *System) settleDone(offset float64) bool {
	if s.postStepUpdates < 1 {
		return false // a step is never served as synchronized
	}
	if s.sinceStep < s.cfg.SettleUpdates {
		return false
	}
	return !s.loop.stepAllowed() || math.Abs(offset) <= s.cfg.Loop.StepThreshold
}

// mayStep decides whether the loop is allowed to step on this update. The
// first step of a run is always allowed — it is how a host with no RTC gets
// its clock — but a *further* step must rest on more than one post-step
// sample. Without that, a measurement that was computed before the first step
// and only dequeued afterwards steps the clock a second time by the same
// amount, in the opposite direction. Two survivors that have both reported
// since the step count as well: survivors agree by construction, having
// passed the intersection.
func (s *System) mayStep(sel *Selection) bool {
	if s.loop.Steps == 0 {
		return true
	}
	if sel.System.sinceStep >= 2 {
		return true
	}
	fresh := 0
	for _, src := range sel.Survivors {
		if src.sinceStep >= 1 {
			fresh++
		}
	}
	return fresh >= 2
}

// Tick runs the per-second work: phase slewing, time-driven source
// eligibility, and the holdover timeout.
//
// The reselection is what makes eligibility honest. Selection ages
// uncertainty and rejects over-distance and stale sources, but it used to run
// only on a measurement or an explicit lifecycle event, so if every producer
// went silent a synchronized source stayed selected indefinitely and the
// holdover timer never started (RA6X-003). It cannot re-integrate an
// already-consumed observation: the per-source usedAt watermark is what gates
// the loop, and a tick brings no new sample.
func (s *System) Tick(now float64) Result {
	res := s.reselect(now)
	res.Actions = append(s.loop.Tick(now), res.Actions...)
	if s.state == StateHoldover && now-s.holdoverSince > s.cfg.HoldoverMax {
		s.unsyncedReason = ntp.KissHOLD
		s.setState(StateUnsynced, &res)
	}
	return res
}

// Status returns a snapshot at time now. Root dispersion is aged and
// includes the phase still to be slewed, so it is an honest error bound.
func (s *System) Status(now float64) Status {
	st := Status{
		State:        s.state,
		Stratum:      s.stratum,
		RefID:        s.refID,
		Leap:         s.leap,
		RootDelay:    s.rootDelay,
		Offset:       s.offset,
		Frequency:    s.loop.Freq,
		FreqKnown:    s.loop.FreqKnown,
		Jitter:       s.loop.Jitter,
		Pending:      s.loop.Pending,
		SystemSource: s.sysName,
		PreferLost:   s.preferLost,
		PPSQualified: s.sel.PPSQualified,
		Updates:      s.loop.Updates,
		Steps:        s.loop.Steps,
	}
	if s.haveUpdate {
		st.LastUpdate = s.lastUpdate
		st.RootDisp = s.rootDisp + Phi*math.Max(0, now-s.lastUpdate) + math.Abs(s.loop.Pending)
	}
	switch s.state {
	case StateUnsynced:
		st.Stratum = 16
		st.Leap = ntp.LeapUnsync
		st.RootDisp = MaxDispersion
		// HOLD means "was synchronized, lost its sources", which is what
		// ntpd and chrony users read it as. A clock so far out that the
		// panic gate refused it is a different problem and gets its own
		// code, or the operator goes looking at the network.
		st.RefID = s.unsyncedReason
	case StateSettling:
		st.Leap = ntp.LeapUnsync
		if s.loop.Steps > 0 && s.offset == 0 {
			st.RefID = ntp.KissSTEP
		}
	}
	for _, src := range s.all() {
		st.Sources = append(st.Sources, SourceStatus{
			Name:          src.Name,
			Status:        src.Status,
			Prefer:        src.Prefer,
			NoSelect:      src.NoSelect,
			Reach:         src.Reach,
			Poll:          src.Poll,
			Offset:        src.Offset,
			Delay:         src.Delay,
			Dispersion:    src.Dispersion,
			Jitter:        src.Jitter,
			Distance:      src.Distance,
			Stratum:       src.Stratum,
			RefID:         src.RefID,
			Leap:          src.Leap,
			DisagreesWith: src.DisagreesWith,
			Disagreement:  src.Disagreement,
			Updated:       src.Updated,
		})
	}
	return st
}
