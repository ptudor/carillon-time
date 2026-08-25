// Package control implements the daemon's local status interface: a unix
// socket speaking newline-delimited JSON, one request and one response per
// connection. It is deliberately not NTP mode 6/7 and is never reachable
// from the network.
package control

import (
	"fmt"
	"strconv"
	"time"

	"carillon/internal/discipline"
	"carillon/internal/engine"
	"carillon/internal/ntp"
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

// CounterStats is one address family's NTP listener counters. Every datagram
// the listener read increments exactly one outcome counter, so Served plus
// every refusal reason accounts for all received traffic.
type CounterStats struct {
	Served   uint64 `json:"served"`
	Unsynced uint64 `json:"unsynced"`
	KoD      uint64 `json:"kod"`

	Denied      uint64 `json:"denied"`
	Martian     uint64 `json:"martian"`
	RateLimited uint64 `json:"ratelimited"`
	BadAuth     uint64 `json:"badauth"`
	BadVersion  uint64 `json:"badversion"`
	NonClient   uint64 `json:"nonclient"`
	Malformed   uint64 `json:"malformed"`
	Oversize    uint64 `json:"oversize"`

	NoKernelTS  uint64 `json:"no_kernel_timestamp"`
	KernelDrops uint64 `json:"kernel_drops"`
	Clients     int64  `json:"clients"`

	// Modes counts refused datagrams by NTP mode name ("control", "private");
	// Versions counts accepted requests by protocol version ("3", "4"). Only
	// non-zero buckets appear, so a quiet server carries neither field.
	Modes    map[string]uint64 `json:"modes,omitempty"`
	Versions map[string]uint64 `json:"versions,omitempty"`

	// LastRequest and LastServed are nil when nothing has arrived or been
	// answered yet. They are pointers because encoding/json cannot omit a
	// zero time.Time, and a client charting these must see the field absent
	// rather than the year 1.
	LastRequest *time.Time `json:"last_request,omitempty"`
	LastServed  *time.Time `json:"last_served,omitempty"`
}

// ModeOrder and VersionOrder list the histogram keys in their natural order,
// for callers that render them.
var (
	ModeOrder    = []string{"reserved", "symmetric-active", "symmetric-passive", "server", "broadcast", "control", "private"}
	VersionOrder = []string{"1", "2", "3", "4"}
)

// Requests returns every datagram accounted for by this snapshot.
func (c *CounterStats) Requests() uint64 { return c.Served + c.Dropped() }

// Dropped returns the datagrams that were refused for any reason.
func (c *CounterStats) Dropped() uint64 {
	return c.Denied + c.Martian + c.RateLimited + c.BadAuth +
		c.BadVersion + c.NonClient + c.Malformed + c.Oversize
}

// ServerStats is the NTP listener's request-counter snapshot: the totals at
// the top level, with the same counters broken out per address family so a
// dual-stack server can be charted as the two monitors the NTP pool sees.
type ServerStats struct {
	CounterStats
	IPv4 CounterStats `json:"ipv4"`
	IPv6 CounterStats `json:"ipv6"`
}

// ServerStatsOf converts the server package's atomic counter snapshot.
func ServerStatsOf(s ntpserver.StatsSnapshot) *ServerStats {
	return &ServerStats{
		CounterStats: counterStatsOf(s.Total),
		IPv4:         counterStatsOf(s.IPv4),
		IPv6:         counterStatsOf(s.IPv6),
	}
}

func counterStatsOf(c ntpserver.CounterSnapshot) CounterStats {
	out := CounterStats{
		Served: c.Served, Unsynced: c.Unsynced, KoD: c.KoD,
		Denied: c.Denied, Martian: c.Martian, RateLimited: c.RateLimited,
		BadAuth: c.BadAuth, BadVersion: c.BadVersion, NonClient: c.NonClient,
		Malformed: c.Malformed, Oversize: c.Oversize,
		NoKernelTS: c.NoKernelTS, KernelDrops: c.KernelDrops, Clients: c.Clients,
		LastRequest: optionalTime(c.LastRequest),
		LastServed:  optionalTime(c.LastServed),
	}
	for mode, value := range c.Modes {
		// Client mode is the request itself, never a refusal reason.
		if value == 0 || ntp.Mode(mode) == ntp.ModeClient {
			continue
		}
		if out.Modes == nil {
			out.Modes = make(map[string]uint64, 4)
		}
		out.Modes[ntp.Mode(mode).String()] = value
	}
	for version, value := range c.Versions {
		if value == 0 {
			continue
		}
		if out.Versions == nil {
			out.Versions = make(map[string]uint64, 4)
		}
		out.Versions[strconv.Itoa(version)] = value
	}
	return out
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
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
