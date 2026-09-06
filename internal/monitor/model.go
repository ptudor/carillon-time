// Package monitor exposes carillon's immutable operational state through a
// read-only HTTP interface and Prometheus metrics.
package monitor

import (
	"time"

	"github.com/ptudor/carillon-time/internal/control"
	"github.com/ptudor/carillon-time/internal/discipline"
	"github.com/ptudor/carillon-time/internal/engine"
	ntpserver "github.com/ptudor/carillon-time/internal/server"
)

const schemaV1 = "carillon.status.v1"

// Health statuses. Degraded means the clock is still worth using but
// something has been lost; unhealthy means it is not.
const (
	statusHealthy   = "healthy"
	statusDegraded  = "degraded"
	statusUnhealthy = "unhealthy"
)

// Metadata is operator-provided identity used only by monitoring clients.
type Metadata struct {
	ID    string   `json:"id,omitempty"`
	Name  string   `json:"name,omitempty"`
	Roles []string `json:"roles,omitempty"`
}

// Instance identifies this process and its operator-assigned role.
type Instance struct {
	Metadata
	Version string `json:"version"`

	// StartedAt is the wall-clock reading taken once, when this process
	// started. It describes one process instance and does not move when the
	// clock is stepped — which is why it is captured rather than derived
	// from now minus uptime (RA6X-053). On a host whose clock was wrong at
	// boot it therefore reports the wrong time the daemon saw at start;
	// UptimeSeconds, which comes from the monotonic clock, is the reliable
	// measure of how long the process has run.
	StartedAt     time.Time `json:"started_at"`
	UptimeSeconds float64   `json:"uptime_seconds"`
}

// snapshotStaleSeconds is how old the engine's published snapshot may be
// before the monitor calls it stale. Measured on the monotonic clock.
const snapshotStaleSeconds = 5.0

// Health is the daemon's role-independent operational assessment. Reasons
// are stable machine-readable values suitable for a monitoring client.
type Health struct {
	Status             string   `json:"status"`
	Reasons            []string `json:"reasons"`
	SnapshotAgeSeconds float64  `json:"snapshot_age_seconds"`
}

// ServerStatus is the NTP listener's configuration state and counters.
type ServerStatus struct {
	Enabled bool `json:"enabled"`
	control.ServerStats
}

// Snapshot is the complete response returned by /api/v1/status.
type Snapshot struct {
	Schema     string    `json:"schema"`
	SnapshotAt time.Time `json:"snapshot_at"`
	ServedAt   time.Time `json:"served_at"`
	Instance   Instance  `json:"instance"`
	Health     Health    `json:"health"`

	Tracking  *control.Tracking  `json:"tracking"`
	Sources   []control.Source   `json:"sources"`
	Refclocks []control.Refclock `json:"refclocks"`
	Server    ServerStatus       `json:"server"`
}

// SnapshotOf converts the current lock-free engine and listener snapshots
// into the stable monitoring representation.
func SnapshotOf(st *engine.Status, stats ntpserver.StatsSnapshot, serverEnabled bool, meta Metadata, servedAt time.Time, mono float64, startedAt time.Time) Snapshot {
	servedAt = servedAt.UTC()
	serverStatus := control.ServerStatsOf(stats)
	return Snapshot{
		Schema:     schemaV1,
		SnapshotAt: st.Now.UTC(),
		ServedAt:   servedAt,
		Instance: Instance{
			Metadata:      Metadata{ID: meta.ID, Name: meta.Name, Roles: append([]string(nil), meta.Roles...)},
			Version:       st.Version,
			StartedAt:     startedAt.UTC(),
			UptimeSeconds: st.Uptime.Seconds(),
		},
		Health:    healthOf(st, servedAt, mono),
		Tracking:  control.TrackingOf(st),
		Sources:   control.SourcesOf(st),
		Refclocks: control.RefclocksOf(st),
		Server:    ServerStatus{Enabled: serverEnabled, ServerStats: *serverStatus},
	}
}

func healthOf(st *engine.Status, servedAt time.Time, mono float64) Health {
	// Freshness is measured on the monotonic scale, not by subtracting wall
	// timestamps. A daemon whose job is to step the wall clock cannot use it
	// to measure elapsed time: a backward step made an old snapshot look
	// fresh, because the negative age was clamped to zero (RA6X-053).
	age := mono - st.PublishedMono
	if age < 0 || st.PublishedMono == 0 {
		age = 0
	}
	h := Health{Status: statusHealthy, Reasons: []string{}, SnapshotAgeSeconds: age}
	switch st.State {
	case discipline.StateSynced:
	case discipline.StateHoldover:
		h.Status = statusDegraded
		h.Reasons = append(h.Reasons, "holdover")
	default:
		h.Status = statusUnhealthy
		h.Reasons = append(h.Reasons, st.State.String())
	}
	if st.PreferLost {
		if h.Status == statusHealthy {
			h.Status = statusDegraded
		}
		h.Reasons = append(h.Reasons, "preferred_source_lost")
	}
	if st.Now.IsZero() || age > snapshotStaleSeconds {
		h.Status = statusUnhealthy
		h.Reasons = append(h.Reasons, "snapshot_stale")
	}
	if !st.LeapExpiry.IsZero() {
		remaining := st.LeapExpiry.Sub(st.Now)
		switch {
		case remaining <= 0:
			h.Status = statusUnhealthy
			h.Reasons = append(h.Reasons, "leapfile_expired")
		case remaining <= 30*24*time.Hour:
			if h.Status == statusHealthy {
				h.Status = statusDegraded
			}
			h.Reasons = append(h.Reasons, "leapfile_expiring")
		}
	}
	return h
}
