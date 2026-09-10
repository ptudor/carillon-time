package leap

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// A Seed is one of the fixed HTTPS publishers of leap-seconds.list. The
// configuration names a seed; nothing at runtime supplies a URL, and the
// TLS policy is not configurable. IERS issues Bulletin C and publishes the
// file first; NIST republishes it, historically weeks to months later, and
// dates its `#$` record differently (see docs/leap-distribution.md), so a
// deployment chooses one publisher and stays with it.
type Seed struct {
	Kind string // provider kind recorded with the accepted object
	URL  string
}

const (
	NISTURL = "https://tf.nist.gov/leap-seconds.list"
	IERSURL = "https://hpiers.obspm.fr/iers/bul/bulc/ntp/leap-seconds.list"
)

var seeds = map[string]Seed{
	"nist": {Kind: "nist", URL: NISTURL},
	"iers": {Kind: "iers", URL: IERSURL},
}

// SeedFor maps an acquisition mode to its publisher. Only "nist" and "iers"
// are seed modes; manual, peers and off have no HTTPS side.
func SeedFor(mode string) (Seed, bool) {
	s, ok := seeds[mode]
	return s, ok
}

// Validators are the publisher's cache validators for the last body we
// evaluated. Sending them back turns the daily check into a conditional
// request the server answers with 304 and no body when nothing changed.
// They are hints: a stale or missing validator costs one full 5 KiB
// download, never a wrong decision, because the body is validated on its
// own merits and a 304 never renews an expiry.
type Validators struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
}

const maxValidatorLength = 256

func (v Validators) valid() bool {
	return len(v.ETag) <= maxValidatorLength && len(v.LastModified) <= maxValidatorLength &&
		!strings.ContainsAny(v.ETag, "\r\n") && !strings.ContainsAny(v.LastModified, "\r\n")
}

// SeedResult is one completed request. Object is nil for 304 Not Modified.
type SeedResult struct {
	Object      *Object
	Validators  Validators
	NotModified bool
}

// SeedError is a response the publisher chose to send instead of the file.
// RetryAfter carries the server's own Retry-After when it sent one, bounded
// so a bogus header cannot park the seed for months.
type SeedError struct {
	Status     int
	RetryAfter time.Duration
	reason     *Rejection
}

func (e *SeedError) Error() string { return e.reason.Error() }
func (e *SeedError) Unwrap() error { return e.reason }

// Client errors other than these say the request itself is wrong or the
// resource is gone; retrying them more than daily only annoys the publisher.
func (e *SeedError) clientError() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != http.StatusTooManyRequests &&
		e.Status != http.StatusRequestTimeout && e.Status != http.StatusTooEarly
}

// maxRetryAfter bounds an honored Retry-After. A week is far shorter than
// the six-month expiry cycle, so obeying it costs nothing.
const maxRetryAfter = 7 * 24 * time.Hour

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	var wait time.Duration
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > int64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		wait = time.Duration(seconds) * time.Second
	} else if at, err := http.ParseTime(value); err == nil {
		wait = at.Sub(now)
	} else {
		return 0
	}
	return max(0, min(wait, maxRetryAfter))
}

// Fetch performs one conditional GET with the system trust store and
// certificate date validation. Redirects are refused, proxies from the
// environment are ignored, and the connection is closed afterwards: a
// once-a-day client has no business holding a server connection open.
func (s Seed) Fetch(ctx context.Context, userAgent string, v Validators) (SeedResult, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // acquisition policy never comes from the environment
	transport.DisableCompression = true
	transport.DisableKeepAlives = true
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("leap: redirects are forbidden") }}
	return fetchSeed(ctx, client, s.URL, userAgent, v)
}

// The injected client/URL are private test seams. Production only calls the
// fixed-URL entry point above, after establishing UTC.
func fetchSeed(ctx context.Context, client *http.Client, url, userAgent string, v Validators) (SeedResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return SeedResult{}, err
	}
	if req.URL.Scheme != "https" {
		return SeedResult{}, Reject("transport", "HTTPS is required")
	}
	if userAgent == "" {
		return SeedResult{}, Reject("transport", "a User-Agent is required")
	}
	req.Close = true
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("Accept-Encoding", "identity")
	conditional := false
	if v.valid() {
		if v.ETag != "" {
			req.Header.Set("If-None-Match", v.ETag)
			conditional = true
		}
		if v.LastModified != "" {
			req.Header.Set("If-Modified-Since", v.LastModified)
			conditional = true
		}
	}
	res, err := client.Do(req)
	if err != nil {
		return SeedResult{}, fmt.Errorf("leap: seed fetch %s: %w", req.URL.Host, err)
	}
	defer res.Body.Close()
	got := Validators{ETag: res.Header.Get("ETag"), LastModified: res.Header.Get("Last-Modified")}
	if !got.valid() {
		got = Validators{}
	}
	switch res.StatusCode {
	case http.StatusNotModified:
		// A 304 answers a conditional request. To an unconditional one it
		// is a broken server, not confirmation that our (absent) copy holds.
		if !conditional {
			return SeedResult{}, Reject("http", "304 Not Modified to an unconditional request")
		}
		// RFC 9110 lets a 304 omit the validators; keep the ones we sent.
		if got.ETag == "" {
			got.ETag = v.ETag
		}
		if got.LastModified == "" {
			got.LastModified = v.LastModified
		}
		return SeedResult{NotModified: true, Validators: got}, nil
	case http.StatusOK:
	default:
		return SeedResult{}, &SeedError{Status: res.StatusCode, RetryAfter: parseRetryAfter(res.Header.Get("Retry-After"), time.Now()),
			reason: &Rejection{"http", fmt.Sprintf("HTTP status %d from %s", res.StatusCode, req.URL.Host)}}
	}
	if enc := res.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		return SeedResult{}, Reject("transport", "encoded body refused")
	}
	if res.ContentLength > MaxFileSize {
		return SeedResult{}, Reject("size", "HTTP body exceeds 64 KiB")
	}
	o, err := ReadObject(res.Body)
	if err != nil {
		return SeedResult{}, err
	}
	return SeedResult{Object: o, Validators: got}, nil
}
