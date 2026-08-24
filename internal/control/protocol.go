// Package control implements the daemon's local status interface: a unix
// socket speaking newline-delimited JSON, one request and one response per
// connection. It is deliberately not NTP mode 6/7 and is never reachable
// from the network.
package control

import (
	"fmt"
	"time"

	"carillon/internal/discipline"
	"carillon/internal/engine"
	ntpserver "carillon/internal/server"
)

// Commands understood by the server.
const (
	CmdTracking    = "tracking"
	CmdSources     = "sources"
	CmdRefclock    = "refclock"
	CmdServerStats = "serverstats"
	CmdWaitSync    = "waitsync"
	CmdVersion     = "version"
)

// Request is one line of JSON from the client.
type Request struct {
	Command string `json:"command"`

	// Timeout in seconds, for waitsync (0 = wait for ever).
	Timeout float64 `json:"timeout,omitempty"`
}

// Response is one line of JSON from the server. Exactly one of the payload
// fields is set, or Error.
type Response struct {
	Error       string       `json:"error,omitempty"`
	Version     string       `json:"version,omitempty"`
	Tracking    *Tracking    `json:"tracking,omitempty"`
	Sources     []Source     `json:"sources,omitempty"`
	Refclocks   []Refclock   `json:"refclocks,omitempty"`
	ServerStats *ServerStats `json:"serverstats,omitempty"`
	Synced      *bool        `json:"synced,omitempty"`
}

// Refclock is one local PPS or NMEA clock's hardware, filtering and
// qualification status.
type Refclock struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Device    string `json:"device"`
	Edge      string `json:"edge"`
	Reach     uint8  `json:"reach"`
	Poll      int8   `json:"poll"`
	Sequence  uint32 `json:"sequence"`
	Qualified bool   `json:"qualified"`
	Stable    bool   `json:"stable"`
	Locked    bool   `json:"locked"`

	WindowSamples  int       `json:"window_samples"`
	WindowJitter   float64   `json:"window_jitter"`
	IntervalJitter float64   `json:"interval_jitter"`
	LastInterval   float64   `json:"last_interval"`
	LastPulse      time.Time `json:"last_pulse,omitempty"`
	LastOffset     float64   `json:"last_offset"`
	FixKnown       bool      `json:"fix_known"`
	FixValid       bool      `json:"fix_valid"`
	FixQuality     int       `json:"fix_quality"`
	Satellites     int       `json:"satellites"`
	Sentence       string    `json:"sentence,omitempty"`
	LastSentence   time.Time `json:"last_sentence,omitempty"`
	MeasuredLag    float64   `json:"measured_lag_seconds"`
	LagSamples     int       `json:"lag_samples"`

	Samples  uint64 `json:"samples"`
	Timeouts uint64 `json:"timeouts"`
	Gaps     uint64 `json:"gaps"`
	Glitches uint64 `json:"glitches"`
	Spikes   uint64 `json:"spikes"`
}

// ServerStats is the NTP listener's request-counter snapshot.
type ServerStats struct {
	Served      uint64    `json:"served"`
	Denied      uint64    `json:"denied"`
	RateLimited uint64    `json:"ratelimited"`
	BadAuth     uint64    `json:"badauth"`
	Unsynced    uint64    `json:"unsynced"`
	NoKernelTS  uint64    `json:"no_kernel_timestamp"`
	LastRequest time.Time `json:"last_request,omitempty"`
	LastServed  time.Time `json:"last_served,omitempty"`
}

// ServerStatsOf converts the server package's atomic counter snapshot.
func ServerStatsOf(s ntpserver.StatsSnapshot) *ServerStats {
	return &ServerStats{
		Served: s.Served, Denied: s.Denied, RateLimited: s.RateLimited,
		BadAuth: s.BadAuth, Unsynced: s.Unsynced, NoKernelTS: s.NoKernelTS,
		LastRequest: s.LastRequest, LastServed: s.LastServed,
	}
}

// Tracking is the system-level state.
type Tracking struct {
	State        string    `json:"state"`
	Stratum      uint8     `json:"stratum"`
	RefID        string    `json:"refid"`
	Leap         string    `json:"leap"`
	LeapSource   string    `json:"leap_source"`
	LeapExpiry   time.Time `json:"leapfile_expires,omitempty"`
	RefTime      time.Time `json:"reftime"`
	Now          time.Time `json:"now"`
	Uptime       float64   `json:"uptime_seconds"`
	Offset       float64   `json:"offset"`
	Frequency    float64   `json:"frequency_ppm"`
	FreqKnown    bool      `json:"frequency_known"`
	Jitter       float64   `json:"jitter"`
	Pending      float64   `json:"pending_slew"`
	RootDelay    float64   `json:"root_delay"`
	RootDisp     float64   `json:"root_dispersion"`
	SystemSource string    `json:"system_source,omitempty"`
	PreferLost   bool      `json:"prefer_lost"`
	Updates      int       `json:"updates"`
	Steps        int       `json:"steps"`
	Version      string    `json:"version"`
}

