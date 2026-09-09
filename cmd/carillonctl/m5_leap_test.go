package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/carillon-time/internal/control"
	"github.com/ptudor/carillon-time/internal/leap"
)

func TestTrackingPrintsLeapReadinessAndCandidates(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "tracking.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	original := os.Stdout
	os.Stdout = file
	defer func() { os.Stdout = original }()
	updated := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	expiry := time.Date(2026, 12, 28, 0, 0, 0, 0, time.UTC)
	printTracking(&control.Tracking{State: "unsynced", Leap: "unsynchronized", LeapSource: "peer", LeapRequired: true, LeapReason: "UTC not established", LeapHash: "accepted-hash", LeapProvider: leap.Provider{Kind: "peer", Name: "home", KeyID: 7}, LeapUpdated: &updated, LeapExpiry: &expiry, LeapUpdate: leap.UpdateStatus{Mode: "peers", LastResult: "candidate refused", Pending: "pending-hash", LastRejection: "conflict"}})
	os.Stdout = original
	b, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(b), "\n") {
		lines = append(lines, strings.Join(strings.Fields(line), " "))
	}
	text := strings.Join(lines, "\n")
	for _, want := range []string{
		"Leap unsynchronized", "Leap source peer", "Leap ready false (table required: true)", "Leap readiness reason UTC not established",
		"Leap SHA-256 accepted-hash", "Leap provider peer home (key 7)", "Leap data updated 2026-09-01T00:00:00Z",
		"Leap acquisition peers: candidate refused", "Leap pending SHA-256 pending-hash", "Leap last rejection conflict", "Leap file expires 2026-12-28T00:00:00Z",
	} {
		if !strings.Contains("\n"+text, "\n"+want+"\n") {
			t.Errorf("tracking missing %q:\n%s", want, text)
		}
	}
}
