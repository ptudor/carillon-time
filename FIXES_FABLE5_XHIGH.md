# carillon — fixes applied from REVIEW_FABLE5_XHIGH.md

Source review: `review/2026/09/REVIEW_FABLE5_XHIGH.md` (Claude Fable 5.1, xhigh,
against commit `85353ae`). Work follows the review's "Suggested fix order".

One line per finding: ID, what changed, files touched, verification result.
A finding whose fix specification is ambiguous or contradicted by the code is
logged as SKIPPED with the reason rather than guessed at.

## RF5X-001 — PPS spike gate freezes and rejects every pulse — FIXED

**Changed.** The spike gate no longer compares against a median of a window
that a rejection is unable to refresh. `PPS.spikeGate` fits a least-squares
line through the last 8 accepted offsets (x = the pulse sequence number, so a
gap is handled) and compares the new offset against that extrapolation, with
the limit `max(5·MAD(residuals), 1 µs)` widened by the phase the discipline
loop is entitled to move between the two pulses (`MaxSlewPPM·1e-6·Δseq`,
Δseq clamped to the 8-pulse depth of the reach register) — the review's fix
option (b). Two self-healing paths from item 2 were added as well: a run of
`spikeResetAfter` = 4 consecutive rejections drops and re-primes the window,
and `timeout()` re-primes it on every unreachable fetch rather than only on
the reachable→unreachable transition. The window now carries the sequence
number alongside each offset (`windowSeq`), and `PPSConfig` gained
`MaxSlewPPM` (default 500), wired from `[discipline] max_slew_ppm` in
`main.go`. `spikeFloor` was left at 1 µs and the lock criterion, emitted
`Measurement` fields, `OnPulse` delivery and the `Spikes`/`Gaps`/`Glitches`
counters are unchanged, per the "must not change" list.

**Files.** `internal/refclock/pps.go`, `internal/refclock/pps_test.go`,
`internal/refclock/pps_sim_test.go` (new), `cmd/carillon/main.go`,
`DESIGN.md` §5.2.

**Verification.** PASS. Three new tests, all of which fail against the
pre-fix gate with exactly the review's symptoms:
`TestPPSSpikeGateSurvivesLoopSlew` (pre-fix `accepted=0 rejected=300` in the
slew phase; post-fix 300 accepted in the slew phase, 100/100 in the steady
phase, reach `11111111`, 0 spikes), `TestPPSSpikeRejectedAndRecovers` (a real
10 ms spike is still rejected, does not enter the window, and the next good
pulse is accepted), `TestPPSTimeoutRePrimesWindowWhileUnreachable`. The
closed-loop `TestPPSSimClosesTheLoop` runs the real refclock against a real
`discipline.System` and `clock.Fake` from a 500 µs offset with −20 ppm drift:
pre-fix the system source stays `"ntp"`, post-fix it is `pps0` at stratum 1
refid PPS with reach `0xff` and a 0.05 µs steady-state RMS (limit 10 µs).
`go vet ./...` and `go test -race ./...` pass.

## RF5X-004 — Shutdown leaves the slew transient in the kernel — FIXED

**Changed.** `Engine.Run` now calls `restoreBaseFrequency` after `wg.Wait()`
and before the final drift write: if the loop has issued anything and the word
it last issued differs from the base estimate, the base is written back and
logged at INFO with the abandoned slew and the abandoned phase. The pending
phase is not finished and the clock is not stepped. `Loop.Applied()` and the
`System.Applied()`/`System.Pending()` accessors were added to expose what the
kernel is actually holding. Per item 2, a fatal error that came from
`SetFrequency` itself is now tagged with a sentinel (`errFrequencyRefused`) and
suppresses the restore, so the exit path cannot produce a second failure of the
same call. `STA_UNSYNC` handling on exit, the drift-file format and the
never-step-on-exit rule are untouched.

**Files.** `internal/engine/engine.go`, `internal/engine/engine_test.go`,
`internal/discipline/loop.go`, `internal/discipline/system.go`, `DESIGN.md` §12.

**Verification.** PASS. `TestEngineLeavesBaseFrequencyInKernel` asserts the
last frequency written equals `Status().Frequency` while `Pending != 0`;
without the fix it reports the review's evidence verbatim — *kernel left at
500.000000 ppm with 0.292000 s of phase still pending; want the base estimate
0.000000 ppm*. `TestEngineDoesNotRewriteFrequencyAfterTheKernelRefusedOne`
asserts the refused call is not repeated on the way out. `go vet ./...` and
`go test -race ./...` pass.

## RF5X-012 — Filter dispersion ignores unfilled stages — SKIPPED

**Reason: the fix specification conflicts with the code's actual behaviour.**

