package monitor

import (
	"context"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/ptudor/carillon-time/internal/discipline"
	"github.com/ptudor/carillon-time/internal/engine"
	ntpserver "github.com/ptudor/carillon-time/internal/server"
)

func serverStatsZero() ntpserver.StatsSnapshot { return ntpserver.StatsSnapshot{} }

// TestAstra6JSONEncodingFailureIsNotSuccess covers RA6X-049 as it applies to
// the monitor. HTTP 200 was committed before encoding and the encoder's error
// was discarded, so non-finite state produced an empty success response.
func TestAstra6JSONEncodingFailureIsNotSuccess(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, 200, map[string]float64{"frequency": math.NaN()})
	if w.Code == 200 {
		t.Fatalf("encoding failure returned HTTP %d with body %q", w.Code, w.Body.String())
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("encoding failure returned HTTP %d, want 500", w.Code)
	}
}

// TestAstra6ValidJSONIsUnchanged checks the serialize-then-commit order did
// not alter the schema or the status code for ordinary data.
func TestAstra6ValidJSONIsUnchanged(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]any{"a": 1, "b": "two"})
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type %q", got)
	}
	var back map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &back); err != nil {
		t.Fatalf("body is not valid JSON: %v (%q)", err, w.Body.String())
	}
	if back["b"] != "two" {
		t.Fatalf("body %v", back)
	}
}

// TestAstra6CloseBeforeServeReleasesListener is the review's RA6X-052 probe.
// http.Server.Close only closes listeners it has been given, and Serve is
// what gives it this one, so closing before Serve left the port bound.
func TestAstra6CloseBeforeServeReleasesListener(t *testing.T) {
	s, err := Listen(Config{
		Listen: netip.MustParseAddrPort("127.0.0.1:0"),
		Allow:  []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		Status: func() *engine.Status { return &engine.Status{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	addr := s.Addr().String()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("Close before Serve retained the bound port: %v", err)
	}
	ln.Close()
	// Repeated cleanup is safe.
	_ = s.Close()
}

// TestAstra6MonitorLifecycle covers the rest of RA6X-052.
func TestAstra6MonitorLifecycle(t *testing.T) {
	newServer := func(t *testing.T) *Server {
		t.Helper()
		s, err := Listen(Config{
			Listen: netip.MustParseAddrPort("127.0.0.1:0"),
			Allow:  []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
			Status: func() *engine.Status { return &engine.Status{} },
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	t.Run("cancellation before Serve", func(t *testing.T) {
		s := newServer(t)
		addr := s.Addr().String()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := s.Serve(ctx); err != nil {
			t.Fatalf("Serve after cancellation: %v", err)
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("the port was not released: %v", err)
		}
		ln.Close()
	})

	t.Run("Serve joins shutdown before returning", func(t *testing.T) {
		s := newServer(t)
		addr := s.Addr().String()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Serve(ctx) }()

		// Make one request so the server is demonstrably up.
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("Serve did not return")
		}
		// Serve reporting finished must mean the port is free.
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("Serve returned before the listener was released: %v", err)
		}
		ln.Close()
	})
}

// TestAstra6FreshnessIsMonotonic covers RA6X-053. Snapshot age was computed by
// subtracting wall timestamps, so a backward clock step made an old snapshot
// look fresh: the negative age was clamped to zero.
func TestAstra6FreshnessIsMonotonic(t *testing.T) {
	now := time.Date(2026, 8, 23, 20, 0, 0, 0, time.UTC)
	st := testEngineStatus(now, discipline.StateSynced)

	// The clock steps backwards by an hour between publication and serving,
	// while the monotonic clock advances by ten seconds.
	h := healthOf(st, now.Add(-time.Hour), testPublishedMono+10)
	if h.SnapshotAgeSeconds != 10 {
		t.Fatalf("snapshot age %v across a backward step, want 10", h.SnapshotAgeSeconds)
	}
	if h.Status != "unhealthy" {
		t.Fatalf("a ten-second-old snapshot reported %q", h.Status)
	}

	// And forwards.
	h = healthOf(st, now.Add(time.Hour), testPublishedMono+1)
	if h.SnapshotAgeSeconds != 1 {
		t.Fatalf("snapshot age %v across a forward step, want 1", h.SnapshotAgeSeconds)
	}
	if h.Status != "healthy" {
		t.Fatalf("a one-second-old snapshot reported %q", h.Status)
	}
}

// TestAstra6InstanceIdentityIsStable covers the started_at half of RA6X-053:
// it describes one process instance and must not move when the clock steps.
func TestAstra6InstanceIdentityIsStable(t *testing.T) {
	now := time.Date(2026, 8, 23, 20, 0, 0, 0, time.UTC)
	started := now.Add(-time.Hour)
	st := testEngineStatus(now, discipline.StateSynced)

	first := SnapshotOf(st, serverStatsZero(), false, Metadata{}, now, testPublishedMono, started)
	// The clock steps forward by a day; uptime keeps its monotonic value.
	st.Now = now.Add(24 * time.Hour)
	second := SnapshotOf(st, serverStatsZero(), false, Metadata{}, st.Now, testPublishedMono, started)

	if !first.Instance.StartedAt.Equal(second.Instance.StartedAt) {
		t.Fatalf("started_at moved across a clock step: %v -> %v",
			first.Instance.StartedAt, second.Instance.StartedAt)
	}
	if first.Instance.UptimeSeconds != second.Instance.UptimeSeconds {
		t.Fatalf("uptime changed with the wall clock: %v -> %v",
			first.Instance.UptimeSeconds, second.Instance.UptimeSeconds)
	}
}
