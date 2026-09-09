# M5 Fable 5.1 review fixes

Work date: 2026-09-09. Review: [REVIEW_M5_FABLE51.md](REVIEW_M5_FABLE51.md).
Starting commit: `2d3b07c`. All eight findings are addressed below, across
regular commits. All clock tests use fake clocks; network fixtures use
loopback or in-memory connections.

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

Health also preserves this distinction: an expired optional table remains a
degraded warning even when fresh LI disappears; it does not turn a plain
client's healthy clock or ordinary holdover into an unhealthy leap state.
The required-table expiration test remains unhealthy.

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
12,000 bytes after a 64-second RATE minimum, checking every chunk at four-second
spacing. Scheduling regressions exercise `Updater.Run` itself: configured
peer order and failover, transient timeout retry, unsupported capability's
24-hour floor, DENY/RSTR stops, and RATE retention followed by recovery.

Additional RM5-007 coverage in this checkpoint checks the 15-minute retry
after a failed cache commit, pending activation retry, and hourly manual
replacement with conflicting bytes or a temporarily missing file. The
original active table survives each rejection. Scheduler tests use standard
library simulated time and synchronized fixtures, without external hosts.

## RM5-007 — Coverage gaps

Added all listed coverage areas:

- Plain-client lost polls, holdover/recovery/expiry and split votes; a delayed
  insertion crossing with one filter reset per source and no daemon step.
- `Updater.Run` peer order, timeout/unsupported/RATE/denial schedules,
  activation retries and cache-write retry timing, as detailed above.
- Health reasons `leap_table_unavailable`, `leap_conflict` (including armed
  rejection), `leap_source_disagreement`, optional-table expiry and ordinary
  fetch failure with valid data. Metrics verify all nine bounded event labels,
  readiness, table validity and SETTLING's zero pending indicator, and exclude
  provider names and candidate hashes from labels.
- Control `LeapValid` uses the greater of wall UTC and the saved bound,
  including exact expiry and missing data; JSON preserves provider and
  candidate diagnostics. `carillonctl tracking` output assertions cover the
  new readiness, provider, dates, acquisition and rejection lines.
- Store loading rejects invalid provider/key metadata, inconsistent pending
  generations, acceptance/execution/seed checks after the UTC bound, oversize
  state and permissive file modes.
- `CheckUpdate` accepts the exact 400-day expiry boundary and refuses one
  second beyond it, rejects changed baseline dates/offsets, and rejects UTC
  before the baseline.
- Manual reload conflicts/missing files preserve active data. Real `-check`
  calls inspect existing valid and corrupt caches, refuse an undated manual
  file even with a cache, leave existing state unchanged, and create no cache
  for an empty automatic configuration.
- A real loopback UDP listener exports a running relay engine's durable
  peer-sourced cache to a second running fake-clock engine. The learner
  activates only after persistence, records the immediate relay/key, preserves
  original bytes and restart state, and keeps CLPS packets out of time-source
  measurements and reach.

The full race suite passes. The UDP integration additionally waits for the
worker's acceptance acknowledgment before shutdown and passed its targeted
race rerun.

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

## Final verification

All passed with Go 1.27.1 on darwin/arm64, with module and build caches in
the session scratch directory:

- `CGO_ENABLED=1 go test -race ./...` (the complete suite).
- `CGO_ENABLED=0 go vet ./...` and `CGO_ENABLED=0 go build ./...`.
- Pure-Go command builds for darwin/arm64, linux/amd64 and freebsd/amd64.
- `scripts/check-configs.py` against the newly built host daemon: both shipped
  configurations pass offline.
- The review's two original engine overlay probes pass with `-race`.
  The permanent proxy regression replaces the original proxy probe's possible
  external fetch with an intercepted dial and verifies the fixed destination.
- Go formatting and `git diff --check` are clean.

Native Linux/FreeBSD race/ABI checks and live GPS/PPS leap-event acceptance
remain target-host validation. Cross-builds and fake-clock simulations do not
claim that hardware evidence.
