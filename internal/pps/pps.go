// Package pps exposes the RFC 2783 kernel PPS API on Linux and FreeBSD.
// Kernel timestamps are returned unchanged; user-space scheduling is not in
// the timestamp path.
package pps

import (
	"errors"
	"fmt"
	"time"
)

// Edge selects which modem-control transition marks the second boundary.
type Edge uint8

const (
	Assert Edge = iota + 1
	Clear
)

func (e Edge) String() string {
	switch e {
	case Assert:
		return "assert"
	case Clear:
		return "clear"
	default:
		return "unknown"
	}
}

// ParseEdge validates a configured edge name.
func ParseEdge(s string) (Edge, error) {
	switch s {
	case "assert":
		return Assert, nil
	case "clear":
		return Clear, nil
	default:
		return 0, fmt.Errorf("edge %q is not assert or clear", s)
	}
}

// Sample is one kernel-captured PPS transition.
type Sample struct {
	Sequence uint32
	Time     time.Time
}

// Reader is an open PPS source.
type Reader interface {
	Fetch(timeout time.Duration) (Sample, error)
	Close() error
}

var (
	// ErrTimeout means no new pulse arrived before the requested timeout.
	ErrTimeout = errors.New("PPS fetch timed out")

	// ErrUnsupportedPlatform means this build host has no PPS backend.
	ErrUnsupportedPlatform = errors.New("PPS is unsupported on this platform")
)