The spec says to count absent stages at `MaxDispersion` (RFC 5905 §10) while
explicitly leaving "the staleness rule" unchanged. Those two cannot both hold
here. `SourceState.apply` only refreshes `Dispersion` when the measurement is
`Valid`, and a source's measurement is `Valid` only when its filter reported
`updated` — i.e. when the new sample beat every older one on delay. So a
source's *reported* dispersion freezes at the stage count the filter had the
last time it picked a new best sample, and under the RFC rule that frozen
value is seconds, not milliseconds.

Implemented as specified and probed in the simulation:

- `TestSystemRemoveSource`: the single source's first replies happen to have
  the lowest delays, so the filter never reports another update. Its
  dispersion is pinned at the three-sample value `1.938` and its root distance
  at `1.95` — permanently above `MaxDistance` (1.5). The source is `invalid`
  for the whole 600 s run and the daemon never leaves `unsynced`. On a real
  host that is a server that never synchronises because its first reply was
  unusually fast.
- `TestSimFalseticker`: sources cross the `MaxDistance` threshold at different
  times for the same reason, so there is a window in which only one is a
  candidate. With the review's change, at t=512 that one is the falseticker
  (`bias = 3.0`): it becomes the sole survivor and system source and steps the
  clock by 3 s — `steps=2` where the test requires 0. Today the intersection
  excludes it because all three sources are candidates from their first
  sample.

Making the RFC dispersion correct would require also changing what the spec
says not to change — reporting the filter output on every poll and gating only
the loop update on `updated`, as ntpd does — which is a larger design change
than this finding authorises and overlaps RF5X-006. Left unfixed; the finding
is real but needs a fix specification that addresses the staleness rule too.

## RF5X-002 — Pre-step measurements applied after the step, causing a second step — FIXED

**Changed.** `discipline.Measurement` gained a `Generation uint64` stamp. The
engine owns an `atomic.Uint64` (shared with the sources through
`engine.Config.Generation`, created in `main.go` because the sources are built
first) that starts at 1 and is incremented in `handle()` immediately before
`ActionStep` is applied and immediately before the leap-crossing `Reset()`
sweep. Each source reads it when it *starts* a sample — NTP before T1, PPS
before the fetch, NMEA at the `$` that fixes the arrival timestamp — and
re-reads it before the sample is used; on a change the sample is dropped
without entering the filter or window, reach is still updated, and a new
`Stale` counter is incremented (`Received` is not, since the reply was not
usable). `Engine.stale()` then drops any measurement whose generation is
behind its own, closing the remaining window between a source's last look and
the engine dequeuing. `Generation == 0` means "unstamped" and is never stale,
so test doubles and the `-check`/query paths are unaffected. Stale drops are
merged into the published per-source `Info` and surface as
`carillon_source_events_total{result="stale"}` and `carillonctl sources`'
`stale` field.

Item 5 (defence in depth) is implemented as a caller veto: `Loop.Update` took
a new `mayStep` argument and reports `Deferred` when a step is withheld —
leaving the loop wholly untouched, so neither the step budget nor an update is
consumed. `System.mayStep` grants it always for the first step of a run, and
afterwards only when the system source has ≥ 2 valid measurements since the
last step (`SourceState.sinceStep`, zeroed by `invalidate()`) or ≥ 2 survivors
have each reported once. `Source.Reset()`, the step-policy semantics and the
existing `Measurement` field names are unchanged.

**Files.** `internal/discipline/measurement.go`, `loop.go`, `select.go`,
`system.go`, `sim_test.go`, `loop_test.go`; `internal/engine/engine.go`,
`engine_test.go`; `internal/source/source.go`, `ntp.go`, `source_test.go`;
`internal/refclock/pps.go`, `nmea.go`; `internal/control/protocol.go`;
`internal/monitor/metrics.go`; `cmd/carillon/main.go`; `DESIGN.md` §5.1, §6.4,
§6.6.

**Verification.** PASS. `TestEngineStepsOnceWithTwoSources` promotes the
review's scratch test and asserts `len(clk.Steps) == 1`; with both guards
removed it reports the review's evidence verbatim — *steps applied to the
clock: [2s 2s]*. `TestEngineDropsStaleMeasurement` covers the engine-side
drop and the "generation 0 is never stale" rule.
`TestGenerationBumpDiscardsReply` (package `source`) bumps the generation
inside the fake server's handler and asserts the reply is discarded:
`Received` unchanged at 0, `Stale == 1`, filter empty, measurement not valid,
reach still 1. `TestGenerationStampedOnMeasurements` checks the stamp itself.
`TestSystemSecondStepNeedsMoreThanOneSample` and
`TestSystemSecondStepWithTwoAgreeingSurvivors` cover both arms of the step
gate, including that a deferred step leaves `Updates` and `Pending` alone.
`go vet ./...` and `go test -race ./...` pass.

## RF5X-005 — A leap transition drops the server to LI=3 for three loop updates — FIXED

