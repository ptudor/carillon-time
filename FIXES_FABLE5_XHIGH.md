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
