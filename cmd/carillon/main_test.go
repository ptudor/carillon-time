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