// Source is one source's line: the discipline's view merged with the
// source's own counters.
type Source struct {
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	Prefer     bool    `json:"prefer"`
	NoSelect   bool    `json:"noselect"`
	Reach      uint8   `json:"reach"`
	Poll       int8    `json:"poll"`
	Offset     float64 `json:"offset"`
	Delay      float64 `json:"delay"`
	Dispersion float64 `json:"dispersion"`
	Jitter     float64 `json:"jitter"`
	Distance   float64 `json:"distance"`
	Stratum    uint8   `json:"stratum"`
	RefID      string  `json:"refid"`
	Leap       string  `json:"leap"`

	Address    string    `json:"address,omitempty"`
	Resolved   string    `json:"resolved,omitempty"`
	LastRx     time.Time `json:"last_rx,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	Sent       uint64    `json:"sent"`
	Received   uint64    `json:"received"`
	Timeouts   uint64    `json:"timeouts"`
	Bogus      uint64    `json:"bogus"`
	BadAuth    uint64    `json:"bad_auth"`
	Kiss       uint64    `json:"kiss"`
	NoKernelTS uint64    `json:"no_kernel_timestamp"`
	Denied     bool      `json:"denied"`
}

// TrackingOf converts an engine snapshot.
func TrackingOf(st *engine.Status) *Tracking {
	return &Tracking{
		State:        st.State.String(),
		Stratum:      st.Stratum,
		RefID:        st.RefID.String(),
		Leap:         st.Leap.String(),
		LeapSource:   st.LeapSource,
		LeapExpiry:   st.LeapExpiry,
		RefTime:      st.RefTime,
		Now:          st.Now,
		Uptime:       st.Uptime.Seconds(),
		Offset:       st.Offset,
		Frequency:    st.Frequency,
		FreqKnown:    st.FreqKnown,
		Jitter:       st.Jitter,
		Pending:      st.Pending,
		RootDelay:    st.RootDelay,
		RootDisp:     st.RootDisp,
		SystemSource: st.SystemSource,
		PreferLost:   st.PreferLost,
		Updates:      st.Updates,
		Steps:        st.Steps,
		Version:      st.Version,
	}
}

// SourcesOf converts an engine snapshot's sources.
func SourcesOf(st *engine.Status) []Source {
	out := make([]Source, 0, len(st.Sources))
	for _, s := range st.Sources {
		line := Source{
			Name:       s.Name,
			Status:     s.Status.String(),
			Prefer:     s.Prefer,
			NoSelect:   s.NoSelect,
			Reach:      s.Reach,
			Poll:       s.Poll,
			Offset:     s.Offset,
			Delay:      s.Delay,
			Dispersion: s.Dispersion,
			Jitter:     s.Jitter,
			Distance:   s.Distance,
			Stratum:    s.Stratum,
			RefID:      s.RefID.String(),
			Leap:       s.Leap.String(),
		}
		if s.Status == discipline.StatusUnreachable {
			line.Stratum, line.RefID = 0, ""
		}
		if info, ok := st.Infos[s.Name]; ok {
			line.Address = info.Address
			if info.Resolved.IsValid() {
				line.Resolved = info.Resolved.String()
			}
			line.LastRx = info.LastRx
			line.LastError = info.LastError
			line.Sent, line.Received, line.Timeouts = info.Sent, info.Received, info.Timeouts
			line.Bogus, line.BadAuth, line.Kiss = info.Bogus, info.BadAuth, info.Kiss
			line.NoKernelTS = info.NoKernelTS
			line.Denied = info.Denied
			line.Reach = info.Reach
			line.Poll = info.Poll
		}
		out = append(out, line)
	}
	return out
}

// RefclocksOf merges each local source's lock-free hardware snapshot with the
// selector's second-numbering decision. Only pulse sources need global PPS
// qualification; NMEA numbers its own seconds.
func RefclocksOf(st *engine.Status) []Refclock {
	out := make([]Refclock, 0)
	for _, s := range st.Sources {
		info, ok := st.Infos[s.Name]
		if !ok || info.Refclock == nil {
			continue
		}
		r := info.Refclock
		pulse := r.Type == "pps" || r.Type == "gps-pps"
		qualified := s.Reach != 0
		if pulse {
			qualified = qualified && st.PPSQualified
		}
		out = append(out, Refclock{
			Name: s.Name, Type: r.Type, Device: r.Device, Edge: r.Edge,
			Reach: s.Reach, Poll: s.Poll, Sequence: r.Sequence,
			Qualified: qualified, Stable: r.Stable, Locked: qualified && r.Stable,
			WindowSamples: r.WindowSamples, WindowJitter: r.WindowJitter,
			IntervalJitter: r.IntervalJitter, LastInterval: r.LastInterval,
			LastPulse: r.LastPulse, LastOffset: r.LastOffset, Samples: r.Samples, Timeouts: r.Timeouts,
			Gaps: r.Gaps, Glitches: r.Glitches, Spikes: r.Spikes,
			FixKnown: r.FixKnown, FixValid: r.FixValid, FixQuality: r.FixQuality, Satellites: r.Satellites,
			Sentence: r.Sentence, LastSentence: r.LastSentence,
			MeasuredLag: r.MeasuredLag, LagSamples: r.LagSamples,
		})
	}
	return out
}

// ReachOctal renders a reach register the way ntpq and chronyc do.
func ReachOctal(r uint8) string { return fmt.Sprintf("%03o", r) }
