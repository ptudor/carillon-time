package monitor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/carillon-time/internal/discipline"
	"github.com/ptudor/carillon-time/internal/engine"
)

func startTestServer(t *testing.T, allow []netip.Prefix, state discipline.State, mutate ...func(*engine.Status)) (*Server, string) {
	t.Helper()
	now := time.Now().UTC()
	status := func() *engine.Status {
		st := testEngineStatus(now, state)
		for _, m := range mutate {
			m(st)
		}
		return st
	}
	s, err := Listen(Config{
		Listen:   netip.MustParseAddrPort("127.0.0.1:0"),
		Allow:    allow,
		Metadata: Metadata{ID: "test"},
		Status:   status,
		Now:      func() time.Time { return now.Add(time.Second) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("monitor server did not stop")
		}
	})
	return s, "http://" + s.Addr().String()
}

func TestStatusAndHealthHandlers(t *testing.T) {
	_, base := startTestServer(t, []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, discipline.StateSynced)

	resp, err := http.Get(base + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status response: %s %q", resp.Status, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security headers missing: %v", resp.Header)
	}
	var snapshot Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Schema != schemaV1 || snapshot.Health.Status != "healthy" || snapshot.Instance.ID != "test" {
		t.Fatalf("snapshot: %+v", snapshot)
	}

	health, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("health: %s", health.Status)
	}

	metrics, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metricsBody, err := io.ReadAll(metrics.Body)
	metrics.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`carillon_state{state="synced"} 1`,
		`carillon_build_info{version="test-version"} 1`,
		`carillon_server_requests_total{family="ipv4",result="served"} 0`,
		`carillon_server_requests_total{family="ipv6",result="martian"} 0`,
	} {
		if !strings.Contains(string(metricsBody), want) {
			t.Errorf("metrics missing %q:\n%s", want, metricsBody)
		}
	}

	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/status", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	methodResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	methodResp.Body.Close()
	if methodResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", methodResp.StatusCode)
	}
}

func TestUnhealthyAndDenied(t *testing.T) {
	_, unhealthyBase := startTestServer(t, []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, discipline.StateUnsynced)
	resp, err := http.Get(unhealthyBase + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unhealthy status = %d", resp.StatusCode)
	}

	_, deniedBase := startTestServer(t, []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, discipline.StateSynced)
	denied, err := http.Get(deniedBase + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(denied.Body)
	denied.Body.Close()
	if denied.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "Forbidden") {
		t.Fatalf("denied: %s %q", denied.Status, body)
	}
}

// TestDegradedHealthzStaysOK checks that only unhealthy fails the probe. A
// degraded instance is still serving usable time, so a load balancer must
// keep it; the reason for the degradation is carried in the body instead.
func TestDegradedHealthzStaysOK(t *testing.T) {
	tests := []struct {
		name   string
		state  discipline.State
		mutate func(*engine.Status)
		want   int
		status string
		reason string
	}{
		{"prefer lost", discipline.StateSynced,
			func(st *engine.Status) { st.PreferLost = true },
			http.StatusOK, statusDegraded, "preferred_source_lost"},
		{"holdover", discipline.StateHoldover, nil,
			http.StatusOK, statusDegraded, "holdover"},
		{"settling", discipline.StateSettling, nil,
			http.StatusServiceUnavailable, statusUnhealthy, "settling"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mutate []func(*engine.Status)
			if tt.mutate != nil {
				mutate = append(mutate, tt.mutate)
			}
			_, base := startTestServer(t, []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, tt.state, mutate...)

			resp, err := http.Get(base + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			var probe struct {
				Health Health `json:"health"`
			}
			err = json.NewDecoder(resp.Body).Decode(&probe)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tt.want {
				t.Fatalf("healthz = %d, want %d (health %+v)", resp.StatusCode, tt.want, probe.Health)
			}
			if probe.Health.Status != tt.status || !slices.Contains(probe.Health.Reasons, tt.reason) {
				t.Fatalf("health body %+v, want %q with reason %q", probe.Health, tt.status, tt.reason)
			}
		})
	}
}
