package monitor

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/carillon-time/internal/discipline"
	"github.com/ptudor/carillon-time/internal/engine"
	"github.com/ptudor/carillon-time/internal/leap"
	"github.com/ptudor/carillon-time/internal/ntp"
	ntpserver "github.com/ptudor/carillon-time/internal/server"
)

func TestLeapHealthReasons(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name           string
		mutate         func(*engine.Status)
		status, reason string
	}{
		{"required table absent", func(s *engine.Status) { s.LeapRequired = true }, "unhealthy", "leap_table_unavailable"},
		{"plain client unknown LI", func(s *engine.Status) { s.Leap = ntp.LeapUnsync }, "healthy", ""},
		{"plain client holdover", func(s *engine.Status) { s.State = discipline.StateHoldover; s.Leap = ntp.LeapUnsync }, "degraded", "holdover"},
		{"plain client expired table", func(s *engine.Status) { s.Leap = ntp.LeapUnsync; s.LeapExpiry = now.Add(-time.Second) }, "degraded", "leapfile_expired"},
		{"conflict", func(s *engine.Status) { s.LeapUpdate.LastRejection = "conflict" }, "degraded", "leap_conflict"},
		{"armed rejection", func(s *engine.Status) { s.LeapUpdate.LastRejection = "armed" }, "degraded", "leap_conflict"},
		{"source disagreement", func(s *engine.Status) { s.LeapDisagreement = true }, "degraded", "leap_source_disagreement"},
		{"transient fetch error", func(s *engine.Status) {
			s.LeapRequired, s.LeapReady = true, true
			s.LeapExpiry = now.Add(60 * 24 * time.Hour)
			s.LeapUpdate.LastRejection = "io"
		}, "healthy", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testEngineStatus(now, discipline.StateSynced)
			tc.mutate(st)
			h := healthOf(st, now, testPublishedMono)
			if h.Status != tc.status || (tc.reason != "" && !slices.Contains(h.Reasons, tc.reason)) {
				t.Fatalf("health: %+v", h)
			}
			if !st.LeapRequired && slices.Contains(h.Reasons, "leap_table_unavailable") {
				t.Fatal("optional leap knowledge became a health requirement")
			}
		})
	}
}

func TestLeapMetricsHaveBoundedLabelsAndSeparateReadiness(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	st := testEngineStatus(now, discipline.StateSettling)
	st.Leap = ntp.LeapUnsync
	st.LeapReady, st.LeapRequired = true, true
	st.LeapExpiry, st.LeapUpdated = now.Add(60*24*time.Hour), now.Add(-time.Hour)
	st.LeapHash = "accepted-object-hash"
	st.LeapProvider = leap.Provider{Kind: "peer", Name: "private-relay", KeyID: 1}
	st.LeapUpdate = leap.UpdateStatus{Pending: "pending-object-hash", Rejected: "rejected-object-hash", Counts: leap.Counts{Probes: 1, Bytes: 2, Accepted: 3, Failures: 4, Rollback: 5, Conflict: 6, CacheFailures: 7, Served: 8, RateLimited: 9}}
	body := gatherMetrics(t, func() Snapshot {
		return SnapshotOf(st, ntpserver.StatsSnapshot{}, false, Metadata{}, now, testPublishedMono, now)
	})
	for _, want := range []string{
		"carillon_leap_pending{} 0", "carillon_leap_ready{} 1", "carillon_leap_table_required{} 1", "carillon_leapfile_valid{} 1",
		"carillon_leap_events_total{result=probes} 1", "carillon_leap_events_total{result=bytes} 2", "carillon_leap_events_total{result=accepted} 3",
		"carillon_leap_events_total{result=failures} 4", "carillon_leap_events_total{result=rollback} 5", "carillon_leap_events_total{result=conflict} 6",
		"carillon_leap_events_total{result=cache_failures} 7", "carillon_leap_events_total{result=served} 8", "carillon_leap_events_total{result=rate_limited} 9",
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("metric missing %q", want)
		}
	}
	for _, private := range []string{st.LeapHash, st.LeapProvider.Name, st.LeapUpdate.Pending, st.LeapUpdate.Rejected} {
		if strings.Contains(body, private) {
			t.Errorf("unbounded metric label leaked %q", private)
		}
	}
}
