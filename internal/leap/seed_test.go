package leap

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestSeedModesAndFixedURLsIgnoreProxyEnvironment(t *testing.T) {
	for _, mode := range []string{"off", "manual", "peers", "", "iana"} {
		if _, ok := SeedFor(mode); ok {
			t.Fatalf("%q must not be a seed mode", mode)
		}
	}
	for _, name := range []string{"HTTPS_PROXY", "https_proxy"} {
		t.Setenv(name, "http://127.0.0.1:1")
	}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
	// Stop at DialContext so this tests the real fixed-URL entry point
	// without consulting DNS or making any external connection.
	original := http.DefaultTransport
	transport := original.(*http.Transport).Clone()
	// Avoid ProxyFromEnvironment's process-wide cache from earlier tests.
	transport.Proxy = func(*http.Request) (*url.URL, error) { return url.Parse(os.Getenv("HTTPS_PROXY")) }
	dialed := make(chan string, 1)
	stop := errors.New("test stopped before network I/O")
	transport.DialContext = func(_ context.Context, _, address string) (net.Conn, error) {
		dialed <- address
		return nil, stop
	}
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = original; transport.CloseIdleConnections() })
	for mode, host := range map[string]string{"nist": "tf.nist.gov:443", "iers": "hpiers.obspm.fr:443"} {
		seed, ok := SeedFor(mode)
		if !ok || seed.Kind != mode {
			t.Fatalf("%s: seed %+v", mode, seed)
		}
		if _, err := seed.Fetch(context.Background(), "carillon/test", Validators{}); !errors.Is(err, stop) {
			t.Fatalf("%s: unexpected fetch result: %v", mode, err)
		}
		if address := <-dialed; address != host {
			t.Fatalf("%s: environment redirected the fixed-URL fetch to %q", mode, address)
		}
	}
}

type recordedRequests struct {
	mu      sync.Mutex
	headers []http.Header
}

func (r *recordedRequests) add(h http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.headers = append(r.headers, h.Clone())
}

func (r *recordedRequests) all() []http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]http.Header(nil), r.headers...)
}

func TestSeedFetchIsAPoliteConditionalClient(t *testing.T) {
	o := objectForTest(t, "2026-07-06T07:44:57Z", "2027-06-28T00:00:00Z", "# IERS\n")
	const etag = `"13c9-655ec9478b1c2"`
	const modified = "Mon, 06 Jul 2026 07:54:11 GMT"
	var requests recordedRequests
	https := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.add(r.Header)
		if r.Header.Get("If-None-Match") == etag && r.Header.Get("If-Modified-Since") == modified {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", modified)
		w.Header().Set("Set-Cookie", "__cf_bm=opaque; HttpOnly; Secure; Path=/")
		w.Write(o.Bytes())
	}))
	defer https.Close()
	client := https.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }
	ctx := context.Background()
	const ua = "carillon/test (+https://github.com/ptudor/carillon-time; NTP daemon leap-seconds seed)"

	if _, err := fetchSeed(ctx, client, https.URL, "", Validators{}); Reason(err) != "transport" {
		t.Fatalf("anonymous request was not refused: %v", err)
	}
	first, err := fetchSeed(ctx, client, https.URL, ua, Validators{})
	if err != nil || first.NotModified || first.Object == nil || first.Object.manifest != o.manifest {
		t.Fatalf("first fetch: %+v %v", first, err)
	}
	if first.Validators != (Validators{ETag: etag, LastModified: modified}) {
		t.Fatalf("validators not captured: %+v", first.Validators)
	}
	second, err := fetchSeed(ctx, client, https.URL, ua, first.Validators)
	if err != nil || !second.NotModified || second.Object != nil || second.Validators != first.Validators {
		t.Fatalf("conditional fetch: %+v %v", second, err)
	}
	sent := requests.all()
	if len(sent) != 2 {
		t.Fatalf("%d requests reached the publisher, want 2", len(sent))
	}
	for i, h := range sent {
		if h.Get("User-Agent") != ua || h.Get("Accept-Encoding") != "identity" || h.Get("Accept") != "text/plain" {
			t.Fatalf("request %d headers: %v", i, h)
		}
		if h.Get("Cookie") != "" {
			t.Fatalf("request %d returned the publisher's cookie", i)
		}
	}
	if sent[0].Get("If-None-Match") != "" || sent[0].Get("If-Modified-Since") != "" {
		t.Fatalf("first request was conditional without validators: %v", sent[0])
	}
	if sent[1].Get("If-None-Match") != etag || sent[1].Get("If-Modified-Since") != modified {
		t.Fatalf("second request did not send validators: %v", sent[1])
	}
}

