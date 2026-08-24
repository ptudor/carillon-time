package monitor

import (
	"slices"
	"testing"
	"time"

	"carillon/internal/discipline"
	"carillon/internal/engine"
	ntpserver "carillon/internal/server"
	"carillon/internal/source"
)

func testEngineStatus(now time.Time, state discipline.State) *engine.Status {
	return &engine.Status{
		Status: discipline.Status{
			State:        state,
			Stratum:      2,
			SystemSource: "home",
			Offset:       0.0001,
			Jitter:       0.00002,
		},
		Now:     now,
		Uptime:  time.Hour,
		Version: "test-version",
		Infos:   map[string]source.Info{},
	}
}

func TestSnapshotOf(t *testing.T) {
	now := time.Date(2026, 8, 23, 20, 0, 0, 0, time.UTC)
	st := testEngineStatus(now, discipline.StateSynced)
	s := SnapshotOf(st, ntpserver.StatsSnapshot{Served: 12}, true,
		Metadata{ID: "twocom", Name: "Twocom", Roles: []string{"colo", "ntp-pool"}}, now.Add(time.Second))
	if s.Schema != schemaV1 || s.Health.Status != "healthy" || s.Health.SnapshotAgeSeconds != 1 {
		t.Fatalf("snapshot health: %+v", s)
	}
	if s.Instance.ID != "twocom" || s.Instance.Version != "test-version" || !s.Instance.StartedAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("instance: %+v", s.Instance)
	}
	if s.Tracking.SystemSource != "home" || s.Server.Served != 12 || !s.Server.Enabled {
		t.Fatalf("payload: %+v", s)
	}
}

func TestHealthStates(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name       string
		state      discipline.State
		preferLost bool
		age        time.Duration
		want       string
		wantReason string
	}{
		{"synced", discipline.StateSynced, false, time.Second, "healthy", ""},
		{"holdover", discipline.StateHoldover, false, time.Second, "degraded", "holdover"},
		{"prefer lost", discipline.StateSynced, true, time.Second, "degraded", "preferred_source_lost"},
		{"settling", discipline.StateSettling, false, time.Second, "unhealthy", "settling"},
		{"stale", discipline.StateSynced, false, 6 * time.Second, "unhealthy", "snapshot_stale"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := testEngineStatus(now, tt.state)
			st.PreferLost = tt.preferLost
			h := healthOf(st, now.Add(tt.age))
			if h.Status != tt.want {
				t.Fatalf("status %q want %q: %+v", h.Status, tt.want, h)
			}
			if tt.wantReason != "" && !slices.Contains(h.Reasons, tt.wantReason) {
				t.Fatalf("missing reason %q: %+v", tt.wantReason, h)
			}
		})
	}
}

func TestHealthReportsLeapfileFreshness(t *testing.T) {
	now := time.Date(2026, 8, 23, 20, 0, 0, 0, time.UTC)
	st := testEngineStatus(now, discipline.StateSynced)
	st.LeapExpiry = now.Add(20 * 24 * time.Hour)
	if h := healthOf(st, now.Add(time.Second)); h.Status != "degraded" || !slices.Contains(h.Reasons, "leapfile_expiring") {
		t.Fatalf("expiring: %+v", h)
	}
	st.LeapExpiry = now.Add(-time.Second)
	if h := healthOf(st, now.Add(time.Second)); h.Status != "unhealthy" || !slices.Contains(h.Reasons, "leapfile_expired") {
		t.Fatalf("expired: %+v", h)
	}
}
