# M5 Fable 5.1 review fixes

Work date: 2026-09-09. Review: [REVIEW_M5_FABLE51.md](REVIEW_M5_FABLE51.md).
Starting commit: `2d3b07c`. Entries are updated with each completed checkpoint.
All clock tests use fake clocks.

## RM5-001 — Network-only client synchronization

Implemented the review's preferred policy, consistent with DESIGN §6.5 and
§6.8: missing fresh LI does not demote a plain client's discipline state,
stratum or kernel synchronization. Public LI remains 3 when unknown. Kernel
event flags are clear unless an earlier warning already armed a transition;
that event survives lost polls. Required-table hosts retain readiness gating.
The actuator's LI=3-to-STA_UNSYNC mapping is accounted for explicitly.

Validation: `go test -race ./internal/engine ./internal/clock` passes. New
regressions cover a tableless client after 129 seconds without replies, entry
to holdover, recovery on one reply, holdover expiry, and a split final-day
vote. The expired-table regression also checks that loss of fresh LI keeps an
already armed kernel insertion. Updated the older source-exit test to match
the documented plain-client holdover policy.

## RM5-006 — SETTLING leap status

A usable table can establish readiness and be exported during coarse UTC
settling, but its indicator and any latched-event override are published only
while SYNCED or HOLDOVER. SETTLING keeps LI=3 and cannot arm a kernel leap.

Validation: the same race run passes; the coarse-UTC export test now requires
public LI=3 as well as an unsynchronized kernel and no armed event.

RM5-007 boundary coverage is also added here: a delayed engine tick spanning
an insertion's repeated second and midnight resets each source once, records
execution, and preserves valid service without a daemon step.

## RM5-002 — Fixed NIST transport

The cloned HTTPS transport explicitly disables proxies. A regression sets
both proxy environment spellings and intercepts dialing before any DNS or
network I/O; the destination must remain `tf.nist.gov:443`. TLS verification,
redirect refusal, encoding, size and time limits remain enforced.

## RM5-003 — Transient probe retry

Three unanswered probes now produce an ordinary timeout rejection, using
15-minute to six-hour error backoff. Only authenticated evidence of missing
or unsupported CLPS capability receives the 24-hour capability floor.
The specification now distinguishes unanswered requests from legacy replies.

## RM5-005 — Recovery after RATE

A successful authenticated probe resumes normal four-second request spacing
after honoring the previous kiss's retry minimum. A new RATE aborts the
transfer and reinstates backoff. Successful fetches also clear retained
scheduler spacing. This prevents a transient RATE from making larger files
permanently untransferable while preserving the existing listener budgets.

Validation for RM5-002/003/005: `go test -race ./internal/leap` passes. An
in-memory authenticated transfer uses simulated time to download a file over
12 KiB after a 64-second RATE minimum, checking every chunk at four-second
spacing. Scheduling regressions exercise `Updater.Run` itself: configured
peer order and failover, transient timeout retry, unsupported capability's
24-hour floor, DENY/RSTR stops, and RATE retention followed by recovery.

Additional RM5-007 coverage in this checkpoint checks the 15-minute retry
after a failed cache commit, pending activation retry, and hourly manual
replacement with conflicting bytes or a temporarily missing file. The
original active table survives each rejection. Scheduler tests use standard
library simulated time and synchronized fixtures, without external hosts.

## Remaining work

The remaining RM5-007 coverage is in progress.

## RM5-004 — Operator recovery

Documented diagnosis and a complete offline acceptance-record reset in
`deploy/README.md` and `docs/leap-distribution.md`: stop and verify the daemon
has exited, preserve tracking/logs and a private audit copy of `state.json`,
remove its canonical entry by moving it, record the reason, correct sources
and trust, validate offline, restart and verify activation/readiness.

The instructions explain forward-wrong UTC bounds and how long they persist,
alternatives for manual conflicts/pending generations, and the consequences
of losing rollback, execution, cached objects and seed-check history. They
explicitly cover final-day rearming risk and state that no selective reset
subcommand exists. No host state was reset as part of this work.

## RM5-008 — Checkpoint write volume and shutdown

Routine UTC checkpoints now coalesce 15 minutes of progress: about 96 full
state writes per day instead of 1,440, plus acquisition/event writes. The
single atomic file continues coupling objects and rollback metadata. Observed
expiry and execution still request an immediate worker save. Shutdown drains
the network job and attempts a final save on the cache owner, with errors
logged and the existing process auxiliary deadline bounding a stuck disk.
The design/specification explicitly document up to 15 minutes of routine
checkpoint loss after an abrupt stop.

Validation: `go test -race ./internal/leap` passes. Regressions check the
15-minute cadence, no per-minute writes, immediate expiry/execution saves,
a final bound not yet seen by the worker ticker, retained seed-check state,
and a failed shutdown write preserving prior disk state and reporting the
failure. The reset documentation was checked against cache naming, locking,
validation, startup and persistence code.
