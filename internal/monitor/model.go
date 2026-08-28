// Package monitor exposes carillon's immutable operational state through a
// read-only HTTP interface and Prometheus metrics.
package monitor

import (
	"time"

	"carillon/internal/control"
	"carillon/internal/discipline"
	"carillon/internal/engine"
	ntpserver "carillon/internal/server"
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
	Version       string    `json:"version"`
	StartedAt     time.Time `json:"started_at"`
	UptimeSeconds float64   `json:"uptime_seconds"`
}

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
func SnapshotOf(st *engine.Status, stats ntpserver.StatsSnapshot, serverEnabled bool, meta Metadata, servedAt time.Time) Snapshot {
	servedAt = servedAt.UTC()
	startedAt := st.Now.Add(-st.Uptime).UTC()
	if st.Now.IsZero() {
		startedAt = time.Time{}
	}
	serverStatus := control.ServerStatsOf(stats)
	return Snapshot{
		Schema:     schemaV1,
		SnapshotAt: st.Now.UTC(),
		ServedAt:   servedAt,
		Instance: Instance{
			Metadata:      Metadata{ID: meta.ID, Name: meta.Name, Roles: append([]string(nil), meta.Roles...)},
			Version:       st.Version,
			StartedAt:     startedAt,
			UptimeSeconds: st.Uptime.Seconds(),
		},
		Health:    healthOf(st, servedAt),
		Tracking:  control.TrackingOf(st),
		Sources:   control.SourcesOf(st),
		Refclocks: control.RefclocksOf(st),
		Server:    ServerStatus{Enabled: serverEnabled, ServerStats: *serverStatus},
	}
}

func healthOf(st *engine.Status, servedAt time.Time) Health {
	age := servedAt.Sub(st.Now).Seconds()
	if age < 0 {
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
	if st.Now.IsZero() || age > 5 {
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
