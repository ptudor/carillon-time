package main

import (
	"strings"
	"testing"
	"time"

	"carillon/internal/config"
	"carillon/internal/leap"
)

func TestConfigurationWarnings(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.Refclocks = []config.Refclock{{Name: "gps", Type: "gps"}}
	warnings := configurationWarnings(cfg, nil, now)
	if len(warnings) != 1 || !strings.Contains(warnings[0].message, "no leapfile") || warnings[0].error {
		t.Fatalf("missing leapfile warning: %+v", warnings)
	}

	table := &leap.Table{Expiry: now.Add(20 * 24 * time.Hour)}
	warnings = configurationWarnings(cfg, table, now)
	if len(warnings) != 1 || !strings.Contains(warnings[0].message, "expires in") || warnings[0].error {
		t.Fatalf("expiring leapfile warning: %+v", warnings)
	}

	table.Expiry = now.Add(-time.Second)
	warnings = configurationWarnings(cfg, table, now)
	if len(warnings) != 1 || !strings.Contains(warnings[0].message, "expired") || !warnings[0].error {
		t.Fatalf("expired leapfile warning: %+v", warnings)
	}
}

func TestPublicServeWarnings(t *testing.T) {
	public := func(mutate func(*config.Config)) []configurationWarning {
		cfg := config.Default()
		cfg.Serve.Listen = []string{"0.0.0.0:123", "[::]:123"}
		cfg.Serve.Allow = []string{"0.0.0.0/0", "2000::/3"}
		if mutate != nil {
			mutate(cfg)
		}
		return serveWarnings(cfg)
	}

	// The defaults were chosen for a LAN, so an open ACL should say so and
	// name both settings that need revisiting.
	warnings := public(nil)
	if len(warnings) != 3 {
		t.Fatalf("warnings: %+v", warnings)
	}
	for i, want := range []string{"public internet", "rate_limit_pps", "recv_buffer"} {
		if !strings.Contains(warnings[i].message, want) || warnings[i].error {
			t.Fatalf("warning %d: %+v", i, warnings[i])
		}
	}
	if !strings.Contains(warnings[0].message, "0.0.0.0/0, 2000::/3") {
		t.Fatalf("the warning must quote the prefixes: %q", warnings[0].message)
	}

	// Tuned for a public server: still announced, but nothing left to fix.
	warnings = public(func(c *config.Config) {
		c.Serve.RateLimitPPS = 0.25
		c.Serve.RecvBuffer = 4 << 20
	})
	if len(warnings) != 1 || !strings.Contains(warnings[0].message, "public internet") {
		t.Fatalf("tuned public server: %+v", warnings)
	}

	// A LAN ACL says nothing at all, and neither does a disabled server.
	if got := public(func(c *config.Config) { c.Serve.Allow = []string{"192.168.1.0/24", "fd00::/8"} }); got != nil {
		t.Fatalf("private ACL warned: %+v", got)
	}
	if got := public(func(c *config.Config) { c.Serve.Allow = nil }); got != nil {
		t.Fatalf("disabled server warned: %+v", got)
	}
}

// TestAuxiliariesWaitNamesStragglers covers the other half of RF5X-033: the
// final wait has a deadline, and a component that misses it is named rather
// than waited on for ever.
func TestAuxiliariesWaitNamesStragglers(t *testing.T) {
	aux := newAuxiliaries()
	quick := make(chan struct{})
	stuck := make(chan struct{})
	aux.start("quick", func() { <-quick })
	aux.start("NTP server", func() { <-stuck })
	aux.start("control socket", func() { <-stuck })
	close(quick)

	start := time.Now()
	names := aux.wait(200 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("wait took %v; it must give up at the deadline", elapsed)
	}
	if len(names) != 2 || names[0] != "NTP server" || names[1] != "control socket" {
		t.Fatalf("stragglers %v, want the two blocked components in sorted order", names)
	}

	// Once they finish, wait reports nothing.
	close(stuck)
	if names := aux.wait(2 * time.Second); names != nil {
		t.Fatalf("stragglers after everything stopped: %v", names)
	}
}
