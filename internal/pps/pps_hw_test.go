//go:build (linux || freebsd) && hwtest

package pps

import (
	"errors"
	"os"
	"testing"
	"time"
)

// TestDevice performs the non-destructive target-host PPS acceptance check:
// open/configure the API, verify an edge arrives, and close it. On Linux a
// tty path temporarily attaches N_PPS; a /dev/ppsN path is used directly.
//
//	CARILLON_HW_TESTS=1 CARILLON_PPS_DEVICE=/dev/pps0 \
//	  go test -tags hwtest ./internal/pps -run TestDevice
func TestDevice(t *testing.T) {
	if os.Getenv("CARILLON_HW_TESTS") != "1" {
		t.Skip("set CARILLON_HW_TESTS=1 on a target host to run")
	}
	path := os.Getenv("CARILLON_PPS_DEVICE")
	if path == "" {
		t.Fatal("set CARILLON_PPS_DEVICE to the tty or /dev/ppsN")
	}
	edgeName := os.Getenv("CARILLON_PPS_EDGE")
	if edgeName == "" {
		edgeName = "assert"
	}
	edge, err := ParseEdge(edgeName)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(path, edge)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	s, err := r.Fetch(3 * time.Second)
	if errors.Is(err, ErrTimeout) {
		t.Fatalf("no %s PPS edge within 3 seconds", edge)
	}
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if s.Sequence == 0 || s.Time.IsZero() {
		t.Fatalf("bad sample: %+v", s)
	}
	t.Logf("sequence=%d time=%s", s.Sequence, s.Time.UTC().Format(time.RFC3339Nano))
}
