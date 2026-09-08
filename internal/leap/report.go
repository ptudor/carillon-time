package leap

import (
	"sync/atomic"
	"time"
)

// Counters have fixed names; packet-supplied identities never become labels.
type Counters struct {
	Probes        atomic.Uint64
	Bytes         atomic.Uint64
	Accepted      atomic.Uint64
	Failures      atomic.Uint64
	Rollback      atomic.Uint64
	Conflict      atomic.Uint64
	CacheFailures atomic.Uint64
	Served        atomic.Uint64
	RateLimited   atomic.Uint64
}

type Counts struct {
	Probes        uint64 `json:"probes"`
	Bytes         uint64 `json:"bytes"`
	Accepted      uint64 `json:"accepted"`
	Failures      uint64 `json:"failures"`
	Rollback      uint64 `json:"rollback"`
	Conflict      uint64 `json:"conflict"`
	CacheFailures uint64 `json:"cache_failures"`
	Served        uint64 `json:"served"`
	RateLimited   uint64 `json:"rate_limited"`
}

type UpdateStatus struct {
	Mode          string    `json:"mode"`
	LastFetch     time.Time `json:"last_fetch,omitzero"`
	LastResult    string    `json:"last_result,omitempty"`
	LastRejection string    `json:"last_rejection,omitempty"`
	Pending       string    `json:"pending_sha256,omitempty"`
	Rejected      string    `json:"rejected_sha256,omitempty"`
	Counts        Counts    `json:"counters"`
}

type Report struct {
	Counters Counters
	status   atomic.Pointer[UpdateStatus]
}

func (r *Report) Snapshot() UpdateStatus {
	if r == nil {
		return UpdateStatus{}
	}
	var s UpdateStatus
	if p := r.status.Load(); p != nil {
		s = *p
	}
	c := &r.Counters
	s.Counts = Counts{c.Probes.Load(), c.Bytes.Load(), c.Accepted.Load(), c.Failures.Load(), c.Rollback.Load(), c.Conflict.Load(), c.CacheFailures.Load(), c.Served.Load(), c.RateLimited.Load()}
	return s
}

func (r *Report) publish(s UpdateStatus) { r.status.Store(&s) }