**Changed.** Option 1 of the fix specification. `System.Resync(now)` replaces
`InvalidateSources` in the engine's leap branch: it drops every source
estimate and clears `lastUsedAt` exactly as before, but sets a `resyncing`
flag (only when the daemon was SYNCED or HOLDOVER) that makes the first
post-reset loop update restore SYNCED directly instead of passing through
SETTLING. `InvalidateSources` is retained for any other caller. The
generation counter from RF5X-002 is bumped at the same point (item 3), so
pre-leap measurements are discarded rather than invalidated-then-applied. The
kernel `STA_INS`/`STA_DEL` sequencing, the `Source.Reset()` sweep and the
holdover timeout are untouched.

**Files.** `internal/discipline/system.go`, `internal/engine/engine.go`,
`internal/engine/engine_test.go`, `internal/discipline/sim_test.go`,
`DESIGN.md` §6.5.

**Verification.** PASS. `TestEngineLeapfileOverridesAndResetsAtTransition`
extended: the first measurement after the transition gives `StateSynced`,
`Leap == LeapNone`, `clk.Status().Synced == true`, and the `main.go` mapping
the NTP listener uses reads synchronized. `TestEngineNeverServesUnsyncedAcrossALeap`
walks four updates across the transition and asserts the listener's view is
never false. `TestSystemResyncSkipsSettling` covers the System level.

## RF5X-010 — The SETTLING counter is not reset by a second step — FIXED

**Changed.** RF5X-006 replaced the counter, so per the specification's own
instruction the "since last step" reference is now reset on *every* step:
`sinceStep`, `postStepUpdates` and `resyncing` are zeroed in the `u.Stepped`
branch before `setState`, which no longer owns the counter at all
(`setState`'s `if to != StateSynced { settled = 0 }` is gone).

**Files.** `internal/discipline/system.go`, `internal/discipline/sim_test.go`.

**Verification.** PASS. `TestSystemSecondStepResetsSettling` reproduces the
review's scratch sequence with `settle_updates = 3`: after a second step the
evidence counters read 0 and SYNCED comes only after three further post-step
samples, not two.

## RF5X-006 — SETTLING counts filter updates, so a restarted host answers LI=3 for minutes — FIXED

**Changed.** SETTLING progress is now counted in *measurements received for
the system source since the last step* (`System.sinceStep`, incremented in
`Update`), not in loop updates. `System.settleDone` declares SYNCED when (1)
at least one post-step loop update has run — so a step is never served as
synchronized, (2) `sinceStep >= SettleUpdates`, and (3) another step is no
longer on the table: `!loop.stepAllowed()` or the offset at the last update
was at or below `StepThreshold`. `SettleUpdates` moved out of `main.go` into
`[discipline] settle_updates`, default 1, validated `>= 1`, and documented in
the example config. The loop-update gate (`lastUsedAt`) that protects the PLL,
the wire behaviour while genuinely UNSYNCED and `waitsync` semantics are
unchanged.

**Files.** `internal/discipline/system.go`, `internal/config/config.go`,
`cmd/carillon/main.go`, `deploy/carillon.toml.example`, `DESIGN.md` §6.5,
`internal/discipline/sim_test.go`, `internal/engine/engine_test.go`.

**Verification.** PASS. `TestSimSettling` gained the assertion the review
asks for: the time spent in `StateSettling` after the last step must be under
two poll intervals (2 × 64 s) for the sim source with delay noise. All three
states are still seen.

## RF5X-034 — Refid HOLD advertised after a panic refusal — FIXED

**Changed.** `System` tracks `unsyncedReason`: `INIT` from construction,
`HOLD` set when the holdover timeout fires in `Tick`, and a new
`ntp.KissPANC` set when the loop refuses a panic correction. `Status()`
advertises that reason instead of deriving `HOLD` from `haveUpdate`.

**Files.** `internal/ntp/time.go`, `internal/discipline/system.go`,
`internal/discipline/sim_test.go`, `DESIGN.md` §7.4.

**Verification.** PASS. `TestSimPanicRefused` now asserts the refid is `PANC`.

## RF5X-036 — "preferred source is not usable" logged at ERROR on every start — FIXED

**Changed.** `Select` remembers per source whether it has ever been reachable
(`everReachable`) and ever been a survivor (`everSurvived`), and reports
`PreferLost` only once the prefer source has been reachable at least once and
something has been a survivor at least once. Before anything has ever been
usable there is nothing to have lost. `prefer_lost` semantics once running are
unchanged, and the engine still logs a genuine loss at ERROR.

**Files.** `internal/discipline/select.go`, `internal/discipline/sim_test.go`.

**Verification.** PASS. `TestSimPreferLost` asserts no `EventPreferLost`
before the first survivor; with the gate removed it fails with *1 prefer-lost
events before the first survivor at t=64*.
