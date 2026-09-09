package control

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ptudor/carillon-time/internal/discipline"
	"github.com/ptudor/carillon-time/internal/engine"
	"github.com/ptudor/carillon-time/internal/leap"
	"github.com/ptudor/carillon-time/internal/ntp"
)

func TestTrackingLeapValidityUsesUTCBound(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name          string
		expiry, bound time.Time
		valid         bool
	}{
		{"absent", time.Time{}, now, false},
		{"unexpired", now.Add(time.Hour), now, true},
		{"expiry reached", now, now, false},
		{"clock behind saved bound", now.Add(time.Hour), now.Add(2 * time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &engine.Status{Status: discipline.Status{State: discipline.StateSettling, Leap: ntp.LeapUnsync}, Now: now, UTCbound: tc.bound, LeapExpiry: tc.expiry, LeapRequired: true, LeapReason: "UTC not established", LeapHash: "active-hash", LeapProvider: leap.Provider{Kind: "peer", Name: "home", KeyID: 7}, LeapUpdate: leap.UpdateStatus{Pending: "pending-hash", Rejected: "rejected-hash", LastRejection: "conflict"}}
			tr := TrackingOf(st)
			if tr.LeapValid != tc.valid || tr.LeapReady || tr.Leap != "unsynchronized" {
				t.Fatalf("table expiry and readiness conflated: %+v", tr)
			}
			b, err := json.Marshal(tr)
			if err != nil {
				t.Fatal(err)
			}
			var decoded Tracking
			if err := json.Unmarshal(b, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.LeapProvider != st.LeapProvider || decoded.LeapUpdate != st.LeapUpdate || decoded.LeapHash != st.LeapHash || decoded.LeapReason != st.LeapReason {
				t.Fatalf("JSON lost leap diagnostics: %s", b)
			}
			if tc.expiry.IsZero() && decoded.LeapExpiry != nil {
				t.Fatal("absent table acquired a date")
			}
		})
	}
}
