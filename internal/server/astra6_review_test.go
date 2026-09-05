package server

import (
	"math"
	"net/netip"
	"testing"
	"time"
)

// TestAstra6HandlerRejectsUnboundedLimits covers RA6X-035 at the public
// constructor boundary. NewHandler used `!(v > 0)` and `!(v >= 1)`, which
// reject a NaN but accept +Inf — an infinite rate or burst is no rate
// limiting at all, and config.Validate has not necessarily run.
func TestAstra6HandlerRejectsUnboundedLimits(t *testing.T) {
	base := func() Config {
		return Config{
			Allow:        []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
			RateLimitPPS: 8,
			RateBurst:    16,
			Status:       testStatus,
			Now:          func() time.Time { return testWall },
		}
	}
	if _, err := NewHandler(base()); err != nil {
		t.Fatalf("setup: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*Config)
		ok     bool
	}{
		{"rate +inf", func(c *Config) { c.RateLimitPPS = math.Inf(1) }, false},
		{"rate nan", func(c *Config) { c.RateLimitPPS = math.NaN() }, false},
		{"rate too large", func(c *Config) { c.RateLimitPPS = 1e30 }, false},
		{"rate zero", func(c *Config) { c.RateLimitPPS = 0 }, false},
		{"burst +inf", func(c *Config) { c.RateBurst = math.Inf(1) }, false},
		{"burst nan", func(c *Config) { c.RateBurst = math.NaN() }, false},
		{"burst too large", func(c *Config) { c.RateBurst = 1e30 }, false},
		{"burst below one", func(c *Config) { c.RateBurst = 0.5 }, false},
		{"rate at the maximum", func(c *Config) { c.RateLimitPPS = maxRateLimit }, true},
		{"burst at the maximum", func(c *Config) { c.RateBurst = maxRateLimit }, true},
		{"burst one", func(c *Config) { c.RateBurst = 1 }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := base()
			c.mutate(&cfg)
			_, err := NewHandler(cfg)
			if c.ok && err != nil {
				t.Fatalf("valid limits refused: %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("unbounded limits accepted")
			}
		})
	}
}