func TestSeedFetchReportsPublisherInstructions(t *testing.T) {
	var mu sync.Mutex
	status, retry := http.StatusOK, ""
	https := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if retry != "" {
			w.Header().Set("Retry-After", retry)
		}
		w.WriteHeader(status)
	}))
	defer https.Close()
	client := https.Client()
	for _, tc := range []struct {
		status     int
		retry      string
		wantRetry  time.Duration
		clientSide bool
	}{
		{http.StatusTooManyRequests, "120", 2 * time.Minute, false},
		{http.StatusTooManyRequests, "99999999", maxRetryAfter, false},
		{http.StatusTooManyRequests, "-5", 0, false},
		{http.StatusServiceUnavailable, "", 0, false},
		{http.StatusServiceUnavailable, "garbage", 0, false},
		{http.StatusInternalServerError, "", 0, false},
		{http.StatusNotFound, "", 0, true},
		{http.StatusForbidden, "", 0, true},
		{http.StatusGone, "", 0, true},
		{http.StatusRequestTimeout, "", 0, false},
	} {
		mu.Lock()
		status, retry = tc.status, tc.retry
		mu.Unlock()
		_, err := fetchSeed(context.Background(), client, https.URL, "carillon/test", Validators{})
		var se *SeedError
		if !errors.As(err, &se) || se.Status != tc.status || Reason(err) != "http" {
			t.Fatalf("status %d: %v", tc.status, err)
		}
		if se.RetryAfter != tc.wantRetry || se.clientError() != tc.clientSide {
			t.Fatalf("status %d Retry-After %q: retry %s client %t", tc.status, tc.retry, se.RetryAfter, se.clientError())
		}
	}
}

func TestParseRetryAfterDates(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	if got := parseRetryAfter(now.Add(90*time.Minute).Format(http.TimeFormat), now); got != 90*time.Minute {
		t.Fatalf("HTTP-date: %s", got)
	}
	if got := parseRetryAfter(now.Add(-time.Hour).Format(http.TimeFormat), now); got != 0 {
		t.Fatalf("past HTTP-date: %s", got)
	}
	if got := parseRetryAfter(now.Add(30*24*time.Hour).Format(http.TimeFormat), now); got != maxRetryAfter {
		t.Fatalf("distant HTTP-date: %s", got)
	}
}

func TestSeedScheduleHonorsRetryAfterAndClientErrorFloor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
		u, c := testUpdater(t, "iers", now)
		o := objectForTest(t, "2026-07-06T07:44:57Z", "2027-06-28T00:00:00Z", "# IERS\n")
		responses := []error{
			&SeedError{Status: http.StatusTooManyRequests, RetryAfter: 2 * time.Hour, reason: &Rejection{"http", "HTTP status 429"}},
			&SeedError{Status: http.StatusNotFound, reason: &Rejection{"http", "HTTP status 404"}},
		}
		var recorded scheduledCalls[time.Time]
		u.fetchSeed = func(_ context.Context, v Validators) (SeedResult, error) {
			n := recorded.add(time.Now())
			if n <= len(responses) {
				return SeedResult{}, responses[n-1]
			}
			return SeedResult{Object: o, Validators: Validators{ETag: `"x"`}}, nil
		}
		stop := runScheduledUpdater(t, u)
		defer stop()
		calls := recorded.snapshot()
		if len(calls) != 1 || u.cfg.Report.Snapshot().LastRejection != "http" {
			t.Fatalf("initial seed check: %v %+v", calls, u.cfg.Report.Snapshot())
		}
		// Retry-After 2 h outranks the 15-minute backoff, with jitter only
		// ever delaying further.
		time.Sleep(2*time.Hour - time.Second)
		synctest.Wait()
		if len(recorded.snapshot()) != 1 {
			t.Fatal("retried inside the publisher's Retry-After")
		}
		time.Sleep(13 * time.Minute)
		synctest.Wait()
		calls = recorded.snapshot()
		if len(calls) != 2 {
			t.Fatalf("Retry-After not honored within its jitter window: %v", calls)
		}
		// A 404 is not retried more than daily.
		time.Sleep(24*time.Hour - time.Second - time.Since(calls[1]))
		synctest.Wait()
		if len(recorded.snapshot()) != 2 {
			t.Fatal("client error retried inside a day")
		}
		time.Sleep(2*time.Hour + 25*time.Minute)
		synctest.Wait()
		calls = recorded.snapshot()
		if len(calls) != 3 || c.active.Load() == nil || c.active.Load().Object.manifest != o.manifest {
			t.Fatalf("daily retry did not run or activate: %v", calls)
		}
		seed, err := u.cfg.Store.LoadSeed()
		if err != nil || seed.Validators.ETag != `"x"` || !seed.LastAttempt.Equal(now) {
			t.Fatalf("seed bookkeeping after success: %+v %v", seed, err)
		}
		// Success returns to the daily cadence, ±10%.
		time.Sleep(21*time.Hour + 30*time.Minute)
		synctest.Wait()
		if len(recorded.snapshot()) != 3 {
			t.Fatal("successful check re-polled inside a day")
		}
		time.Sleep(5 * time.Hour)
		synctest.Wait()
		if len(recorded.snapshot()) != 4 {
			t.Fatal("daily check did not run")
		}
	})
}

