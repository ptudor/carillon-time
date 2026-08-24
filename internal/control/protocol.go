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
	ServerStats *ServerStats `json:"serverstats,omitempty"`
	Synced      *bool        `json:"synced,omitempty"`
}

// ServerStats is the NTP listener's request-counter snapshot.
type ServerStats struct {
	Served      uint64 `json:"served"`
	Denied      uint64 `json:"denied"`
	RateLimited uint64 `json:"ratelimited"`
	BadAuth     uint64 `json:"badauth"`
	Unsynced    uint64 `json:"unsynced"`
}

// ServerStatsOf converts the server package's atomic counter snapshot.
func ServerStatsOf(s ntpserver.StatsSnapshot) *ServerStats {
	return &ServerStats{
		Served: s.Served, Denied: s.Denied, RateLimited: s.RateLimited,
		BadAuth: s.BadAuth, Unsynced: s.Unsynced,
	}
}

// Tracking is the system-level state.
type Tracking struct {
	State        string    `json:"state"`
	Stratum      uint8     `json:"stratum"`
	RefID        string    `json:"refid"`
	Leap         string    `json:"leap"`
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

	Address   string    `json:"address,omitempty"`
	Resolved  string    `json:"resolved,omitempty"`
	LastRx    time.Time `json:"last_rx,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Sent      uint64    `json:"sent"`
	Received  uint64    `json:"received"`
	Timeouts  uint64    `json:"timeouts"`
	Bogus     uint64    `json:"bogus"`
	BadAuth   uint64    `json:"bad_auth"`
	Kiss      uint64    `json:"kiss"`
	Denied    bool      `json:"denied"`
}

// TrackingOf converts an engine snapshot.
func TrackingOf(st *engine.Status) *Tracking {
	return &Tracking{
		State:        st.State.String(),
		Stratum:      st.Stratum,
		RefID:        st.RefID.String(),
		Leap:         st.Leap.String(),
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
			line.Denied = info.Denied
			line.Reach = info.Reach
			line.Poll = info.Poll
		}
		out = append(out, line)
	}
	return out
}

// ReachOctal renders a reach register the way ntpq and chronyc do.
func ReachOctal(r uint8) string { return fmt.Sprintf("%03o", r) }
