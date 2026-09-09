package engine

import (
	"context"
	"math"
	"time"

	"github.com/ptudor/carillon-time/internal/discipline"
	"github.com/ptudor/carillon-time/internal/leap"
	"github.com/ptudor/carillon-time/internal/ntp"
)

var leapMissingRefID = ntp.RefID{'X', 'L', 'E', 'P'}

// applyLeap gates effective service while leaving the pure discipline free
// to acquire UTC, settle and qualify PPS. All external consumers see the
// resulting State/LI; ClockState preserves the loop's separate progress.
func (e *Engine) applyLeap(st *discipline.Status, wall time.Time) {
	wasReady, oldReason := e.leapReady, e.leapReason
	defer func() {
		if !e.leapReady && (wasReady || (e.leapReason == "expired" && oldReason != "expired")) {
			e.log.Warn("leap authority unavailable", "reason", e.leapReason, "required", e.cfg.LeapRequired)
		} else if e.leapReady && !wasReady {
			e.log.Info("leap authority ready", "source", e.leapAuthority)
		}
	}()
	e.clockState = st.State
	synced := st.State == discipline.StateSynced || st.State == discipline.StateHoldover
	coarse := st.State == discipline.StateSettling && st.SystemSource != "" && math.Abs(st.Offset)+math.Abs(st.Pending) < discipline.PPSGuard
	e.timeKnown = (synced || coarse) && !wall.Before(e.startupUTCbound)
	// A one-second backward movement is possible at a positive kernel leap.
	// The greatest established UTC still decides expiry, so it cannot renew
	// an expired object's authority. Larger setbacks require reacquisition.
	if wall.Add(time.Second).Before(e.utcBound) {
		e.timeKnown = false
	}
	if e.timeKnown && wall.After(e.utcBound) {
		e.utcBound = wall
	}
	validAt := wall
	if e.utcBound.After(validAt) {
		validAt = e.utcBound
	}
	e.leapReady = false
	e.leapReason = "missing table"
	e.leapAuthority = "unknown"
	var table *leap.Table
	if e.activeLeap != nil {
		table = e.activeLeap.Object.Table()
		if err := leap.CheckUpdate(nil, e.activeLeap.Object, validAt); err != nil {
			e.leapReason = leap.Reason(err)
			table = nil
		}
	} else if e.cfg.LeapTable != nil {
		// Internal compatibility for the deterministic table fixtures. The
		// daemon uses durable Object records exclusively.
		table = e.cfg.LeapTable
		if !validAt.Before(table.Expiry) {
			table = nil
			e.leapReason = "expired"
		}
	}
	if !e.timeKnown {
		e.leapReason = "UTC not established"
	}
	expiring := table != nil && table.Expiry.Sub(validAt) <= 30*24*time.Hour
	if expiring && !e.leapExpiring {
		e.log.Warn("leap table expires within 30 days", "expires", table.Expiry)
	}
	e.leapExpiring = expiring
	if table != nil && e.timeKnown {
		e.leapReady = true
		e.leapAuthority = "file"
		if e.activeLeap != nil {
			e.leapAuthority = e.activeLeap.Provider.Kind
		}
		indicator := table.Indicator(wall)
		disagree := st.Leap != ntp.LeapUnsync && st.Leap != indicator
		if disagree != e.leapDisagreement {
			if disagree {
				e.log.Warn("leap table and fresh survivor LI disagree", "table", indicator.String(), "survivors", st.Leap.String())
			} else {
				e.log.Info("leap source disagreement cleared")
			}
		}
		e.leapDisagreement = disagree
		if synced {
			st.Leap = indicator
		}
	} else if !e.cfg.LeapRequired && st.Leap != ntp.LeapUnsync && e.timeKnown {
		e.leapReady = true
		e.leapAuthority = "sources"
	}
	if table == nil || !e.timeKnown {
		e.leapDisagreement = false
	}
	if e.leapReady {
		e.leapReason = ""
		// Once armed, an update cannot cancel the event. Consistent table
		// renewals proceed; an LI withdrawal does not silently disarm it.
		if synced {
			e.notePendingLeap(st.Leap, wall)
			if !e.pendingLeap.IsZero() && wall.Before(e.pendingLeap) {
				st.Leap = e.pendingLeapKind
			}
			if !e.executedLeap.IsZero() && wall.Before(e.executedLeap) {
				st.Leap = ntp.LeapNone
			}
		}
	} else if synced && e.cfg.LeapRequired {
		st.State = discipline.StateUnsynced
		st.Leap = ntp.LeapUnsync
		st.Stratum = 16
		st.RefID = leapMissingRefID
	}
}

// LeapView is safe for the worker. Snapshot age is checked independently of
// the engine so a stuck engine cannot authorize updates indefinitely.
func (e *Engine) LeapView() leap.View {
	s := e.Status()
	return leap.View{Now: e.clk.Now(), Known: s.TimeKnown && e.clk.Monotonic()-s.PublishedMono <= 30, UTCbound: s.UTCbound, Executed: s.LeapExecuted}
}

// CurrentLeap is eligible for export even when the time service has lost
// synchronization, provided UTC remains established by the fresh engine
// snapshot. It never promotes an expired or provisional generation.
func (e *Engine) CurrentLeap() *leap.Object {
	s := e.Status()
	if e.clk.Monotonic()-s.PublishedMono > 30 {
		return nil
	}
	return s.LeapObject
}

func (e *Engine) checkLeap(r *leap.Record) error {
	if r == nil || r.Object == nil {
		return leap.Reject("metadata", "missing candidate")
	}
	wall := e.clk.Now()
	st := e.sys.Status(e.processing(e.clk.Monotonic()))
	e.applyLeap(&st, wall)
	if !e.timeKnown {
		return leap.Reject("time_unknown", "UTC is not established")
	}
	if e.utcBound.After(wall) {
		wall = e.utcBound
	}
	if err := leap.CheckUpdate(e.leapAnchor, r.Object, wall); err != nil {
		return err
	}
	if !e.pendingLeap.IsZero() {
		found := false
		for _, tr := range r.Object.Table().Transitions {
			if tr.At.Equal(e.pendingLeap) && tr.Leap == e.pendingLeapKind {
				found = true
				break
			}
		}
		if !found {
			return leap.Reject("armed", "candidate removes or changes the armed leap")
		}
	}
	return nil
}

func (e *Engine) ApproveLeap(ctx context.Context, r *leap.Record) error {
	return e.leapRequest(ctx, r, false)
}
func (e *Engine) ActivateLeap(ctx context.Context, r *leap.Record) error {
	return e.leapRequest(ctx, r, true)
}

func (e *Engine) leapRequest(ctx context.Context, r *leap.Record, activate bool) error {
	done := make(chan error, 1)
	f := func() error {
		if err := ctx.Err(); err != nil {
			done <- err
			return nil
		}
		e.crossLeap(e.processing(e.clk.Monotonic()))
		if err := e.checkLeap(r); err != nil {
			done <- err
			return nil
		}
		if !activate {
			e.approvedLeap = r.Object.Manifest()
			done <- nil
			return nil
		}
		if r.Object.Manifest() != e.approvedLeap {
			done <- leap.Reject("generation", "activation does not identify the approved object")
			return nil
		}
		e.activeLeap = r
		e.leapAnchor = r.Object
		e.approvedLeap = leap.Manifest{}
		// This path may update kernel flags. An actuator failure is returned
		// to Run as fatal as well as to the waiting worker.
		err := e.handle(discipline.Result{}, e.processing(e.clk.Monotonic()))
		done <- err
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case e.reqs <- f:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}