func TestSeedRestartHonorsAttemptFloorAndKeepsValidators(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
		u, c := testUpdater(t, "nist", now)
		saved := SeedState{Validators: Validators{ETag: `"e"`, LastModified: "Mon, 06 Jul 2026 07:54:11 GMT"}, LastAttempt: now.Add(-5 * time.Minute)}
		if err := u.cfg.Store.SaveSeed(saved); err != nil {
			t.Fatal(err)
		}
		restarted := NewUpdater(UpdaterConfig{Mode: "nist", Store: u.cfg.Store, Controller: c, Report: new(Report)})
		var got scheduledCalls[Validators]
		restarted.fetchSeed = func(_ context.Context, v Validators) (SeedResult, error) {
			got.add(v)
			return SeedResult{NotModified: true, Validators: v}, nil
		}
		stop := runScheduledUpdater(t, restarted)
		defer stop()
		if len(got.snapshot()) != 0 {
			t.Fatal("restart asked the publisher again inside the attempt floor")
		}
		time.Sleep(10*time.Minute + time.Second)
		synctest.Wait()
		calls := got.snapshot()
		if len(calls) != 1 || calls[0] != saved.Validators {
			t.Fatalf("validators did not survive restart: %+v", calls)
		}
		if s := restarted.cfg.Report.Snapshot(); s.LastResult != "not modified" || s.LastRejection != "" {
			t.Fatalf("304 status: %+v", s)
		}
		// 304 confirms nothing changed; it cannot create or renew authority.
		if c.active.Load() != nil {
			t.Fatal("304 created a table")
		}
	})
}

func TestSeedBookkeepingRejectsCorruptionAndStartsClean(t *testing.T) {
	u, _ := testUpdater(t, "iers", time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC))
	dir := u.cfg.Store.root.Name()
	for name, body := range map[string]string{
		"unknown field":    `{"version":1,"etag":"x","proxy":"http://evil"}`,
		"bad version":      `{"version":2}`,
		"header injection": `{"version":1,"etag":"x\r\nX-Injected: 1"}`,
		"trailing data":    `{"version":1}{"version":1}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, "seed.json"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := u.cfg.Store.LoadSeed(); err == nil {
			t.Fatalf("%s accepted", name)
		}
		clean := NewUpdater(UpdaterConfig{Mode: "iers", Store: u.cfg.Store, Controller: u.cfg.Controller, Report: new(Report)})
		if clean.seedState != (SeedState{}) {
			t.Fatalf("%s: corrupt bookkeeping was not discarded", name)
		}
	}
	if err := os.Remove(filepath.Join(dir, "seed.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "state.json"), filepath.Join(dir, "seed.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := u.cfg.Store.LoadSeed(); err == nil {
		t.Fatal("symlinked seed bookkeeping was followed")
	}
	if err := u.cfg.Store.SaveSeed(SeedState{Validators: Validators{ETag: "x\n"}}); err == nil {
		t.Fatal("invalid validators persisted")
	}
}
