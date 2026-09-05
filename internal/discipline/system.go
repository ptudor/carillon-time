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

	// SettleUpdates is the number of consecutive loop updates without a
	// step after which the clock is trusted (SettLING → SYNCED).
	SettleUpdates int
}

// EventKind classifies a notable thing that happened during an update.
type EventKind int

const (
	EventStep           EventKind = iota // the clock was stepped; Value = seconds
	EventPanicRefused                    // offset beyond the panic threshold; Value = seconds
	EventPopcorn                         // a spike was ignored; Value = seconds
	EventStateChange                     // From → To
	EventFalseticker                     // Source became a falseticker
	EventTruechimer                      // Source is no longer a falseticker
	EventPreferLost                      // the prefer source is not a survivor
	EventPreferRegained                  // the prefer source is back
	EventSystemSource                    // Source became the system source
	EventUnknownSource                   // a measurement arrived for an unregistered source
	EventPPSUnqualified                  // PPS is stable but no numbering source survives
	EventPPSQualified                    // PPS has a numbering source again
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
}

// System ties the sources, selection and loop together and owns the state
// machine. It is not safe for concurrent use; the engine goroutine owns it.
type System struct {
	cfg     Config
	sources map[string]*SourceState
	order   []string
	loop    *Loop

	state         State
	settled       int
	sel           Selection
	sysName       string
	lastUsedAt    float64
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
		cfg:     cfg,
		sources: make(map[string]*SourceState),
		loop:    NewLoop(cfg.Loop, freq, freqKnown),
		stratum: 16,
		refID:   ntp.KissINIT,
		leap:    ntp.LeapUnsync,
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
	s.lastUsedAt = 0
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
	return s.reselect(m.Now)
}

func (s *System) setState(to State, res *Result) {
	if s.state == to {
		return
	}
	res.Events = append(res.Events, Event{Kind: EventStateChange, From: s.state, To: to})
	s.state = to
	if to != StateSynced {
		s.settled = 0
	}
}

func (s *System) reselect(now float64) Result {
	var res Result
	all := s.all()
	prev := make(map[string]SelectStatus, len(all))
	for _, src := range all {
		prev[src.Name] = src.Status
	}
	sel := Select(all, now, s.cfg.MinSurvivors)
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
			} else if was == StatusUnqualified && is != StatusUnqualified && is != StatusUnreachable && is != StatusInvalid {
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

	if sel.System == nil {
		s.sysName = ""
		switch s.state {
		case StateSettling, StateSynced:
			s.holdoverSince = now
			s.setState(StateHoldover, &res)
		}
		return res
	}

	if sel.System.Name != s.sysName {
		s.sysName = sel.System.Name
		s.lastUsedAt = 0
		res.Events = append(res.Events, Event{Kind: EventSystemSource, Source: s.sysName})
	}
	// Run the loop only when the system source has a sample it has not
	// already used; updates from other sources just refresh the selection.
	if sel.System.At <= s.lastUsedAt {
		if s.state == StateHoldover {
			s.setState(StateSettling, &res)
		}
		return res
	}
	s.lastUsedAt = sel.System.At

	u := s.loop.Update(sel.Offset, sel.System.Poll, now, s.state == StateSynced)
	res.Actions = append(res.Actions, u.Actions...)
	switch {
	case u.PanicRefused:
		res.Events = append(res.Events, Event{Kind: EventPanicRefused, Value: sel.Offset})
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
		s.setState(StateSettling, &res)
	default:
		s.offset = sel.Offset
		switch s.state {
		case StateUnsynced, StateHoldover:
			s.setState(StateSettling, &res)
			s.settled = 1
		case StateSettling:
			s.settled++
			if s.settled >= s.cfg.SettleUpdates {
				s.setState(StateSynced, &res)
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
	s.leap = majorityLeap(sel.Survivors)
	s.lastUpdate = now
	s.haveUpdate = true
	return res
}

// Tick runs the per-second work: phase slewing and the holdover timeout.
func (s *System) Tick(now float64) Result {
	res := Result{Actions: s.loop.Tick(now)}
	if s.state == StateHoldover && now-s.holdoverSince > s.cfg.HoldoverMax {
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
		if !s.haveUpdate {
			st.RefID = ntp.KissINIT
		} else {
			st.RefID = ntp.KissHOLD
		}
	case StateSettling:
		st.Leap = ntp.LeapUnsync
		if s.loop.Steps > 0 && s.offset == 0 {
			st.RefID = ntp.KissSTEP
		}
	}
	for _, src := range s.all() {
		st.Sources = append(st.Sources, SourceStatus{
			Name:       src.Name,
			Status:     src.Status,
			Prefer:     src.Prefer,
			NoSelect:   src.NoSelect,
			Reach:      src.Reach,
			Poll:       src.Poll,
			Offset:     src.Offset,
			Delay:      src.Delay,
			Dispersion: src.Dispersion,
			Jitter:     src.Jitter,
			Distance:   src.Distance,
			Stratum:    src.Stratum,
			RefID:      src.RefID,
			Leap:       src.Leap,
			Updated:    src.Updated,
		})
	}
	return st
}
