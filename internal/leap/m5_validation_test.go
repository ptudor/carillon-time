package leap

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCacheRejectsInvalidAcceptanceMetadata(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	o := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", "")
	conflict := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", "# different bytes\n")
	for _, tc := range []struct {
		name     string
		mutate   func(*diskState)
		mode     os.FileMode
		oversize bool
		want     string
	}{
		{"provider", func(d *diskState) { d.Active.Provider.Kind = "untrusted" }, 0600, false, "metadata"},
		{"peer without key", func(d *diskState) { d.Active.Provider = Provider{Kind: "peer", Name: "relay"} }, 0600, false, "metadata"},
		{"peer key out of range", func(d *diskState) { d.Active.Provider = Provider{Kind: "peer", Name: "relay", KeyID: 65536} }, 0600, false, "metadata"},
		{"pending conflict", func(d *diskState) {
			p := *d.Active
			p.Data, p.Manifest = conflict.Bytes(), conflict.manifest
			d.Pending = &p
		}, 0600, false, "inconsistent pending"},
		{"acceptance after UTC bound", func(d *diskState) { d.Active.Accepted = now.Add(time.Second) }, 0600, false, "acceptance after"},
		{"execution after UTC bound", func(d *diskState) { d.Executed = now.Add(time.Second) }, 0600, false, "execution record"},
		{"seed check after UTC bound", func(d *diskState) { d.LastCheck = now.Add(time.Second) }, 0600, false, "seed check date"},
		{"oversize", nil, 0600, true, "bounded regular file"},
		{"permissive mode", nil, 0644, false, "private"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, _ := testUpdater(t, "off", now)
			d := diskState{Version: 1, UTCbound: now, Active: &diskRecord{Manifest: o.manifest, Data: o.Bytes(), Provider: Provider{Kind: "file", Name: "local"}, Accepted: now}}
			if tc.mutate != nil {
				tc.mutate(&d)
			}
			b, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			if tc.oversize {
				b = append(b, []byte(strings.Repeat(" ", maxStateSize))...)
			}
			if err := u.cfg.Store.root.WriteFile("state.json", b, tc.mode); err != nil {
				t.Fatal(err)
			}
			if err := u.cfg.Store.root.Chmod("state.json", tc.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := u.cfg.Store.Load(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("invalid cache accepted or wrong rejection: %v", err)
			}
		})
	}
}

func TestCheckUpdateExpiryLimitAndBaseline(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	for _, extra := range []time.Duration{0, time.Second} {
		o := objectForTest(t, "2016-07-08T00:00:00Z", now.Add(400*24*time.Hour+extra).Format(time.RFC3339), "")
		err := CheckUpdate(nil, o, now)
		if (extra == 0 && err != nil) || (extra != 0 && Reason(err) != "date") {
			t.Fatalf("400-day expiry boundary +%s: %v", extra, err)
		}
	}
	old := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", "")
	renew := objectForTest(t, "2026-09-01T00:00:00Z", "2027-06-28T00:00:00Z", "")
	for _, body := range []string{
		strings.ReplaceAll(strings.ReplaceAll(string(renew.Bytes()), "2272060800 10", "2272060800 11"), "2287785600 11", "2287785600 12"),
		strings.ReplaceAll(string(renew.Bytes()), "2272060800 10", "2256163200 10"),
	} {
		changed, err := NewObject([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckUpdate(old, changed, now); Reason(err) != "history" {
			t.Fatalf("changed baseline accepted: %v", err)
		}
	}
	ancient := objectForTest(t, "1971-12-01T00:00:00Z", "1972-12-28T00:00:00Z", "")
	if err := CheckUpdate(nil, ancient, ancient.table.Baseline.Add(-time.Second)); Reason(err) != "date" {
		t.Fatalf("UTC before baseline accepted: %v", err)
	}
}
