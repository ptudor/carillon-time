# carillon — fixes applied from REVIEW_ASTRA6_XHIGH.md

Source review: `review/2026/09/REVIEW_ASTRA6_XHIGH.md` (Astra6, xhigh, against
commit `8060697`). Work follows the review's "Suggested fix order and
dependencies" table, wave by wave.

One entry per finding: ID, what changed, files touched, verification result.
A finding whose fix specification is ambiguous or contradicted by the code is
logged as SKIPPED with the reason rather than guessed at.

Verification runs on this darwin/arm64 development host with Go 1.27.0 and
`CGO_ENABLED=1 go test -race`. Anything needing a Linux or FreeBSD kernel, a
real serial device, a live PPS edge, or a privileged clock is cross-compiled
and type-checked here and marked **deferred to the target hosts** — per
CLAUDE.md hard rule 1, Claude never runs those.

## Outcome

The table records the original review pass. **2026-09-08 follow-up:** M5
implements the maintainer's selected leap policy and closes RA6X-023. Three
items remain open: RA6X-021, RA6X-040 and RA6X-057. See
[M5 implementation verification](M5_IMPLEMENTATION.md).

| | Count |
|---|---:|
| Fixed | 55 |
| Skipped | 4 |
| **Total** | **59** |

The four skipped findings are RA6X-021, RA6X-023, RA6X-040 and RA6X-057. Each
is one the review itself marks **Needs investigation**, and in each case the
fix specification opens with evidence this session cannot gather (receiver
captures, live peer captures) or a policy decision that changes documented
behaviour and is the maintainer's to make. Each entry below states what is
needed to close it and what the present behaviour is. The other two
investigation findings, RA6X-047 and RA6X-058, were implemented; their entries
explain why those decisions were one-sided in a way the four are not.

## Final verification

Run from the repository root after the last change:

| Check | Result |
|---|---|
| `CGO_ENABLED=1 go test -race -count=2 ./...` | pass, all nineteen packages |
| `make vet`, `make test`, `make dist` | pass; all four cross-builds report `CGO_ENABLED=0` |
| `review/2026/09/takeover-repro/reproduce.py baseline` | **pass in full** — twelve delayed-feedback cases and the drift-persistence case, all of which failed on the reviewed baseline |
| `verification_fable5_xhigh/run_probes.py` | 8 of 9 executable probes pass; see the caveats below |
| `verification_fable5_xhigh/probe_signals.py` | pass |
| `go list -test -tags abicheck` for six (GOOS, GOARCH, package) pairs | loads cleanly; was rejected on the baseline |
| `go test -c` for `internal/serial` on linux and freebsd, amd64 and arm64 | builds |

**Caveats on the Fable5 probe set**, which is a fixed set of text fixtures
from the previous review and not part of the build:

- `TestVerification036ReportsNeverReachablePreferAfterFallback` fails, and
  correctly. It builds a `SourceState` with `Updated: 100` and then runs
  selection at `now = 1000` to represent "900 s of fallback service" — but a
  source that has said nothing for 900 s at poll 6 is fourteen missed polls
  stale, which RA6X-003's freshness rule now makes ineligible. Real fallback
  service means the source keeps reporting, which is what
  `TestAstra6ReportsNeverReachablePreferAfterFallback` does; the property the
  fixture is about is covered there and in `TestAstra6PreferLostPhases`.
- The `internal/refclock` and `internal/source` fixtures no longer compile:
  they call `n.consume` and assign `n.lookup` with the signatures those had
  before RA6X-006 added the clock epoch to the NMEA framing path and RA6X-037
  made DNS return every answer. Signature drift in an out-of-tree fixture, not
  a regression.

---

## Wave 0 — enable native verification alongside the fixes

## RA6X-054 — ABI verification tests cannot be built because they import C in test files — FIXED

**Changed.** Go rejects `import "C"` inside a `_test.go` file, so the three
hand-written-ABI comparisons never compiled, let alone ran. The C side moved
into ordinary (non-test) files that are still excluded from every production
build by their tags, and each test file is now plain Go that iterates over the
facts the C file collected:

- `internal/pps/abi_linux_cgo.go` builds `[]cLayout` from `<linux/pps.h>` —
  sizes and offsets for `pps_ktime`, `pps_kinfo`, `pps_fdata`, `pps_kparams`,
  the hand-declared `PPS_API_VERS_1`/`PPS_CAPTURE*`/`PPS_TSFMT_TSPEC`/
  `PPS_TIME_INVALID` constants, and the four `PPS_*` ioctl numbers carillon
  takes from `x/sys` (a mismatch there is just as fatal as one in our own
  constants, so they are compared too). `internal/pps/abi_linux_test.go`
  compares them.
- `internal/pps/abi_freebsd_cgo.go` does the same against
  `<sys/timepps.h>` for `pps_info_t`, `pps_params_t`, `struct pps_fetch_args`
  and the five hand-declared `PPS_IOC_*` numbers, with
  `internal/pps/abi_freebsd_test.go` comparing.
- `internal/clock/abi_freebsd_cgo.go` collects the `struct timex` size, all
  seventeen field offsets, and the fifteen `MOD_*`/`STA_*` constants from
  `<sys/timex.h>`; `internal/clock/abi_freebsd_test.go` compares.

The tag is now `abicheck`, deliberately **not** `hwtest`: reading header
offsets opens no device and touches no clock, so this safe check no longer
requires enabling the tests that do mutate hardware. `make abicheck` runs it.
Coverage is slightly wider than the deleted files (they omitted
`pps_kinfo.clear_sequence`, `pps_kparams.mode`, `pps_fetch_args.tsformat` and
every non-ioctl constant). Production and cross builds stay `CGO_ENABLED=0`.

**Files.** `internal/pps/abi_linux_cgo.go` (new),
`internal/pps/abi_linux_test.go` (new), `internal/pps/abi_freebsd_cgo.go`
(new), `internal/pps/abi_freebsd_test.go` (new),
`internal/clock/abi_freebsd_cgo.go` (new),
`internal/clock/abi_freebsd_test.go` (new),
`internal/pps/pps_linux_cgo_test.go` (deleted),
`internal/pps/pps_freebsd_cgo_test.go` (deleted),
`internal/clock/timex_freebsd_cgo_test.go` (deleted), `Makefile`.

**Verification.** The review's reproduction was `go list -test` with cgo
failing on `use of cgo in test <file> not supported`. All twelve
(GOOS, GOARCH, package) combinations the review names now load cleanly under
both `-tags hwtest` and `-tags abicheck`:

```
GOOS={linux,freebsd} GOARCH={amd64,arm64} CGO_ENABLED=1 \
  go list -test -tags abicheck ./internal/pps ./internal/clock   → OK (was: rejected)
```

`CGO_ENABLED=1 go test -race ./...` passes natively; all four production
cross-builds still report `CGO_ENABLED=0` under `go version -m`. **Deferred to
the target hosts:** actually compiling the C against real `<linux/pps.h>` and
`<sys/timepps.h>`/`<sys/timex.h>` and comparing the layouts needs native
Linux and FreeBSD — `make abicheck` there.

## RA6X-055 — The documented race-test Make target fails on deployment platforms — FIXED

**Changed.** The Makefile's global `export CGO_ENABLED = 0` reached the `test`
recipe, whose `-race` needs the cgo-backed race runtime; on Linux and FreeBSD
`make test` therefore died with `go: -race requires cgo` before running a
single test. The global export is gone, replaced by two explicit variables:
`BUILD_ENV := CGO_ENABLED=0` applied to `build`, `vet` and the `cross`
template, and `TEST_ENV := CGO_ENABLED=1` applied to `test`. `-race` was not
dropped. The new `abicheck` target (RA6X-054) sets `CGO_ENABLED=1` itself.
Comments now state that native race testing needs a C compiler and that
cross-compiled race testing is unsupported — run `make test` on each host.

**Files.** `Makefile`.

**Verification.** Baseline reproduced: `GOOS=linux GOARCH=amd64
CGO_ENABLED=0 go test -race ./internal/discipline` → `go: -race requires cgo`.
With cgo enabled that error is gone and the build proceeds to the C toolchain
(which then fails only for want of a *cross* C compiler on this Mac, exactly
as the fix documents). `make test GO=/opt/local/bin/go` passes natively; `make
dist` produces all four binaries and `go version -m` reports `CGO_ENABLED=0`
for each. **Deferred to the target hosts:** `make test` on Linux and FreeBSD
with their native compilers.

## RA6X-056 — The serial EOF regression asserts the wrong kernel event path — FIXED

**Changed.** Closing a pipe's write end makes `poll(2)` report `POLLHUP`,
which `ReadTimeout` correctly classifies as a device error *before* it calls
`read(2)` — so the old test's `errors.Is(err, io.EOF)` assertion failed on
Linux and FreeBSD while never reaching the zero-length-read branch it was
written to protect. Production classification is unchanged. Instead, `Port`
gained a two-function syscall seam (`poll`, `read`), nil in production where
the real `unix.Poll`/`unix.Read` are used and set only by tests; a `Port` is
owned by one refclock goroutine, so no locking is involved. Four tests now
cover the path:

- `TestReadTimeoutClosedPipeIsNotATimeout` — the real closed pipe, asserting a
  prompt (< 1 s against a 2 s timeout) non-timeout disconnect rather than a
  specific error identity.
- `TestReadTimeoutReportsEOF` — the seam produces the pair no pipe can, `n=0,
  err=nil` after a readable poll, and requires it to wrap `io.EOF`.
- `TestReadTimeoutHangupIsNotEOF` — pins the production HUP contract: `read(2)`
  is never called, and the error is neither a timeout nor an `io.EOF`.
- `TestReadTimeoutStillTimesOut` — the ordinary no-data path is still
  `ErrTimeout` and does not call `read(2)`.

**Files.** `internal/serial/serial.go`, `internal/serial/read_unix.go`,
`internal/serial/read_unix_test.go`.

**Verification.** The package is `//go:build linux || freebsd`, so it cannot
execute on this host. `go vet` passes for both GOOSes and the test binary
compiles for linux/amd64, linux/arm64, freebsd/amd64 and freebsd/arm64.
`CGO_ENABLED=1 go test -race ./...` still passes natively. **Deferred to the
target hosts:** running the four tests on Linux and FreeBSD.

---

## Wave 1 — guard inputs and the clock actuator

## RA6X-014 — NaN in the drift file reaches the clock-control state — FIXED

**Changed.** Two layers, as the fix specification asks.

*Input:* `readDrift` now rejects any non-finite value before the range check.
`strconv.ParseFloat` accepts `NaN`, `nan`, `Inf`, `+Inf`, `-Inf` and
`Infinity`, and the existing `v > 500 || v < -500` test is false for every one
of them, so the value was marked a known frequency, initialized the loop, and
survived `clampFreq` (`math.Min`/`math.Max` propagate NaN). A rejected file now
takes the same path any other unusable drift file takes — the `default:` arm of
`initialFrequency`, which logs `ignoring drift file` and falls back to the
kernel's value. It is not silently treated as a measured zero.

*Actuator:* a new `clock.CheckFrequency` rejects a non-finite ppm value, and
`SetFrequency` calls it in all three implementations — `sysclock_linux.go`,
`sysclock_freebsd.go` and `clock.Fake` — before any conversion to a kernel
word. The engine also checks before issuing `ActionSetFrequency`. `Fake` was
included deliberately so a test that lets an invalid word through fails at the
actuator rather than recording it. Valid ±500 ppm values, the scalar file
format and the kernel-frequency fallback are unchanged.

**Files.** `internal/engine/engine.go`, `internal/clock/sysclock_common.go`,
`internal/clock/sysclock_linux.go`, `internal/clock/sysclock_freebsd.go`,
`internal/clock/fake.go`, `internal/engine/astra6_review_test.go` (new).

**Verification.** The named probe `TestAstra6DriftRejectsNaN` passes.
`TestAstra6DriftFileContents` tables all ten non-finite spellings strconv
accepts, out-of-range values, whitespace, garbage, and the ±500 ppm
boundaries; `TestAstra6DriftRoundTrip` proves the stricter parser did not
narrow what `writeDrift` produces; `TestAstra6NonFiniteNeverReachesTheActuator`
asserts the fake actuator records nothing for NaN or either infinity. One
expectation from the review's list was corrected rather than enforced:
`0x1p3` is Go hexadecimal-float syntax for a finite, in-range 8 ppm, and there
is no reason to refuse it. `CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-035 — Positive infinity and unrepresentable durations pass configuration validation — FIXED

**Changed.** The one-sided comparisons (`!(v > 0)`, `!(v >= 1)`) reject NaN but
accept `+Inf`. Two helpers replace them in `internal/config`: `seconds(v)`,
which requires a finite value that converts to a strictly positive
`time.Duration` without overflowing (via `clock.Seconds`, so the bound is the
same one the actuator uses), and `inRange(v, lo, hi)`, which is NaN-safe and
two-sided. Applied to:

| Setting | Rule |
|---|---|
| `daemon.drift_stable_seconds` | `seconds` |
| `daemon.drift_stable_spread_ppm` | `(0, 1000]` — the width of the whole ±500 ppm range; at or above it the spread can never be exceeded, silently disabling the drift gate |
| `discipline.holdover_max` | `seconds` |
| `step.threshold` | `seconds` |
| `step.panic` | `seconds`, then still `> threshold` |
| `serve.rate_limit_pps` | `(0, 1e9]` |
| `serve.rate_burst` | `[1, 1e9]` |

`discipline.max_slew_ppm` already used a two-sided `(0, 500]` test and needed
nothing. A value that rounds to zero nanoseconds is refused rather than
silently becoming a default after conversion.

Three more boundaries the finding names: `server.NewHandler` is a public
constructor that cannot assume `config.Validate` ran, so it applies the same
`(0, 1e9]` / `[1, 1e9]` rules itself; `carillonctl waitsync` parsing moved into
`parseWaitTimeout`, which rejects NaN, both infinities, negatives, values past
the representable limit, and sub-nanosecond values that would round to zero and
silently mean "for ever" — `0` keeps its documented "wait for ever" meaning.
Because a control request is JSON arriving on a socket and need not come from
`carillonctl`, `control.waitDuration` now converts the request timeout with
saturation on both the server and the client side, so an out-of-range value
cannot wrap into a deadline in the past. That last one is outside the
finding's listed locations but is the same defect on the same code path,
reached by the same CLI argument.

**Files.** `internal/config/config.go`, `internal/server/responder.go`,
`internal/server/ratelimit.go`, `internal/control/server.go`,
`internal/control/client.go`, `cmd/carillonctl/main.go`,
`internal/config/astra6_review_test.go` (new),
`internal/server/astra6_review_test.go` (new),
`internal/control/astra6_review_test.go` (new),
`cmd/carillonctl/astra6_review_test.go` (new).

**Verification.** The named probe `TestAstra6ConfigurationRejectsInfinity`
passes for all six settings. `TestAstra6NumericSettingBounds` tables NaN, ±Inf,
zero, negatives, the overflowing and just-overflowing values, sub-nanosecond
values, and the ordinary boundaries across every affected field;
`TestAstra6PanicStaysAboveThreshold` keeps the ordering rule;
`TestAstra6HandlerRejectsUnboundedLimits`,
`TestAstra6WaitSyncTimeoutValidation` and `TestAstra6WaitDurationIsBounded`
cover the constructor, CLI and protocol boundaries. `TestAstra6ShippedExamplesStillParse`
parses `deploy/carillon.toml.example` to prove nothing shipped is rejected.
Invalid configuration still fails in `Validate`, before any socket, device or
clock is touched. `CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-041 — Clock actions are converted to durations without range checks — FIXED

**Changed.** `time.Duration(v * float64(time.Second))` wraps in silence, so
`+1e20 s` was recorded as a step of about +292 years. Three new helpers in
`internal/clock` make the boundary explicit:

- `clock.Seconds(v)` converts a *phase correction* and returns
  `ErrNotRepresentable` for a non-finite value or one beyond ±9.223372036e9 s
  (the strict side of the `time.Duration` limit, allowing for float64 rounding
  near the edge).
- `clock.BoundSeconds(v, limit)` converts an *error bound*, saturating instead
  of failing: an uncertainty too large to express is honestly the largest bound
  the kernel accepts, and is no reason to stop disciplining the clock. NaN maps
  to the limit, negatives to zero.
- `clock.CheckFrequency(ppm)` as described under RA6X-014.

`Engine.handle` converts a step through `clock.Seconds` **before** bumping the
generation, so a refused step does not invalidate every sample in flight for a
discontinuity that never happened. `syncKernel`'s `maxerror`/`esterror`
conversions — the same unchecked pattern the finding names — go through
`BoundSeconds` with a named `maxKernelError` constant replacing the three
repeated `16 * time.Second` literals.

Upstream of the actuator: `NMEA.acceptLine` detects `time.Time.Sub` saturating
(it returns `math.MaxInt64`/`MinInt64` rather than reporting overflow) and
rejects the sentence with a stated reason instead of turning it into a ±292-year
offset. And every refclock calibration offset — `offset`, `pps_offset`,
`nmea_offset` — must now be finite and within ±1 s: these compensate cable,
driver and sentence-lag delays, all far below a second, and a PPS offset of a
second or more makes "which second the edge starts" ambiguous, which is the one
question a PPS refclock cannot answer for itself. Legitimate large startup
corrections (an RTC at the epoch, about 1.77e9 s) remain representable and
accepted; sign, nanosecond units, the step policy and the backend APIs are
unchanged, and nothing is clamped into a centuries-long step.

**Files.** `internal/clock/sysclock_common.go`, `internal/engine/engine.go`,
`internal/refclock/nmea.go`, `internal/config/config.go`,
`internal/engine/astra6_bounds_review_test.go` (new),
`internal/refclock/astra6_review_test.go` (new),
`internal/config/astra6_review_test.go`.

**Verification.** The named probe
`TestAstra6StepRejectsUnrepresentableDuration` passes.
`TestAstra6StepConversionBounds` covers both signs, NaN, both infinities, the
representable limit and the floats either side of it, zero, an RTC-at-1970
bootstrap in both directions and ordinary corrections, asserting the fake
actuator is not called at all on refusal and applies exactly the intended
duration otherwise. `TestAstra6RefusedStepDoesNotBumpGeneration`,
`TestAstra6NonFiniteUncertaintyStaysRepresentable`,
`TestAstra6NMEAFarFutureSentenceIsRejected` (a checksum-valid ZDA for the year
9999), `TestAstra6NMEAEpochBootstrapIsUsable`,
`TestAstra6RefclockOffsetsMustBePlausible` and
`TestAstra6GPSOffsetsMustBePlausible` cover the rest.
`CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-011 — Panic-at-startup bypasses the never-step setting — FIXED

**Changed.** The panic branch in `Loop.Update` called `step` directly, ahead of
the ordinary `stepAllowed()` budget check, so `panic_at_startup = true` with
`limit = 0` moved the clock by thousands of seconds. The startup exception now
also requires `stepAllowed()`; when stepping is forbidden the correction is
refused (`PanicRefused`, no actions) rather than silently becoming an enormous
slew. Precedence is documented in the code and in `DESIGN.md` §6.6 under a new
heading — *`limit` outranks `panic_at_startup`*: the panic setting widens
*which offsets* may be corrected on the first update, `limit` decides *whether
a step may happen at all* — and in `deploy/carillon.toml.example`. The one-time
large startup correction still works for any configuration that permits
stepping, and backward steps remain as allowed as forward ones.

**Files.** `internal/discipline/loop.go`, `DESIGN.md`,
`deploy/carillon.toml.example`,
`internal/discipline/astra6_review_test.go` (new).

**Verification.** The named probe `TestAstra6NeverStepIncludesPanicStartup`
passes, and additionally asserts the refusal is reported and produces no
actions. `TestAstra6StepPolicyCrossProduct` runs the full cross product the
review asked for — limit ∈ {0, 3, -1} × `panic_at_startup` ∈ {false, true} ×
{first update, later update} × ten offsets straddling both thresholds in both
signs (180 subtests) — checking that `Stepped` and `ActionStep` always agree
and that the stepped value is the offset. `CGO_ENABLED=1 go test -race ./...`
passes.

## RA6X-018 — Canceling an NMEA reconnect panics the daemon — FIXED

**Changed.** `NMEA.reopen` returned `nil` on cancellation after clearing the
reader, and `Run` reads `nil` as "reopened successfully", loops, and
dereferences the nil reader — a panic in a source goroutine that takes the
whole daemon down without restoring the base frequency or flushing statistics.
`reopen` now returns `ctx.Err()` in that case, so it returns nil *only* when
`n.reader` holds an open device, and its doc comment states that contract.
`Run` treats a reopen failure with a cancelled context as a clean stop
(`return nil`, preserving cancellation as a normal source stop) and any other
failure as an error. `Run` additionally checks cancellation at the top of the
loop and refuses to proceed with a nil reader, so no future path can
reintroduce the dereference. Exponential backoff and exactly-once cleanup are
untouched. `PPS.reopen` has the same shape but is already safe through an
explicit post-reopen check; it is left for RA6X-004, which rewrites that path.

**Files.** `internal/refclock/nmea.go`,
`internal/refclock/astra6_review_test.go`.

**Verification.** The named probe `TestAstra6NMEACancelDuringReconnect` passes
with no panic. `TestAstra6NMEACancellationPoints` adds the four remaining
positions from the review's list — cancel before `Run`, during `ReadTimeout`,
during a backoff, and immediately after a successful reopen — plus the
no-opener case, each bounded by a ten-second watchdog that fails the test if
`Run` does not return. `CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-016 — Shutdown before the first tick leaves the wrong base frequency applied — FIXED

**Changed.** `restoreBaseFrequency` consulted `Loop.Applied()`, which only
`Tick` ever sets, while the startup write in `Run` goes straight to the clock.
Several measurements could therefore move the base before the first tick, and a
shutdown there returned early, leaving the kernel at the startup value while
status reported the final base. The engine now tracks the actuator itself: a
new `Engine.setFrequency` wraps every frequency write in the daemon — the
startup write, `ActionSetFrequency`, and the restore itself — and records the
value only when the actuator accepted it. `restoreBaseFrequency` uses that,
which is strictly more authoritative than the loop's record of what it
*issued*. The review's option of seeding the loop's applied state was the other
choice offered; engine-side tracking was taken because it also covers a
frequency write the kernel refused. Suppression of the redundant retry after
`errFrequencyRefused`, the no-step-on-exit rule and the drift gate are
unchanged.

**Files.** `internal/engine/engine.go`,
`internal/engine/astra6_bounds_review_test.go`.

**Verification.** `TestAstra6RestoresBaseBeforeFirstTick` reproduces the
scenario with the ticker set to an hour so every base change lands before the
first `Tick`, and asserts the last actuator write equals the reported final
base. `TestAstra6RestoreEdgeCases` covers zero updates (exactly one frequency
write, the startup value) and an initial write failure (`SetFrequency` called
once, no retry on the way out). `CGO_ENABLED=1 go test -race -count=2 ./...`
passes.

---

## Wave 2 — stop destructive filesystem mistakes

## RA6X-015 — Startup temporary cleanup can delete the configured drift file — FIXED

**Changed.** The sweep globbed `.drift-*` beside the drift file, `os.Stat`ed
each match and removed anything older than a minute. The drift filename is
operator-configurable, so a host calibrated into `.drift-calibrated` had its
calibration deleted at every start, and two instances sharing a state
directory could delete each other's temporaries.

`writeDrift` now creates its temporary from a destination-specific pattern —
`driftTempPattern(path)` = `.<basename>-tmp-*` — so an instance's temporaries
are namespaced to the file it writes, and no destination can match its own
sweep glob (`.<base>-tmp-<digits>` is always longer than `<base>`).
`sweepDriftTemps` requires a candidate to clear four tests before removal:

1. the name is one `os.CreateTemp` generates from *this* destination's
   pattern — the exact prefix plus decimal digits and nothing else;
2. `os.Lstat` reports a regular file, so a symlink is never followed and a
   directory is never touched;
3. it is not the configured destination, by `os.SameFile` (device and inode),
   even if the name somehow matched;
4. it is older than a minute, so a concurrent write is left alone.

Temporaries in the pre-existing `.drift-<digits>` shape are still swept, but
only when the destination's basename is exactly `drift`, which is the only
case in which they provably belonged to it — otherwise already-deployed hosts
would accumulate the clutter the sweep exists to remove. Atomic
write/fsync/rename, valid configured filenames and the last known-good
contents are unchanged.

**Files.** `internal/engine/engine.go`,
`internal/engine/astra6_review_test.go`.

**Verification.** The named probe `TestAstra6SweepPreservesConfiguredDrift`
passes — `.drift-calibrated`, aged an hour, survives `New` with its contents
intact. `TestAstra6SweepScope` plants the whole list from the review: matching
and non-matching names, a plausible operator filename, another instance's
temporary, a suffixed name, a non-hidden name, an unrelated dotfile, a
directory named like a temporary, a symlink named like a temporary (with its
target), a fresh temporary, and the destination itself — only the two genuinely
abandoned temporaries disappear. `TestAstra6SweepIsInstanceScoped` and
`TestAstra6WriteDriftUsesItsOwnNamespace` cover the namespacing, and the
existing `TestEngineSweepsStaleDriftTemporaries` still passes.
`CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-032 — Control socket startup can delete ordinary files or unlink a live daemon — FIXED

**Changed.** `Listen` `os.Stat`ed the path without checking its type and
removed anything that failed to accept a connection within 500 ms. A mistyped
control path therefore destroyed a regular file, and a permission error,
exhausted backlog or slow probe unlinked a *live* socket — after which a second
listener could bind the same name and the host had two clock owners.

Ownership of the path is now taken with an exclusive advisory `flock(2)` on a
sibling lock file (`<control>.lock`), held for the daemon's whole run; that
lock, not the presence of the socket inode, is the single-instance check. A
second start reports `ErrInUse` and exits. Only while holding the lock does
`clearAbandonedSocket` consider removing anything, and it removes only a path
that is a socket by `os.Lstat` (so a symlink is refused rather than followed)
*and* whose probe failed in a way that proves nothing is listening.
`socketAbandoned` accepts exactly `ECONNREFUSED` and "it vanished between the
lstat and the dial"; a timeout, `EACCES`, `EPERM` or a reset all fail startup
with the path untouched, because each of those also happens to live sockets.
Non-socket paths fail with a message naming what is actually there. `Server`
gained a `Close` that releases the socket and the lock, `Serve` releases the
lock on both of its exits, and `main` defers `ctl.Close()` so the lock is
dropped on paths that never reach `Serve`. Mode `0660`, the socket path,
`ErrInUse` discoverability and recovery from a genuinely abandoned socket are
preserved; the behaviour is documented in `DESIGN.md` §10.2.

The existing `TestListenRemovesStaleSocket` planted a *regular file*, encoding
the very assumption the finding is about; it now creates a real unix socket and
closes the listener with `SetUnlinkOnClose(false)`, which is exactly what a
SIGKILLed daemon leaves behind.

**Files.** `internal/control/server.go`, `cmd/carillon/main.go`, `DESIGN.md`,
`internal/control/control_test.go`,
`internal/control/astra6_review_test.go`.

**Verification.** The named probe `TestAstra6ListenPreservesRegularFile`
passes: `Listen` fails and the file keeps its contents.
`TestAstra6ListenRefusesNonSockets` covers a directory, a symlink to a live
socket (neither link nor target touched) and a fifo;
`TestAstra6ListenRefusesLiveSocket` confirms a busy listener is reported as in
use and keeps answering afterwards; `TestAstra6ConcurrentStartersElectOneOwner`
runs eight simultaneous `Listen` calls and requires exactly one winner with the
rest returning `ErrInUse`; `TestAstra6ListenRecoversAbandonedSocket` keeps the
recovery path working, mode `0660` included; and
`TestAstra6SocketAbandonedClassification` pins which dial failures may be read
as "no listener". `CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-046 — Statistics retention trusts an unsynchronized wall clock — FIXED

**Changed.** Opening any new dated file called `prune(at)` with that row's own
wall date, so a future RTC at boot deleted every retained day on the first
minute tick or the shutdown server row — and a misdated PPS row could do the
same independently.

The recorder now separates the label from the horizon. Rows are still written
under the date of the observation they describe, unsynchronized observations
included; but destructive retention runs only from `Recorder.horizon`, the
latest wall time seen in a snapshot the discipline reported as `StateSynced`.
`StateSynced` is reached only after settling, which excludes both boot and the
interval around a large startup step, and the horizon never moves backwards, so
an excursion cannot pull retention back either. Until the clock has been
synchronized once, `pruneTrusted` removes nothing at all. Because the horizon
can lag the current day, retention keeps at least `keep_days` and sometimes a
little more — the safe direction. TSV paths and columns, `keep_days = 0`, the
UTC-day semantics and the recording of unsynchronized observations are
unchanged; `DESIGN.md` §11 documents the horizon.

**Files.** `internal/stats/writer.go`, `DESIGN.md`,
`internal/stats/astra6_review_test.go` (new).

**Verification.** The named probe `TestAstra6UntrustedWallTimeDoesNotPrune`
passes: a 2026 file survives a recorder whose `Now` reads 2099 with
`keep_days = 7`. `TestAstra6RetentionHorizon` covers the rest of the review's
list — a future RTC and a past RTC before synchronization (nothing pruned, rows
still written under their own dates), a corrected RTC (pruning resumes and
removes only genuinely expired days), a backward excursion (horizon does not
retreat), a misdated PPS row (recorded, not destructive) and `keep_days = 0`.
The existing `TestRecorderPrunesExpiredDays` and
`TestRecorderKeepsEverythingByDefault` still pass.
`CGO_ENABLED=1 go test -race ./...` passes and `make dist` still builds.

---

## Wave 3 — event time, epochs, and source invalidation

## RA6X-059 — Queued measurement timestamps rewind the engine's processing time — FIXED

**Changed.** The engine used the producer's enqueue-time `Measurement.Now` as
"now" for selection, loop processing and publication, so a buffered or
cross-source measurement arriving after a newer tick moved global time
backwards. A new `Engine.processing(now)` establishes a **nondecreasing
processing clock**: `Run` reads the monotonic clock at consumption and stamps
`m.Now` with it, and `handle`, `publish`, `sourceStopped` and the drift write
all pass through the same function, so a direct caller gets the same guarantee.
The observation's own timestamp (`Measurement.At`) is untouched, so its age
stays honest rather than being concealed. Public time units and the status
schema are unchanged; `DESIGN.md` §5.1 documents the two timestamps.

**Files.** `internal/engine/engine.go`, `DESIGN.md`,
`internal/engine/astra6_bounds_review_test.go`.

**Verification.** The named probe
`TestAstra6QueuedMeasurementDoesNotRewindEngineTime` passes — publishing at
monotonic 100 and then processing a queued `Now=1` observation no longer drops
uptime from 1m40s to 1s. `TestAstra6ProcessingClockIsMonotonic` tables an
out-of-order sequence, and `TestAstra6QueuedMeasurementKeepsObservationTime`
drives the real `Run` loop. `CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-006 — Generation stamping does not bracket clock-step execution — FIXED

**Changed.** The engine incremented the epoch before the syscall and left it
at the new value while the clock was still moving, so a source that started a
sample in that window labelled a pre-step observation with the post-step epoch
— defeating the stamp entirely. The counter now carries two states, documented
in `internal/source` and `DESIGN.md` §5.1: **even values are settled epochs**
(the first is `source.FirstEpoch` = 2) and **an odd value means a discontinuity
is executing**. `Engine.beginEpochChange` increments before the operation and
returns the completion that increments after; it runs even when the operation
fails, so a refused step cannot leave acquisition permanently in progress. The
leap-boundary reset is bracketed the same way.

Both ends now check the state, not just the value:

- `Engine.stale` admits a measurement only when its epoch is *exactly* the
  current settled epoch, instead of the old `m.Generation >= current`, which
  also accepted a value ahead of the engine's own counter.
- The NTP source refuses a sample whose epoch is unchanged but odd — T1 and T4
  straddle a discontinuity even though the counter did not move — and so does
  the PPS fetch path.
- NMEA captures the epoch **around the clock read that fixes the arrival
  time**, and rechecks it immediately after, rather than at the later `$` byte;
  a chunk whose arrival timestamp spans a discontinuity is discarded with its
  partial sentence and its window.

`Measurement.Generation = 0` still means "unstamped" and is never stale.

**Files.** `internal/source/source.go`, `internal/source/ntp.go`,
`internal/engine/engine.go`, `internal/refclock/pps.go`,
`internal/refclock/nmea.go`, `DESIGN.md`, `internal/engine/engine_test.go`,
`internal/source/source_test.go`, `internal/refclock/nmea_test.go`,
`internal/engine/astra6_epoch_review_test.go` (new).

**Verification.** `TestAstra6GenerationCoversTheStep` captures the epoch from
*inside* `Step` and requires it to be unsettled there, requires the epoch
afterwards to be a later settled one, and requires a sample stamped with the
in-progress epoch to be refused. `TestAstra6StepFailureCompletesTheEpoch`
covers the failure path. Both were confirmed to **fail** against a restored
pre-fix single-increment scheme before being accepted. Two existing tests were
updated to the new protocol — the epoch now advances by two per step and
starts at 2 — with the loose `>=` comparison replaced by an exact-epoch check
and a new assertion that a measurement stamped *ahead* of the engine is
dropped. `CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-007 — Leap transitions are detected after queued corrections execute — FIXED

**Changed.** `Run` evaluated `sys.Update(m)` before `handle` looked at the leap
boundary, and `handle` applied the resulting actions before that check, so a
queued observation spanning the transition could step the clock a whole second
before the sources were reset — and resetting afterwards cannot undo an
actuator call. The boundary check moved into `Engine.crossLeap`, which `Run`
calls **before** `stale` and **before** `sys.Update` on the measurement arm and
before `sys.Tick` on the ticker arm. `handle` calls it too, so a direct caller
is covered; it is idempotent because a boundary is crossed once. Boundary-
spanning evidence is discarded through the RA6X-006 epoch: the reset bumps the
epoch, so the queued pre-boundary measurement is stale by the time it is
examined. The post-leap `Resync` behaviour — which keeps a previously
synchronized host serving instead of dropping to SETTLING — is unchanged, and
no extra step is invented for a leap the kernel already performed.

**Files.** `internal/engine/engine.go`, `DESIGN.md`,
`internal/engine/astra6_epoch_review_test.go`.

**Verification.** `TestAstra6LeapCheckedBeforeQueuedMeasurement` drives the
real `Run` loop for both leap directions: a measurement stamped in the
pre-boundary epoch is delivered after the kernel has applied the leap, and
must be counted as a stale drop with no step and no frequency change. It was
confirmed to **fail** against a restored pre-fix ordering (0 drops instead of
1). `TestAstra6LeapResetBracketsItsEpoch`,
`TestAstra6LeapOnTheSameIterationAsATick` and
`TestAstra6RunProcessesLeapBeforeMeasurements` cover the once-only reset, the
ticker arm and a full run. `CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-017 — Source-stop events can be overtaken by queued measurements and swallow fatal errors — FIXED

**Changed.** Four things, all on the engine goroutine:

1. **Queued measurements are dropped when a source's run stops.** `Run` has
   already returned when `sourceStopped` executes and nothing more can arrive
   until the restart, so every measurement of that source's still in the queue
   belongs to the finished run. `dropQueued` removes exactly those, preserving
   the order of the others and counting them as stale drops.
2. **The estimate is revoked, not merely marked unreachable.** The synthesized
   stop measurement now carries `Invalidate: true`, so a source whose goroutine
   has exited cannot stay selected on the strength of the last thing it said.
3. **Health rests on fresh evidence.** `sourceRestarting` records the intent
   but no longer clears `sourceErrors`; `noteSourceAlive`, called when the
   restarted run actually delivers a measurement, does.
4. **Fatal results propagate.** `e.reqs` became `chan func() error`, `Run`
   ends on a non-nil result, and `sourceStopped` returns `handle`'s error
   instead of logging and discarding it — so an actuator failure raised by a
   fallback selection on the source-stop path is as fatal as it is anywhere
   else.

Configured source names, cumulative telemetry, retry backoff and retention of
stopped sources in status are unchanged.

**Files.** `internal/engine/engine.go`, `DESIGN.md`,
`internal/engine/astra6_epoch_review_test.go`.

**Verification.** `TestAstra6QueuedMeasurementCannotReviveStoppedSource`
(deterministic, per the review's note that this is a logical rather than a
memory race), `TestAstra6DropQueuedPreservesOtherSources` (interleaved queue,
order and drop count), `TestAstra6RestartKeepsHealthUntilFreshEvidence`,
`TestAstra6SourceStopPropagatesFatalErrors` and
`TestAstra6SourceStopEndsRunOnActuatorFailure` (the whole lifecycle: `Run`
must return non-nil). `CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-004 — Device reconnect loops retain reachable, selectable estimates — FIXED

**Changed.** A hard device error entered the private reconnect loop after
updating only `LastError`, so the engine heard nothing while the device was
gone: a disconnected PPS could stay system source, and a pre-outage NMEA
estimate could keep numbering a live pulse, for as long as the opener kept
failing. Both refclocks gained a `deviceLost()` that runs **before** `reopen`:
reach goes to zero, the estimate is invalidated (see RA6X-005), and the
temporal state is cleared — PPS drops its window, pending misses and
`havePrevious`; NMEA drops its window, framing, slot and ZDA-preference state
— so the replacement device has to prime and qualify from scratch. The loss is
then emitted as a measurement before the reconnect loop is entered.
`LastError` is left as the caller set it and each failed retry refreshes it,
so health keeps explaining the outage. Source identity, cumulative counters,
context cancellation, bounded backoff and the distinction between a missed
pulse and a vanished device are unchanged.

**Files.** `internal/refclock/pps.go`, `internal/refclock/nmea.go`,
`DESIGN.md`, `internal/refclock/astra6_review_test.go`.

**Verification.** `TestAstra6PPSDeviceLossIsReported` uses a fake reader that
locks and then fails, with an opener that fails indefinitely, and requires an
invalidating measurement with reach 0 while `LastError` stays visible.
`TestAstra6NMEAChronology/a_device_replacement_re-establishes_ordering` covers
the NMEA state reset. **Deferred to the target hosts:** behaviour against a
real unplugged USB serial or PPS device.

## RA6X-005 — Re-priming PPS does not invalidate the selector's old lock — FIXED

**Changed.** `resetWindow` now arms `pendingInvalidate`, and `emit` — the
single exit point for every PPS measurement — carries it on exactly one
measurement and clears it. That covers every local reset that revokes the
estimate: a run of four consecutive rejections, a sequence restart, a fetch
timeout, an engine-requested `Reset`, a hard device loss and any future
recovery path, without each having to remember. An ordinary invalid
measurement deliberately *retains* the previous estimate in `SourceState`, so
without this an unlocked PPS at octal reach 360 stayed selected on a window it
no longer had. Clock-step generation resets stay a separate mechanism, and gap
counters, reach shifting, spike-fit behaviour, polling, lock hysteresis and
the accepted-pulse callback are untouched. NMEA's `resetWindow` does the same
thing, which is what RA6X-019 needs.

**Files.** `internal/refclock/pps.go`, `internal/refclock/nmea.go`,
`DESIGN.md`, `internal/refclock/astra6_review_test.go`.

**Verification.** `TestAstra6PPSResetInvalidatesTheEstimate` drives the real
`Run` loop over a scripted pulse train for both branches — four spikes, and a
sequence restart — and requires the emitted measurement to carry
`Invalidate`; a third subtest requires that an ordinary pulse does *not*.
`CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-019 — NMEA reacquisition reuses a stale pre-outage median — FIXED

**Changed.** Ordinary silence emptied reach without clearing the offset
window, so one fresh sentence requalified a mostly historical window and
stamped its median — computed before the outage, when the host had not yet
slewed or drifted — as current, and falsely precise. `NMEA.tick` now drops the
window when reach reaches zero, mirroring what PPS's `timeout` already did,
and `noteArrival` does the same for a sparse arrival that skips a whole reach
register's worth of slots with no intervening tick. Both go through
`staleWindow`, which also arms the invalidation from RA6X-005 and logs the
reason. `acceptLine` was reordered so freshness is evaluated **before** the new
sample joins the window, rather than appending it and discarding it a moment
later. Requalification then needs the ordinary minimum of four fresh accepted
sentences. Ordinary short packet loss is unaffected: the window survives until
reach actually empties. Sentence selection, the offset calibration sign,
source identity and cumulative counters are unchanged.

**Files.** `internal/refclock/nmea.go`, `internal/refclock/pps.go`
(`reachBits`), `DESIGN.md`, `internal/refclock/astra6_review_test.go`.

**Verification.** The named probe `TestAstra6NMEASilenceRequiresFreshWindow`
passes. `TestAstra6NMEAFreshnessCases` covers short packet loss keeping the
window, a long gap with no tick dropping it, and requalification taking the
full four fresh sentences. `CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-020 — One future GPS date can suppress all subsequent correct time — FIXED

**Changed.** The anti-replay watermark was committed from any checksum-valid
sentence, so one glitch dating a sentence in 2099 poisoned it before the
engine had accepted or refused the correction, and every later correct
timestamp was silently dropped — through window resets and successful
reconnects alike. `chronologyOK` now defines the rule explicitly:

- In sequence — after the watermark and no more than an hour ahead of it, since
  a receiver reporting once a second cannot advance further — accept and
  advance the watermark.
- Anything else — a repeat, a replay, or a jump too large to be progression —
  is refused **and is not committed**. A lone glitch therefore costs one
  sentence and nothing more.
- A genuinely new era still has to be adoptable, so `noteEra` accumulates
  evidence: four consecutive, *self-consistent* sentences adopt the new era,
  drop the window built in the old one, and become the watermark. That is
  bounded repeated evidence, not an open door — an arbitrary old replay has to
  sustain a consistent 1 Hz sequence — and the build-date guard still rejects
  week-rollover dates.
- Re-priming re-establishes ordering: `resetWindow` clears the watermark, so a
  window reset, a device replacement or a successful reconnect starts fresh
  rather than carrying the old device's era for ever.

Same-second duplicate suppression and GPS-rollover rejection are preserved.

**Files.** `internal/refclock/nmea.go`, `DESIGN.md`,
`internal/refclock/astra6_review_test.go`.

**Verification.** The named probe `TestAstra6NMEARecoversAfterFutureDate`
passes. `TestAstra6NMEAChronology` covers a mid-run glitch not poisoning the
watermark, a corroborated era change being adopted after exactly four
sentences, duplicate seconds still being suppressed, and a device replacement
re-establishing ordering. `CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-021 — ZDA can discipline time while the receiver reports an invalid fix — SKIPPED

**Reason.** The fix specification requires evidence this session cannot obtain
and a policy decision that is not Claude's to make. It asks to *"establish and
document a time-validity rule for supported receivers"* and to *"use
authoritative receiver validity for ZDA"*, with a verification step of
*"capture supported receivers during cold start, antenna loss, reacquisition,
and valid timing-only operation; confirm their RMC/GGA/ZDA semantics."* No GPS
hardware is reachable from here, and the review itself marks the finding
**Needs investigation** and warns to *"finalize the receiver policy before
making that assertion normative."*

Neither available answer can be chosen without that evidence. Gating ZDA on a
recent RMC `V` or GGA quality 0 would break the *"intentionally supported
NMEA-only/time-only operation"* the same specification says to preserve, since
many receivers keep valid UTC without a position solution — and the
specification explicitly forbids blindly requiring a 3D fix. Leaving it alone
keeps the contradictory state the finding describes. Guessing between them is
what the instructions say not to do.

**What is needed to close it.** Captures from the receivers carillon supports,
during cold start, antenna loss, reacquisition and timing-only operation,
showing whether their ZDA remains trustworthy while RMC reports `V`; then a
documented rule in `DESIGN.md` and the probe
`TestAstra6ZDAHonorsKnownInvalidTime` turned into policy-specific tests for
fresh-invalid, stale and absent status. The contradictory state the review
demonstrated is real and still present: a recent RMC `V` sets
`Info.Refclock.FixValid = false` while ZDA continues to form valid stratum-1
measurements.

## RA6X-022 — Leap warnings are coupled to loop updates and lack a fileless boundary reset — FIXED

**Changed.** Two separate defects.

*Consensus.* `s.leap = majorityLeap(sel.Survivors)` sat below the early returns
in `reselect`, so the survivor majority was recomputed only after a nonignored
loop update from the system source — a warning announced by the other
survivors while the system source's own lowest-delay winner was unchanged was
ignored entirely. It moved up to run on **every** reselection, immediately
after selection completes: accepted protocol metadata is published
independently of new phase feedback. A bare PPS still does not vote.

*Boundary.* The only boundary reset was conditional on a `LeapTable`, so the
supported upstream-authoritative topology never reset its samples or epoch
after the kernel applied a leap. `Engine.notePendingLeap` now tracks the UTC
boundary a survivor-majority warning implies — the end of the current UTC day —
arming it when the warning appears and dropping it if the warning is
withdrawn; `crossedAnnouncedLeap` fires exactly once when the wall clock
crosses it, and runs the same bracketed reset and `Resync` the leapfile path
uses. Leapfile authority when usable and configured is unchanged, as is the
post-leap holdover/resync behaviour.

**Files.** `internal/discipline/system.go`, `internal/engine/engine.go`,
`DESIGN.md`, `internal/discipline/astra6_review_test.go`,
`internal/engine/astra6_epoch_review_test.go`.

**Verification.** The named probe `TestAstra6LeapUpdatesWithoutSystemFeedback`
passes. `TestAstra6LeapConsensusCases` covers deletion, a minority not
carrying, a warning clearing without system feedback, and a bare PPS not
voting a warning down. `TestAstra6FilelessLeapBoundaryResets` runs both leap
directions with no leapfile and requires exactly one source reset, a bracketed
epoch change, no spurious step, and idempotence on a second crossing;
`TestAstra6FilelessLeapWarningClears` covers a withdrawn warning.
`CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-042 — NMEA leap-second parsing normalizes or rejects the same instant inconsistently — FIXED

**Changed.** `time.Date` normalises `23:59:60` into the following midnight, and
the two sentence types disagreed about it: RMC validated its calendar date
*before* the normalisation and so accepted the leap second as an ordinary
next-day sample, colliding with the real next midnight in the duplicate
watermark; ZDA validated the *normalised* date and rejected it. Second 60 was
also accepted at arbitrary minutes with no leap policy at all.

`parseNMEAClock` now refuses second 60 in both, with distinct reasons:
`23:59:60` is a real leap second that has **no representable instant**, and
second 60 at any other minute is malformed. Deliberate rejection rather than
silent normalisation is the connection to the leap policy: the kernel applies
the leap and the engine handles the boundary (RA6X-007), and losing one
sentence costs nothing at a 16-sample window with an 8-slot reach register.
Separately, the fractional field is now validated **in full** before being
truncated to the supported nine digits — `.123456789abc` used to pass as
123456789 ns because the suffix was cut off before it was checked. Valid
RMC/ZDA timestamps, accepted talkers, checksum requirements, supported
subsecond precision and duplicate suppression are unchanged.

**Files.** `internal/refclock/nmea_sentence.go`, `DESIGN.md`,
`internal/refclock/astra6_review_test.go`.

**Verification.** `TestAstra6NMEALeapBoundaryIsConsistent` tables both sentence
types at 23:59:59, 23:59:60 and 00:00:00 around an insertion date and on an
ordinary day, second 60 at an arbitrary minute, a malformed long fraction, and
fractions at and beyond the supported precision.
`TestAstra6TrueMidnightStaysUsable` confirms the real midnight sample is
unaffected. `CGO_ENABLED=1 go test -race ./...` passes.

---

## Wave 4 — make eligibility and synchronized state truthful

## RA6X-003 — Time-based source expiry never runs on engine ticks — FIXED

**Changed.** Selection ages uncertainty and rejects over-distance sources, but
it ran only on a measurement or an explicit lifecycle event. `System.Tick` did
phase slewing and an already-entered holdover timeout and nothing else, so if
every producer went silent a synchronized source stayed selected indefinitely
and the holdover timer never started. Three changes:

1. **`Tick` reselects.** Eligibility is now evaluated on every tick, so loss is
   noticed at the loss boundary rather than never.
2. **A per-source freshness deadline.** Phi ageing alone takes about 27 hours
   to push a source past `MaxDistance`, which is not a loss boundary.
   `freshnessDeadline(poll)` is eight poll intervals — the width of the reach
   register, so it is exactly "as many missed slots as would empty reach if
   anything were still reporting" — floored at 64 s so a very short poll cannot
   be tripped by ordinary jitter, and clamped to the legal poll range. A
   legitimately sparse poll-17 association is untouched; a 1 Hz refclock is not
   carried for a day. A stale source is reported as `unreachable`, an existing
   status value, so nothing public changed.
3. **Consumption is tracked per source.** `System.lastUsedAt` was a single
   watermark reset to zero on every system-source switch, so switching away
   from a source and back re-applied an observation the loop had already
   integrated — which a tick-driven reselection would have made far more
   frequent. It moved to `SourceState.usedAt`. (This also closes the sub-item
   RA6X-001 raises about the shared watermark; the rest of RA6X-001 is wave 5.)

A tick brings no new sample, so the watermark guarantees it cannot re-integrate
anything. Configured holdover duration, preference rules and the immutable
snapshots are unchanged.

**Files.** `internal/discipline/system.go`, `internal/discipline/select.go`,
`internal/discipline/measurement.go`,
`internal/discipline/astra6_review_test.go`,
`internal/engine/engine_test.go`.

**Verification.** `TestAstra6TickExpiresSilentSources` synchronizes, stops all
measurements, and advances only `Tick`: it requires HOLDOVER within the
source's own freshness deadline, then UNSYNCED after the configured holdover,
with LI=3 and stratum 16. `TestAstra6TickKeepsSparseButLegalPolls` and
`TestAstra6FreshnessDeadlineScales` guard the sparse-poll case and pin the
deadline table. `TestAstra6TickDoesNotReintegrate` requires ten ticks after one
observation to produce no further loop updates.
`TestEngineStepsOnceWithTwoSources` was made deterministic: it waited for a
sixth loop update that only existed because of the re-integration this fix
removes, and now waits for both scripts to be delivered plus the step it is
actually about. `CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-009 — Losing a source during settling promotes untrusted time to holdover — FIXED

**Changed.** The no-system branch read `case StateSettling, StateSynced: →
StateHoldover`, and HOLDOVER is served as synchronized by both the wire and
the kernel — so losing the last source *increased* the trust placed in a clock
the daemon had never finished settling, including immediately after a step.

`System` now tracks `everSynced`: set on entering SYNCED, cleared by a step,
which moves the clock out from under whatever synchronization preceded it.
Serviceable holdover is entered from SYNCED, and from SETTLING only when
`everSynced` still holds — the case where the filter withheld updates, or a
leap resync is in progress, after a spell of real synchronization. Settling
that never reached SYNCED goes to UNSYNCED with reason `INIT` instead. State
names, wire field meanings and holdover expiry are unchanged, as is the
previously-synced leap `Resync` path.

**Files.** `internal/discipline/system.go`,
`internal/discipline/astra6_review_test.go`.

**Verification.** The named probe `TestAstra6SettlingLossDoesNotSynchronize`
passes: status is not holdover, LI is `unsynchronized`, stratum is 16.
`TestAstra6HoldoverEntryRequiresSynchronization` covers the three remaining
cases — an already-synced daemon still holds over, loss right after a step does
not, and settling after a spell of synchronization still may.
`CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-010 — Failed polls count as successful settling evidence — FIXED

**Changed.** `if m.Source == s.sysName { s.sinceStep++ }` counted every
Measurement naming the system source — timeouts, bad authentication, rejected
packets and invalidation notices included — so a transport heartbeat could
complete settling on its own. Testing `Valid` instead would not have worked
either: a successful reply whose clock filter winner is unchanged is emitted
with `Valid = false`.

`Measurement` gained `Acquired`, which is deliberately a *third* thing beside
`Valid` and the error heartbeat: it means "this event is a successful
acquisition from the source in the current clock epoch". `Valid` implies it —
there is no new estimate without an acquisition — so producers only set it for
the acquisition-without-estimate case, and `Measurement.IsAcquisition()` reads
both. The NTP source sets it on a reply that was authenticated, plausible and
entered the filter, whether or not the filter winner changed; PPS sets it on an
edge that passed the sequence, interval and spike checks; NMEA sets it on a
sentence that passed parsing, validity, chronology and epoch checks. Misses,
bad MACs, bogus packets, KoD, stale epochs and reset notices do not. Settling
counts acquisitions. The earlier fix that made settling independent of
minimum-delay winner turnover, and the configurable count, are preserved.

**Files.** `internal/discipline/measurement.go`,
`internal/discipline/system.go`, `internal/source/ntp.go`,
`internal/refclock/pps.go`, `internal/refclock/nmea.go`,
`internal/discipline/astra6_review_test.go`.

**Verification.** The named probe `TestAstra6MissIsNotSettlingEvidence`
passes. `TestAstra6SettlingEvidenceKinds` tables the review's list — a good
reply with an unchanged winner and a new estimate both count; a timeout, a bad
MAC, an unusable stratum, a reset notice and a PPS spike do not.
`TestSystemLeavesSettlingWithoutAFreshLoopUpdate` still passes and remains
meaningful under the new semantics. `CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-023 — An expired authoritative leapfile can suppress valid upstream warnings — FIXED IN M5

**2026-09-08 resolution.** Refclock and serving hosts require a current durable
table; pure network clients may use fresh leap-capable survivor consensus.
The engine withdraws expired-file authority, gates kernel/NTP synchronization,
and retains an established UTC bound across updates and restart. NMEA and PPS
cannot vote on LI. Runtime readiness and expiry are reported in tracking,
health, metrics and logs. M5 tests cover expiry, backward clocks, restart,
upstream fallback and disconnected positive/negative leap execution. The
following paragraphs preserve why this was deferred in the original pass.

**Reason.** The fix specification presents a fork the maintainer has to choose:
*"either cease synchronized service or allow explicitly configured fallback to
current survivor consensus"*, and warns *"never silently change authority
merely to make the test pass."* The two options have opposite operational
consequences — one takes a working server out of service on a date nobody was
watching, the other keeps serving on evidence the file itself says may be
incomplete — and the second additionally implies a new configuration key,
which the review's own preamble says should not appear incidentally. Picking
one is a policy decision, not an implementation detail, so this is logged
rather than guessed.

**What is needed to close it.** A decision between the two policies. Once made,
the implementation is small and the diagnostic half comes with it: `Indicator`
gains an expiry test, `handle`/`publish` stop treating an expired table as
authoritative, and runtime near-expiry and expiry transitions are logged rather
than being reported only at startup — which is the part that reaches an
operator whose daemon crosses expiry months after it started.

**Behaviour at the original review.** `leap.Table.Indicator` never checks `Expiry`;
`handle` and `publish` override the survivor majority with its result whenever
`LeapTable` is non-nil. Monitor health already reports an expired file as
unhealthy, and the expiry and provenance are already exposed in JSON and
metrics, so the condition is observable even while the authority question is
open.

## RA6X-038 — Selection has no local timing-loop rejection — FIXED

**Changed.** Nothing compared a peer's reference against this host's own
identity, so after losing a real upstream two mutually configured instances
could begin selecting each other's retained time while advertising
independence, and a configured self-address was never rejected.

`discipline.Config` gained `LocalRefIDs`, the set of RFC 5905 §7.3 reference
identifiers naming this host — one per local unicast address, from
`net.InterfaceAddrs` at startup, so a multihomed host is covered whichever
address a peer reaches it on and a configuration pointing at the local server
is caught. `SelectWithLocal` refuses a candidate in either direction:
`SourceRefID` matching a local identity means the configured server *is* this
host; `RefID` matching one means our own time would come back to us. The check
is skipped for textual identifiers, so `GPS `, `PPS ` and kiss codes are never
mistaken for addresses, and for stratum 1, which has no upstream reference.
Two peers sharing a third upstream have equal RefIDs to each other and not to
ours, so ordinary shared-upstream configurations are unaffected. The rejection
reuses the existing `invalid` status — no new public vocabulary — and reports
itself through new `EventTimingLoop` / `EventTimingLoopCleared` events, logged
once per transition with a remediation hint. An empty identity set disables the
check, so a host whose interfaces cannot be enumerated still keeps time.

Documented limits, in the code: for IPv6 the identifier is a 32-bit hash, so a
collision (≈2^-32 per local address) would reject a legitimate peer — which
fails closed and costs one source; and a peer behind NAT reporting a public
address this host does not itself hold cannot be detected here.

**Files.** `internal/discipline/system.go`, `internal/discipline/select.go`,
`internal/engine/engine.go`, `cmd/carillon/main.go`,
`internal/discipline/astra6_review_test.go`.

**Verification.** `TestAstra6RejectsTimingLoops` tables a peer synchronized to
our IPv4 and IPv6 addresses, our own addresses configured as a server, a peer
sharing a third upstream, and stratum-1 peers with textual reference ids —
requiring rejection exactly in the first four and an event with each.
`TestAstra6TimingLoopTransitions` requires the event once per transition and
the peer usable again once its reference changes;
`TestAstra6NoLocalIdentityDisablesTheCheck` covers the enumeration-failed case.
**Deferred to the target hosts:** two real instances losing a shared upstream.

## RA6X-050 — A preferred source that never answers is never reported lost — FIXED

**Changed.** `reportPreferLost` required `preferSeen`, which came only from the
preferred source's own `everReachable`, so a miswired, misconfigured or
permanently unavailable preferred GPS stayed silently absent while fallback
service was reported healthy. What the suppression is *for* is the initial
acquisition phase, when nothing has established service yet and an ERROR at
every daemon start would be a false page — and that is now all it does:
`fallbackEstablished` (some source has survived, ever or now) gates the report,
and the preferred source's own history no longer does. The now-unused
`everReachable` field was removed. Exactly-once loss and recovery transitions,
fallback selection, `noselect` behaviour and the multiple-prefer selector
compatibility case are unchanged.

**Files.** `internal/discipline/select.go`,
`internal/discipline/astra6_review_test.go`.

**Verification.** The probe `TestAstra6ReportsNeverReachablePreferAfterFallback`
passes. `TestAstra6PreferLostPhases` covers initial acquisition staying quiet,
a delayed first answer clearing the report, later loss and recovery, and a
`noselect` preferred source not counting as a configured preference.

## RA6X-051 — PPS disagreement diagnostics outlive the comparison that produced them — FIXED

**Changed.** `DisagreesWith` and `Disagreement` were assigned only inside the
PPS-qualified branch, so a PPS that was rejected as a candidate, or whose
numbering source disappeared, kept a current-looking finding — sending an
operator hunting for an edge or calibration error when the present problem was
numbering loss. They are now cleared for every PPS source at the top of
`Select` and re-derived only where a comparison actually happens, so they always
describe the most recent selection or are empty. Qualification, locking, source
selection and the fields' meanings are unchanged.

**Files.** `internal/discipline/select.go`,
`internal/discipline/astra6_review_test.go`.

**Verification.** `TestAstra6ClearsObsoleteDisagreement` drives disagreement →
numbering loss, PPS reach zero, agreement and `noselect`, requiring the fields
to be empty in each.

## RA6X-037 — DNS address choice can pin an association to an unusable endpoint — FIXED

**Changed.** `resolve` returned `addrs[0]` and nothing else, so a hostname
whose first answer was unroutable or had stopped serving could never be
recovered from: re-resolution returned the same first answer. Worse,
`ENETUNREACH`/`EHOSTUNREACH` fell into the generic error branch and never
incremented `consecutiveTimeouts`, so re-resolution was not even attempted.

`resolve` now returns every answer, and the source keeps the list with an
index. `errNetworkUnreachable` classifies the two unreachable errnos and counts
them with the timeouts, as `ECONNREFUSED` already was. `ensureResolved`
separates the two failure kinds: a **temporary DNS failure** keeps the cached
answers (a stale address is better than none), while a **failing endpoint**
rotates to the next answer — refreshing the list first when DNS is working.
`setAddrs` keeps the current address if it is still among the answers, so a
re-resolution that changes nothing does not disturb a healthy association, and
`useAddr` resets the endpoint-specific state — the clock filter, whose samples
describe a different path, and `kodMinPoll`, which is not the new server's
opinion — when the peer actually changes. Rotation happens only on failure, so
a healthy association is never touched; a single-answer server keeps its filter
across a re-resolution. Literal addresses, bounded retries, address families,
request authentication and one active exchange per source are unchanged.
`Query` still uses the first answer, which is what "query this name" means.

**Files.** `internal/source/ntp.go`, `internal/source/source_test.go`,
`internal/source/astra6_review_test.go` (new).

**Verification.** `TestAstra6RotatesPastAnUnusableAnswer` runs the real poller
against a stable answer list whose first entry never replies and requires it to
reach the healthy second one. `TestAstra6EndpointRotation` covers a healthy
association not being disturbed, rotation through three answers and wrapping,
a single answer keeping its filter and poll policy, a peer change resetting
them, a temporary DNS failure keeping the cached answers while still rotating
away from a failing endpoint, a literal address, and a changed answer list.
`CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-039 — Device identity checks use path spelling instead of the underlying device — FIXED

**Changed.** Three separate consequences of comparing names instead of devices.

1. **The Linux same-tty prohibition.** `r.PPS == r.Device` let any alias walk
   past it — `/dev/serial/by-id/...` against `/dev/ttyUSB0` — after which N_PPS
   replaces the line discipline delivering the NMEA bytes. It now compares
   device identity, falling back to a literal comparison when a path cannot be
   identified.
2. **No ownership across refclocks.** Two configured refclocks could open one
   device and compete for its bytes or overwrite its parameters. `Check` now
   builds a device-identity map across every refclock and reports a conflict
   naming both blocks and both paths. Sharing *within* one refclock is
   explicitly allowed, which is the supported FreeBSD arrangement of one
   callout tty carrying both the NMEA stream and the PPS edge.
3. **PPS devices misclassified as ttys.** `linuxPPSPath` matched only a
   `ppsN` basename, so a stable udev alias such as `/dev/pps-gps` was treated
   as a tty and the daemon tried to attach N_PPS to a PPS device. The path is
   now resolved first, and if the resolved name still does not look like one
   the kernel is asked directly: a character device whose number appears under
   `/sys/class/pps/*/dev` is a PPS device whatever it is called. Read-only, and
   it opens no device.

Identity is `chardev:<rdev>` for a character device — the driver's device
number, shared by every name that reaches it — and the fully resolved path
otherwise. Operator-facing aliases keep working, symlinks are not forbidden,
and identity is re-derived on each open, so a retargeted alias is revalidated
when a device is reopened.

**Files.** `internal/config/config.go`,
`internal/config/refclock_check_linux.go`, `internal/pps/pps_linux.go`,
`internal/config/astra6_review_test.go`.

**Verification.** `TestAstra6DeviceIdentityFollowsTheDevice` shows a device and
a symlink to it sharing an identity, that it is the device number and not the
path, and that two different devices do not collide.
`TestAstra6RejectsSharedDevices` covers two refclocks claiming one device under
two names (rejected), one refclock using a device for both roles (allowed),
distinct devices, and `pps` keyword values not being treated as paths. The
Linux-only and PPS-classification paths cross-compile and `go vet` cleanly for
linux/amd64 and linux/arm64. **Deferred to the target hosts:** alias and
hotplug behaviour against real `/dev/ppsN`, `/dev/serial/by-id` and
`/sys/class/pps` entries, and confirmation of the supported FreeBSD sharing.

## RA6X-057 — Source-count validation does not express an achievable or independent quorum — SKIPPED

**Reason.** The fix specification opens with a definition the maintainer has
to supply: *"Define whether min_survivors means surviving associations or
independent clocks and document dependent refclock roles."* Everything else it
asks for follows from that choice. Counting *associations* makes two names for
one endpoint two votes, and a GPS's NMEA and PPS logical sources two votes for
one physical clock; counting *independent clocks* changes what existing
configurations mean — a working `min_survivors = 2` on a host with a GPS
refclock could start refusing to synchronize after an upgrade. The
specification also requires preserving *"any intentionally supported
monitoring-only configuration through an explicit policy"*, which is a second
decision about whether an all-`noselect` configuration is legal.

Choosing either meaning silently would change the semantics of a documented
configuration key, which the review's preamble rules out. Logged rather than
guessed.

**What is needed to close it.** A stated definition of `min_survivors` in
`DESIGN.md`, and a decision on whether all-`noselect` and bare-PPS-without-
numbering configurations are legal-but-idle or invalid. Given those, the rest
is mechanical: static diagnosis of impossible minima at `-check` time,
duplicate resolved-endpoint detection, and a documented note that a GPS's NMEA
and PPS sources are one clock.

**Present behaviour, unchanged.** `Validate` requires at least one source
block, unique names and `MinSurvivors >= 1`; selection counts `SourceState`
entries; `cluster`'s stopping floor is a fixed three. A `min_survivors` larger
than the number of selectable sources is accepted and leaves the daemon
permanently unsynchronized with no startup explanation.

---

## Wave 5 — the coupled discipline redesign

These six were resolved together, as the review requires. The acceptance gate
is the review's own reproduction: `python3
review/2026/09/takeover-repro/reproduce.py baseline`, which now **passes in
full** — all twelve delayed-feedback cases and the drift-persistence case.

## RA6X-008 — Phase debit uses the next frequency word for the previous interval — FIXED

**Changed.** `Tick` computed the *next* frequency word and debited that from
the pending phase, so the accounting disagreed with what the kernel had
actually been running. It now **charges first, then issues**:

- `chargeApplied` debits `(applied − appliedBase)·dt` — the word that was
  issued, minus the base in effect *when it was issued*, which a loop update
  since may have changed — and only then is the next word computed.
- The accounting point (`chargeFrom`) moves on every loop **update** as well
  as every tick. A new observation replaces the residual, and whatever was
  corrected before that observation was taken is already in the offset it
  reports; charging that interval again would debit it twice.
- **First tick:** charges nothing. Nothing has been issued yet, so no
  transient has run; the old code charged a nominal second for one that had
  not.
- **Step:** starts a fresh interval and stops treating the word still in the
  kernel as a transient, since there is no residual left for it to be charged
  against.
- **Backward time:** charges nothing. **Long stall:** charged at the 2 s
  ceiling rather than reduced to a nominal second — the word really did stay
  applied — while still bounding a pathological monotonic jump.

Sign conventions, the ±500 ppm total bound, the configured phase-slew ceiling
and the absence of a base-only pulse between updates are unchanged. `DESIGN.md`
§6.4 carries the corrected pseudocode.

**Files.** `internal/discipline/loop.go`, `DESIGN.md`,
`internal/discipline/loop_test.go`,
`internal/discipline/astra6_review_test.go`.

**Verification.** The review's probe `TestVerification011ChargesTheAppliedWord`
passes — it derives its expectation from `Applied()`, so the corrected
first-tick semantics do not disturb it.
`TestAstra6ChargesTheAppliedWord` restates it with the 39.0625 ppm / 1.5 s
arithmetic explicit. `TestAstra6AppliedWordIntegral` is the independent oracle
the review asked for: it models the kernel word and requires, at every
accounting point, that the phase the loop debited equals the integral of the
word actually held over the interval it was actually held for — across
irregular ticks, a tick past the accounting window, updates that change the
base, a sign change and saturation, at four base frequencies.
`TestAstra6NoDoubleDebitAcrossUpdate` and `TestAstra6AccountingEdgeCases`
cover the reconciliation and the remaining semantics. Three existing loop
tests were updated to the corrected first-tick behaviour, each with the reason
in a comment.

## RA6X-001 — Delayed filter observations destabilize the discipline loop — FIXED

**Changed.** Observation-time semantics are now defined across filter,
combination and loop, taking the review's second option: **propagate the
observation to the current clock using the recorded applied corrections.**

`Loop` keeps `slewLog`, the cumulative phase it has applied, sampled at every
accounting point and bounded to twice the Allan intercept. `AppliedSince(at,
now)` interpolates within an accounting interval, where the frequency word is
constant, so the answer is exact rather than an estimate. Selection then
expresses every stored offset at the selection instant — `Current = Offset −
AppliedSince(At, now)` — and the intersection, clustering, PPS agreement,
combination and prefer paths all use `Current`. The raw `Offset` is untouched
and is what status reports. The oscillator's own drift over the interval is a
separate uncertainty and is already carried by the dispersion the filter ages
at φ, which is the explicit uncertainty model the finding asks for. Per-source
consumption tracking, the other half of this finding, landed with RA6X-003.

**Files.** `internal/discipline/loop.go`, `internal/discipline/select.go`,
`internal/discipline/system.go`, `DESIGN.md`,
`internal/discipline/astra6_review_test.go`.

**Verification.** `python3 review/2026/09/takeover-repro/reproduce.py
baseline` now passes all twelve `TestTakeoverDelayedFeedback` cases,
including the four the review recorded as failing and the steeper symmetric
RTT growth the distance-ranking experiment could not fix. At poll 6 with
symmetric variation the peak frequency error falls from **358.227 ppm to 3.592
ppm** and the final-hour RMS from **167.077 ms to 0.000 ms**; at poll 8 from
224.474 ppm / 322.020 ms to 0.901 ppm / 0.012 ms. The asymmetric cases, which
carry genuine measurement bias rather than a discipline defect, also improve
markedly. `TestAstra6ObservationsArePropagated` and
`TestAstra6PropagationUsesTheAppliedPhase` pin the mechanism.
`CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-002 — The filter can withhold useful corrections for many polls — FIXED

**Changed.** The clock filter ranks the eight stages by **root distance**
(`delay/2 + dispersion`) instead of raw `delay`. A delay advantage is worth
δ/2 to the offset estimate, not δ, so a fast early sample loses its place once
φ·age outweighs half its advantage — about 11 minutes for a 20 ms advantage —
rather than lasting past the 34-minute Allan intercept. This is the deviation
from RFC 5905 §10 and ntpd that `DESIGN.md` said wanted a simulation pass
first; the pass is the takeover reproduction, and it is applied together with
RA6X-001's propagation, without which it does not fix delayed feedback. The
eight-stage bound, the `MaxDispersion` and over-Allan handling, and the
outlier rejection are unchanged.

The design's two incorrect statements are corrected in place: the claim that
an over-Allan observation "must also lose on raw delay" (line 90 explicitly
re-ranks it to `MaxDistance + dispersion`), and the 11-minute figure for a
10 ms RTT advantage, which omitted the half-delay factor — 11 minutes is the
figure for a **20 ms** advantage.

**Files.** `internal/discipline/filter.go`, `DESIGN.md`,
`internal/discipline/filter_test.go`,
`internal/discipline/astra6_review_test.go`.

**Verification.** In the reproduction the maximum age of the selected
observation falls from **448 s to 0 s at poll 6** and **1792 s to 0 s at poll
8**, with an update on essentially every poll (675 of 676, 169 of 169).
`TestFilterWithholdsUpdatesWhileAnEarlyBestSampleStands` was inverted from
documenting the defect to bounding it: the withholding run must now be inside
the distance crossover and must *not* reach the Allan intercept.
`TestAstra6FilterCadence` repeats that at four poll intervals. Two other
filter tests were updated to the new ranking with the arithmetic spelled out.

## RA6X-024 — Filter startup uncertainty omits all unfilled stages — FIXED

**Changed.** Neither extreme is usable here, so the finding's second option —
a documented conservative admission policy — was taken. RFC 5905's MAXDISP
stages report about 7.9 s for one sample, past `MaxDistance`, which makes a
fresh association inadmissible for five or six packets; omitting the stages
entirely reported one packet carrying 1 ms of dispersion as 0.5 ms, *more*
certain than the single measurement it rests on. Absent stages now contribute
a bounded `primingDispersion` of 1 s at their rank weight — seeded into the
same halving recurrence, so the shape matches the RFC's sum — giving at least
500 ms of uncertainty for one sample, decaying to 2 ms by the eighth. An
unprimed source stays admissible (λ ≈ 0.51 s on an ordinary path) so a host
with nothing else can bootstrap from it, any primed source outranks it, and
the root dispersion served during acquisition is honest. Quality is refreshed
on every selection through RA6X-001's propagation and RA6X-025's per-stage
metadata, so evolving uncertainty is published without reintegrating old
offsets. The one-line dispersion patch was not applied, and no falseticker or
step assertion was weakened.

**Files.** `internal/discipline/filter.go`,
`internal/discipline/measurement.go`, `DESIGN.md`,
`internal/discipline/filter_test.go`.

**Verification.** `TestFilterSingleSample` now requires the one-sample
dispersion to be at least the sample's own, with the exact expected value.
`TestFilterPrimingDispersionDecays` requires the term to fall monotonically
from one to eight samples, to vanish on a full register, and — at every count
— to leave the root distance inside `MaxDistance` so an unprimed source can
still bootstrap. `TestFilterDispersionAges` carries the priming term
explicitly. The whole takeover reproduction still passes with the change in
place, so admission and falseticker behaviour are unaffected.

## RA6X-025 — Filtered offsets are paired with metadata from a different packet — FIXED

**Changed.** `hit` obtained the filter output and then handed `emit` the
*latest* exchange's packet, so a historical offset travelled with the newest
stratum, root delay, root dispersion, reference identity, reference time,
precision and leap bits — describing no actual sample. A `stageMeta` ring the
same depth as the filter now records those fields for every exchange, and the
emitted measurement carries the metadata of the observation whose offset is
used. The peer's own identity (`SourceRefID`) stays a property of the
association and comes from the address.

For material quality changes, the coarse and rare signal is the upstream's
**stratum**: a change re-primes the filter, since the samples already held
describe a different quality of service. A reference-id change at a stable
address is ordinary for a stratum-2 peer and is handled by the per-stage
metadata rather than by resetting.

Advertised root dispersion now ages from `rootDispAt`, the moment the estimate
was *received* — which is what the filter aged its own dispersion to — instead
of from the handover instant. Ageing from `now` discarded the elapsed age the
source's selection distance had already accounted for, so switching to an
older survivor advertised less uncertainty than it had; ageing from the
sample's own `At` would have double-counted the interval the filter had
already covered. External status names and the wire format are unchanged.

**Files.** `internal/source/ntp.go`, `internal/discipline/system.go`,
`DESIGN.md`, `internal/source/astra6_review_test.go`.

**Verification.** `TestAstra6MetadataBelongsToItsObservation` runs the real
poller against a server whose first reply is fast and low-delay with one
reference identity and whose later replies are slow with another and wholly
different root delay and dispersion; every valid measurement must carry a
consistent pair. `TestAstra6StratumChangeReprimesTheFilter` observes the
re-prime from outside, as the priming uncertainty returning.

## RA6X-013 — A frozen transient frequency is accepted as stable drift — FIXED

**Changed.** The gate measured the spread of repeated base-frequency readings,
which are flat by construction when nothing is being measured — so a bad
transient left standing during filter starvation or source silence looked
perfectly stable after 900 s and overwrote a known-good drift file. Each
`freqSample` now carries the loop-update count, and `frequencySettled`
additionally requires **at least two accepted loop updates spanning the
window**. Readings are recorded only in `StateSynced` — while settling, in
holdover or unsynchronized the kernel word is a guess being carried, not a
measurement — and the history is discarded when the **system source** changes
or a **step** happens, so evidence from different measurement chains is never
averaged. Flat repeated reads alone can no longer establish stability, and a
frozen tail longer than the window disqualifies itself because the update
count stops advancing. The last known-good file is preserved on insufficient
evidence; the scalar format, atomic replacement and the configurable
spread/window are unchanged.

**Files.** `internal/engine/engine.go`, `DESIGN.md`,
`internal/engine/engine_test.go`.

**Verification.** `TestTakeoverFrozenFrequencyMustNotOverwriteDrift` in the
review's own reproduction now passes: the previously stored 6.125 ppm survives
instead of being replaced by the frozen 30.539062 ppm.
`TestEngineDoesNotPersistAMovingFrequency` and
`TestEngineFrequencySettledGate` still pass — the latter updated to drive the
gate through synchronized snapshots carrying update counts, so it still covers
insufficient history, excessive movement and a steady estimate settling again.
`TestEngineStepsSettlesAndPersistsDrift` proves independently supported stable
observations still permit the write.

---

## Wave 6 — fatal errors, persistence and shutdown ownership

## RA6X-012 — Panic refusal is logged but does not stop the daemon as specified — FIXED

**Changed.** `DESIGN.md` §6.6 says a refused panic correction is fatal — log
the offset, exit 1, let the service manager's backoff make it visible — but it
was implemented as an ordinary event plus a state change, after which `Run`
carried on applying corrections and ticks. `handle` now returns a typed
`engine.ErrPanicRefused` when the discipline reports `EventPanicRefused`,
**after** publishing the snapshot, so the PANC diagnostic is visible to
operators and every client before the run ends. `Run` breaks on it like any
other fatal result, `restoreBaseFrequency` runs (it is suppressed only for
`errFrequencyRefused`), the sources and auxiliaries are cancelled, and `main`
returns `exitRuntime` = 1. The deliberate startup exception (`panic_at_startup`
with stepping permitted, RA6X-011) and the no-step-on-exit rule are unchanged.
Source-stop reselection propagates the same way (RA6X-017).

**Files.** `internal/engine/engine.go`, `DESIGN.md`,
`internal/engine/astra6_epoch_review_test.go`.

**Verification.** `TestAstra6PanicRefusalIsFatal` asserts the typed error, the
published UNSYNCED/PANC status, and that nothing stepped.
`TestAstra6PanicRefusalEndsRun` drives the real `Run` with a scripted source
and requires prompt termination with `ErrPanicRefused`, no step, and the
kernel left at the base estimate. `TestSimPanicRefused` still covers the
selection-level behaviour. `CGO_ENABLED=1 go test -race ./...` passes.

## RA6X-044 — Synchronous persistence can freeze discipline while the NTP server serves stale synchronization — FIXED

**Changed.** Two independent halves.

*Persistence off the engine goroutine.* `driftWriter` is a single-owner worker
with a **one-slot candidate channel**. The engine validates the frequency
(`clock.CheckFrequency`, RA6X-014) and hands it over without blocking;
`offer` replaces a pending candidate rather than queueing behind it, so a
delayed write can never overwrite a newer accepted value. Failures are logged
once and cleared when the file becomes writable again — the same throttling as
before, now owned by the worker. Atomic replacement (write, fsync, rename) and
the stable-write gate (RA6X-013) are unchanged. On shutdown the worker is
closed with a 2 s bound and a diagnostic if it does not finish.

*An independent age limit on served status.* `engine.Status` gained
`PublishedMono`, the engine's monotonic processing time at publication — wall
time cannot serve here, because a daemon whose job is to step the wall clock
has no monotonic guarantee there. The NTP listener's status closure now
computes the snapshot's age from the monotonic clock: past 30 s (thirty
publications, since the engine publishes every tick) it serves
**unsynchronized** with `LI = 3` instead of vouching for a clock nobody is
watching, and while fresh it ages the advertised root dispersion by φ over the
interval since publication.

*Observer contract.* The `Observe` doc comment now states plainly that it runs
on the engine goroutine, must not block, and that the statistics recorder's
bounded non-blocking queue is the model — with the served-status age limit
named as a bound on the damage, not a substitute.

**Files.** `internal/engine/engine.go`, `cmd/carillon/main.go`, `DESIGN.md`,
`internal/engine/astra6_review_test.go`.

**Verification.** `TestAstra6DriftWriteDoesNotStallTheEngine` injects a
writer that blocks, holds a write open, then offers 100 further candidates:
none of the offers block, the slot never holds more than one, and exactly two
writes happen — the blocked one and the single surviving candidate.
`TestAstra6StaleSnapshotIsNotSynchronized` checks the monotonic publication
stamp and that it does not follow the engine once it stops publishing.
`TestAstra6ShutdownRestoresBeforeDraining` covers the worker's bounded close.
**Deferred to the target hosts:** an actual filesystem stall.

## RA6X-045 — The shutdown deadline excludes the engine and frequency restoration — FIXED

**Changed.** The sequence was cancel → `wg.Wait()` → `restoreBaseFrequency` →
`maybeWriteDrift` → return, so a source that ignores cancellation, or a wedged
filesystem, could hold the kernel at the phase-slew transient — up to
±500 ppm, 43 s/day — until a service manager killed the process. That is
exactly what the restore exists to prevent.

The engine now **restores the clock first**, immediately after cancelling: it
owns the actuator and no source can influence it once the loop has left, so
there is nothing to wait for. Then the best-effort drift write, bounded to 2 s.
Then a bounded 5 s source drain that names any goroutine still running, using a
registry the engine keeps as it starts them. Single-writer clock ownership,
ordinary clean shutdown, the no-exit-step rule and the no-frequency-retry rule
after a rejected syscall are unchanged; `DESIGN.md` §12 documents the order and
why it matters.

**Files.** `internal/engine/engine.go`, `DESIGN.md`,
`internal/engine/astra6_epoch_review_test.go`.

**Verification.** `TestAstra6ShutdownRestoresBeforeDraining` runs a source
whose `Run` deliberately never returns alongside a working one, synchronizes
so a nonzero transient is applied, then cancels: `Run` must return within the
bounded drain and the last actuator write must be the base estimate. No real
clock or driver hang is involved, as the review requires.

---

## Wave 7 — protocol and authenticated limiting

## RA6X-026 — FreeBSD answers directed broadcasts as unicast requests — FIXED

**Changed.** `192.0.2.255` is the directed broadcast of a /24 and an ordinary
host address on a /23, so nothing but the interface configuration can tell
them apart. Linux reports the delivery in `recvmsg`'s flags word; FreeBSD does
not — `MSG_BCAST` and `MSG_MCAST` are NetBSD/OpenBSD constants that appear in
no FreeBSD header — and it matters more there, because `in_pcbbind_setup`
accepts a broadcast address as local, so the reply actually left the host with
a broadcast source copied from `IP_RECVDSTADDR` into `IP_SENDSRCADDR`.

`localBroadcasts` reads this host's interface addresses and computes their
IPv4 directed-broadcast addresses, cached for a minute and refreshed lazily on
the receive path — so an interface change is picked up without any
platform-specific network-change notification. The listener consults it on
every platform, after the existing address-based and flags-based checks, and
counts exactly one martian outcome. A /31 or /32 contributes no broadcast
address (RFC 3021). No `MSG_BCAST` constant is imported and no address-suffix
heuristic is used; valid unicast addresses ending in `.255`, non-/24 subnets,
IPv6, wildcard binding and multihomed reply-source selection are unaffected.

**Policy when the metadata is unavailable**, as the finding requires: if the
interface list cannot be read at all, the destination is treated as *not* a
broadcast and the failure is logged once — failing closed would stop the
server answering anything, which is worse than the narrow case this guards. A
*later* failed refresh keeps the last known configuration rather than losing
the check.

**Files.** `internal/server/broadcast.go` (new),
`internal/server/listener.go`, `internal/server/pktinfo_freebsd.go`,
`DESIGN.md`, `internal/server/astra6_review_test.go`.

**Verification.** `TestAstra6DirectedBroadcastIsRecognised` tables a /24, a
/23 (whose broadcast is *not* the `.255` of the address's own third octet), a
/31, loopback and IPv6, requiring exactly the broadcast addresses to be
recognised and ordinary hosts not to be.
`TestAstra6BroadcastMetadataPolicy` covers enumeration failing before anything
is known, the cache not re-enumerating inside the refresh window, and a later
failure keeping the last known configuration. **Deferred to the target hosts:**
FreeBSD packet captures for /24 and non-/24 directed broadcasts, limited
broadcast, multicast and valid secondary unicast addresses, and the existing
FreeBSD martian fixture.

## RA6X-027 — Required-key authentication failures bypass all response limiting — FIXED

**Changed.** The crypto-NAK for a bad MAC from a `require_key` prefix was
returned *before* the limiter was consulted, so 100 bad-MAC requests produced
100 NAKs at arrival rate with burst 8. The rate limiter gained a second
keyspace, `spaceCrypto`, separate from the service buckets, so
invalid-authentication work and NAK emission can never spend the tokens a
legitimate authenticated peer needs.

Two things are bounded, and only one of them can be bounded per address
without starving the very peer a spoofer is impersonating — the fix is
explicit about which is which:

- **Replies.** A crypto-NAK is emitted only while the crypto budget has a
  token, and only a *failure* spends one. That bounds the reflection the
  finding demonstrates.
- **Work.** An address the configuration does not expect to authenticate has
  its CMAC metered, so a public listener cannot be made to do crypto on
  demand. A `require_key` peer is always verified: refusing to verify it
  because someone is flooding its address is exactly the starvation the
  separate keyspace exists to avoid, and the finding's own verification step
  requires a valid required-key request to succeed after such a flood.

Silent rejection of a missing or wrong-but-permitted key, MAC verification
before trusting any key identity, ACL order, response-size bounds and
exactly-one-terminal-outcome accounting are preserved.

**Files.** `internal/server/ratelimit.go`, `internal/server/responder.go`,
`DESIGN.md`, `internal/server/astra6_review_test.go`,
`internal/server/responder_test.go`.

**Verification.** The existing flood probe
`TestVerification035CryptoNAKFloodIsBounded` passes, and
`TestAstra6CryptoNAKFloodIsBounded` repeats it with the added requirement that
the peer's own authenticated request is still served afterwards.
`TestAstra6AuthBudgetsAreSeparate` floods absent, unknown and wrong keys in
turn and requires a valid required-key request to succeed after each.
`TestAstra6LimiterKeyspacesAreDistinct` pins the separation.
`TestAuthenticatedPeerSurvivesASpoofedFlood` was updated: it asserted that the
flood "never reaches the limiter", which is the property RA6X-027 says must
change; it now requires every packet to be accounted for exactly once across
`bad_auth` and `rate_limited`, the NAKs to be bounded, and the peer's own
authenticated poll to be served — a stronger statement than before.

## RA6X-028 — Optional authenticated clients share the unsigned client's bucket — FIXED

**Changed.** With an optional key, `h.verify` ran *after* `limitKey` had been
computed, so `replyKey` was nil and every request from the address used key id
zero — spoofed unsigned traffic could exhaust a correctly authenticated
client's allowance. Verification now happens before the service bucket is
chosen for optional keys as well, so the bucket is keyed by the identity that
was actually verified. The crypto work that makes that possible is metered
from the separate keyspace (RA6X-027), so moving it earlier does not hand a
public listener unbounded CMAC. Optional authentication is preserved:
configuring keys alone still does not require clients to authenticate, and no
unverified trailer id ever allocates privileged tokens.

**Files.** `internal/server/responder.go`, `internal/server/ratelimit.go`,
`DESIGN.md`, `internal/server/astra6_review_test.go`.

**Verification.** The named probe `TestAstra6OptionalAuthHasIndependentBucket`
passes: after an unsigned request has drained the burst, a valid optional-key
request from the same address is still answered.
`TestAstra6LimiterKeyspacesAreDistinct` and the RA6X-027 tests cover the rest.

## RA6X-029 — Authenticated clients cannot authenticate this server's RATE replies — FIXED

**Changed.** The RATE path passed `nil` as the reply key unconditionally, so a
kiss to an authenticated client was unsigned; carillon's own client rejects a
missing MAC before it interprets the kiss code, recorded bad authentication,
and never executed the backoff — two instances could not honour their own
rate-control protocol. The RATE reply is now signed with `replyKey`, which is
non-nil only after a successful verification, so a reply is never signed on
the strength of a key id a request merely claims. Unsigned clients still get
an unsigned kiss, and KoD emission limits, anti-amplification bounds, origin
echo and silent drops where no KoD is due are unchanged.

**Files.** `internal/server/responder.go`, `DESIGN.md`,
`internal/server/astra6_review_test.go`.

**Verification.** The named probe `TestAstra6AuthenticatedRATEIsAuthenticated`
passes — the RATE response verifies under the client's key.
`TestAstra6UnsignedRATEStaysUnsigned` requires an unsigned client's kiss to
remain a bare 48-byte header.

## RA6X-030 — RATE backoff is not consistently applied or cleared — FIXED

**Changed.** Four separate defects, all of them about a demand outliving the
peer that made it or being undercut by scheduling:

1. **The configured bound is immutable.** `handleKiss` assigned
   `n.cfg.PollMax = want`. A new `effPollMax` carries the *association's*
   effective maximum; `cfg.PollMax` never changes.
2. **Endpoint change clears the demand.** `resetEndpointState` (shared with
   RA6X-037) now resets `effPollMax`, the current poll, the burst flag, the
   per-stage metadata and the stratum watermark alongside `kodMinPoll` and the
   filter, so a replacement server does not inherit the previous one's
   expanded maximum and long poll.
3. **A RATE ends an iburst.** `handleKiss` sets `kissedRate`, and the burst
   loop stops on it: the remaining two-second requests are exactly what the
   server asked us to stop sending.
4. **The jitter cannot undercut a demanded minimum.** `pollInterval` takes a
   floor exponent and applies it *after* the ±5 % jitter, so an actual send
   never precedes the interval a server demanded — which is how a client earns
   a second kiss, or a `DENY`.

Randomized scheduling, bounded DNS retry, ordinary iburst behaviour and
legitimate configured long polls are preserved; `DESIGN.md` §5.4 states which
state survives re-resolution to the same endpoint (all of it — `setAddrs`
keeps the current address when it is still among the answers).

**Files.** `internal/source/ntp.go`, `internal/source/poll.go`, `DESIGN.md`,
`internal/source/astra6_review_test.go`.

**Verification.** `TestVerification017AddressChangeDropsKissMaximum` (the
review's own fixture, exercised through the same code path) and
`TestAstra6RateBackoffIsAssociationScoped` require every demanded value to be
dropped when the endpoint changes. `TestAstra6PollJitterRespectsADemandedFloor`
runs 10 000 draws against a demanded floor and requires none to fall short,
and separately confirms the jitter *is* free to go below when there is no
floor, so the test proves something. `TestAstra6RateEndsAnIburst` drives the
real poller against a server that answers every request with a RATE and
requires exactly one request to have been sent.

## RA6X-031 — An untrusted RATE can suppress polling for over a day — FIXED

**Changed.** The only cap on a server's demanded exponent was
`discipline.MaxPoll` = 17 — 131072 s, about 36.4 hours — and it overrode
`poll_max`. `maxRemoteBackoffPoll` = 13 is now the ceiling on *remotely
demanded* backoff, which is [RFC 8633 §5.4](https://www.rfc-editor.org/rfc/rfc8633.html#section-5.4)'s
recommendation, raised only when the operator's own `poll_max` is already
larger — an operator's long poll is their decision, a server's demand is not.
The clamp is logged with the demanded value, the cap and whether the kiss was
authenticated. Origin and address validation, authenticated `DENY`/`RSTR`
semantics and continued bounded polling with visible state are unchanged: the
association keeps polling at the capped interval rather than silently sleeping
for an attacker-selected day-scale interval.

**Files.** `internal/source/ntp.go`, `internal/source/poll.go`, `DESIGN.md`,
`internal/source/astra6_review_test.go`.

**Verification.** `TestAstra6RemoteBackoffIsCapped` tables a demand beyond the
cap, at it, below it, inside `poll_max`, an operator's own poll-17
configuration (kept), and an operator's poll-15 configuration with a smaller
demand (not raised) — asserting in every case that `cfg.PollMax` itself was
not mutated.

## RA6X-040 — Negative root-delay interoperability needs an explicit representation policy — SKIPPED

**Reason.** The fix specification's first instruction is to gather evidence
this session cannot gather: *"Check supported ntpd/chrony implementations and
relevant protocol versions"*, with a verification step of *"compare actual
peer packets"* — and the review records that *"no affected live-peer capture
was obtained in this review."* The two authorities also disagree: RFC 4330 §4
describes root delay as signed and explicitly allows small negative values,
while RFC 5905 describes the short format as unsigned, and which one carillon
must interoperate with is a question about what real peers put on the wire,
not one that can be settled from the source.

Changing the shared `Short.Seconds` conversion without that evidence would
either keep rejecting a legitimate peer or start accepting a field the current
RFC says cannot be negative, in a type also used for root *dispersion*, which
must stay unsigned and nonnegative.

**What is needed to close it.** Captures from the ntpd and chrony versions
carillon is expected to peer with, showing whether any of them ever emits a
root delay with the high bit set. If they do, the fix is a field-specific
signed decode for `RootDelay` only, with raw-bit tests around zero, small
negatives, the maximum positive and negative-delay-plus-dispersion
combinations per version, and a bound that rejects genuinely excessive
negative values rather than letting them cancel uncertainty.

**Present behaviour, unchanged.** A raw `RootDelay` of `0xffff0000` decodes as
+65535 s and is then rejected by the root-distance check, so such a peer is
unusable rather than misinterpreted. Root dispersion is unaffected.

---

## Wave 8 — control and monitor lifecycle and freshness

## RA6X-033 — Control calls ignore cancellation after connecting — FIXED

**Changed.** `DialContext` handles cancellation only while dialing; once
connected, a cancellable context with no deadline did not interrupt a blocked
response read, so an indefinite `waitsync` hung after its caller had given up
and only closing the connection released it. `Call` now installs a
`context.AfterFunc` that closes the connection on cancellation and stops it on
every exit, and reports the error as `ctx.Err()` when the context is what
ended the call, so `errors.Is(err, context.Canceled)` and
`context.DeadlineExceeded` still identify it behind the closed connection.

A nonzero `WaitSync` timeout also becomes a **client-side context deadline** —
the requested wait plus the server's reply allowance — instead of being sent
in the request and trusted; an unresponsive server could previously exceed it
by ten seconds. `timeout = 0` keeps its documented meaning of waiting
indefinitely for as long as the context is live, and the startup retry for a
missing or refused socket is unchanged.

**Files.** `internal/control/client.go`,
`internal/control/astra6_review_test.go`.

**Verification.** The named probe `TestAstra6CallHonorsCancellation` passes:
`Call` returns within 2 s of cancellation with a `context.Canceled`.
`TestAstra6CallCancellationPoints` covers cancellation before dial and a
server that accepts and then never answers, requiring the client's own
deadline to bound the call rather than the server's promise.

## RA6X-034 — Abandoned waitsync requests accumulate and can deadlock listener failure — FIXED

**Changed.** Three things:

1. **Handlers get a child context.** `Serve` derives `hctx` from its own
   context and cancels it on every exit before `wg.Wait()`. The fatal Accept
   branch previously waited for handlers whose context would not be cancelled
   until the *caller* learned Serve had failed — a deadlock on the error path.
2. **A waiting request is bound to its connection.** `watchClose` reads one
   byte on the connection in parallel with the wait; the protocol is one
   request and one response, so any read result means the client is finished.
   A zero-timeout `waitsync` cleared its deadlines and waited only on the
   daemon's context, so nothing noticed a client that had gone away —
   accumulation that ordinary disconnects cause, with no hostile intent.
3. **Concurrent clients are bounded.** `maxConcurrentClients` = 128 is a
   documented admission policy, well beyond any legitimate use (`carillonctl`
   makes one connection and closes it), with a refusal logged.

Legitimate long `waitsync` requests, the one-request/one-response protocol and
the bounded reply write are unchanged.

**Files.** `internal/control/server.go`,
`internal/control/astra6_review_test.go`.

**Verification.** `TestAstra6WaitSyncDisconnectDoesNotLeak` connects, sends
`waitsync 0` and disconnects twenty times while the daemon is unsynchronized,
requiring the waiter count to return to zero.
`TestAstra6FatalAcceptDoesNotDeadlock` parks a live waiter and then closes the
listener under `Serve`, requiring Serve to report the failure promptly instead
of blocking on a handler it had not cancelled.

## RA6X-052 — Monitoring server lifecycle does not fully own its listener and shutdown — FIXED

**Changed.** `http.Server.Close` only closes listeners it has been given, and
`Serve` is what gives it this one — so `Close` before `Serve` left the port
bound and a startup that unwound after binding the monitor could not be
retried. `Close` now closes the bound listener directly as well, ignores an
already-closed listener, and is safe to call repeatedly and after `Serve`.

`Serve` no longer returns while shutdown is still running: the cancellation
`AfterFunc` signals a channel on completion, and if `stop()` reports the
callback was already running, `Serve` joins it before returning. If the
graceful deadline passes with connections still open, they are now
force-closed rather than the timeout merely being logged. `Serve` also closes
the listener on its own way out, so a caller can rely on the advertised
lifecycle being finished. Handlers, ACLs, status codes and normal graceful
request completion are unchanged.

**Files.** `internal/monitor/server.go`,
`internal/monitor/astra6_review_test.go`.

**Verification.** The named probe
`TestAstra6CloseBeforeServeReleasesListener` passes — the address rebinds
immediately, and a repeated `Close` is safe. `TestAstra6MonitorLifecycle`
covers cancellation before `Serve` and requires that `Serve` reporting
finished implies the port is free.

## RA6X-053 — Monitoring freshness and last-activity ordering break across wall-clock steps — FIXED

**Changed.** Three consequences of using wall time to measure elapsed time in
a daemon whose job is to step the wall clock.

1. **Snapshot freshness.** `healthOf` subtracted two wall timestamps and
   clamped a negative age to zero, so a backward step made an old snapshot
   look fresh. It now takes the monotonic instant and compares it against
   `Status.PublishedMono` (added in RA6X-044); `monitor.Config` gained a
   `Monotonic` hook, wired to the same clock the engine stamps with.
2. **Process identity.** `started_at` was recomputed as wall-now minus uptime
   and therefore moved whenever the clock stepped, despite describing one
   process instance. It is captured once in `Listen`. The field's doc comment
   states what it means on a host whose clock was wrong at boot: it reports
   the wrong time the daemon saw, and `uptime_seconds` — monotonic — is the
   reliable measure.
3. **Last activity.** `storeLatest` kept the largest `UnixNano` seen, so after
   a backward step every later request carried a smaller timestamp and was
   refused, pinning `last_request`/`last_served` to a pre-step future value
   for ever. `eventTime` orders by an **event sequence number** and stores
   whatever wall time that event carried, so the newest event wins whichever
   way the clock moved, and concurrent listeners contend on the sequence
   rather than on the wall value.

JSON and Prometheus field names and units are unchanged.

**Files.** `internal/monitor/model.go`, `internal/monitor/server.go`,
`internal/server/stats.go`, `internal/server/responder.go`,
`cmd/carillon/main.go`, `internal/monitor/model_test.go`,
`internal/monitor/metrics_test.go`,
`internal/monitor/astra6_review_test.go`,
`internal/server/astra6_review_test.go`.

**Verification.** `TestAstra6FreshnessIsMonotonic` steps the clock an hour
backwards and an hour forwards between publication and serving and requires
the reported age to follow elapsed time in both directions, with the stale
verdict following it. `TestAstra6InstanceIdentityIsStable` steps the clock a
day and requires `started_at` and `uptime_seconds` not to move.
`TestAstra6LastEventSurvivesAClockStep` requires a corrected time to replace a
pre-step future one, and `TestAstra6ConcurrentEventsKeepOne` runs eight
goroutines against one counter.

---

## Wave 9 — diagnostics, durability, and remaining validation

## RA6X-047 — Statistics omit source loss and state transitions without loop updates — FIXED

**Reason for implementing rather than skipping.** This finding is marked
*Needs investigation*, but unlike RA6X-023 and RA6X-057 the decision it names
does not choose between options with opposite consequences. Its fix
specification says to use *"an explicitly identified event stream or
compatible repeated snapshots rather than silently redefining `Updates`"* —
and an additional file changes no existing column's meaning, no existing
consumer, and nothing about what `Updates` means. The only thing being decided
is whether the coverage exists at all, and adding it is reversible in a way
that redefining an established schema is not.

**Changed.** A new `events.tsv` records **transitions**, with the columns
`time, kind, subject, from, to`: synchronization state, system source, PPS
qualification, prefer loss, and each source's selection status and
reachability. `loop.tsv` and `sources.tsv` remain sampled per loop update, so
their documented contract is untouched — but during filter starvation,
holdover entry and expiry, repeated failures or a source shutdown, they may
record nothing at all, which is precisely when an operator needs the history.
Volume is bounded twice: only transitions are written, and one subject is
recorded at most once a second, so a flapping source cannot fill the disk.
The work stays on the recorder's own goroutine, off the engine.

**Files.** `internal/stats/writer.go`, `DESIGN.md`,
`internal/stats/astra6_review_test.go`.

**Verification.** `TestAstra6EventsReconstructAnOutage` drives synchronized →
source loss → holdover → unsynchronized → recovery **with `Updates` never
changing**, requires every transition to appear in `events.tsv`, and requires
`loop.tsv` to still hold exactly its one per-update row — so the existing
contract is demonstrably unchanged. `TestAstra6EventVolumeIsBounded` flaps a
source ten times a second for twenty seconds and requires the rate bound to
hold.

## RA6X-048 — Per-pulse statistics can silently coalesce accepted PPS events — FIXED

**Changed.** Pulse rows were derived from each source's latest `Info` when the
engine published a snapshot, and `Info` retains only the newest pulse — so
several pulses arriving between two publications collapsed into one row and the
earlier ones vanished without the recorder's drop counter moving. Coalescing
was possible even when the recorder queue never filled, because a source
advances independently of engine consumption.

Accepted pulses now travel their own immutable event path. `source.Pulse` is
the record — source name, kernel timestamp, calibrated offset, device sequence
— `PPSConfig.OnPulse` delivers it at the moment of acceptance, and
`Recorder.Pulse` queues it without blocking the refclock goroutine. A pulse
that cannot be queued increments a **separate** counter, logged separately
from snapshot drops, so a gap in `pps.tsv` is always accounted for and zero
loss is never inferred from an empty snapshot-drop counter. Sequence wrap
handling, the existing TSV columns and the cumulative source counters are
unchanged, and the NMEA lag tracker still receives the same edge.

**Files.** `internal/source/source.go`, `internal/refclock/pps.go`,
`internal/stats/writer.go`, `cmd/carillon/main.go`, `DESIGN.md`,
`internal/stats/writer_test.go`, `internal/stats/astra6_review_test.go`.

**Verification.** `TestAstra6EveryAcceptedPulseIsRecorded` accepts 25 pulses
while nothing is draining the recorder — the case that used to coalesce — and
requires 25 rows with each row's own timestamp and sequence, never a mixture.
`TestAstra6PulseLossIsCounted` overfills the queue by exactly 50 and requires
50 counted pulse drops and zero snapshot drops.
`TestAstra6PulseQueueNeverBlocks` offers ten queue-lengths of pulses and
requires the caller never to block.

## RA6X-049 — JSON output failures are reported as successful empty responses — FIXED

**Changed.** All three paths.

*Monitor:* `writeJSON` committed HTTP 200 before encoding and discarded the
encoder's error, so a non-finite value produced 200 with an empty body. It now
marshals first and only then writes the status and the body; a failure becomes
a bounded, stable HTTP 500 with a fixed JSON error object, and the detail goes
to the log rather than to the client.

*Control:* `reply` logged the marshal failure and closed the connection with
nothing sent. It now sends `{"error":"control: internal encoding failure"}` —
the same shape every other failure takes — so a client is told rather than
left to guess.

*CLI:* `carillonctl -json` used a streaming encoder and discarded its result,
so it could exit 0 having printed nothing or half a document. It now
serializes to a buffer, writes it in one call, and returns exit 2 on either an
encoding or a write failure.

The valid-data schema, units and status codes are unchanged, and the upstream
finiteness guards from RA6X-014, RA6X-035 and RA6X-041 mean invalid numeric
data is refused before it reaches here rather than being turned into healthy
zeros.

**Files.** `internal/monitor/server.go`, `internal/control/server.go`,
`cmd/carillonctl/main.go`, `internal/monitor/astra6_review_test.go`.

**Verification.** The named probe
`TestAstra6JSONEncodingFailureIsNotSuccess` passes and additionally requires
the status to be 500. `TestAstra6ValidJSONIsUnchanged` confirms ordinary data
still produces 200, the right content type, and a body that parses.

## RA6X-058 — Drift replacement lacks an explicit power-loss durability contract — FIXED

**Reason for implementing rather than skipping.** This is marked *Needs
investigation*, and the crash testing its verification asks for cannot be done
from here. But the decision itself is one-sided in a way RA6X-023 and
RA6X-057's are not: making the guarantee *stronger* cannot break any
observable behaviour, costs one `fsync` an hour, and choosing **not** to make
it is the option that would need justifying. The stated guarantee is therefore
adopted and documented, and the crash-testing verification is deferred rather
than the whole finding.

**Changed.** The write sequence is now temp → write → fsync → chmod → rename →
**fsync of the containing directory**. `rename(2)` is atomic to a concurrent
reader at any instant, but on the supported filesystems that says nothing
about which directory entry survives power loss, so `writeDrift` could return
success on a calibration that then reverted or disappeared. A directory that
cannot be opened or synced is reported as an error rather than silently
claimed durable — that false promise is what the finding is about. Atomic
reader visibility, the known-good contents on a pre-rename failure, the file
mode and the stable-write gate (RA6X-013) are unchanged, and the work happens
on the persistence worker (RA6X-044), so durability cannot block discipline.
`DESIGN.md` §10.5 states the guarantee.

**Files.** `internal/engine/engine.go`, `DESIGN.md`,
`internal/engine/astra6_review_test.go`.

**Verification.** `TestAstra6DriftReplacementIsDurable` checks the replacement
succeeds and reads back, that nothing is left behind, and that a pre-rename
failure leaves the previous value untouched. **Deferred to the target hosts:**
filesystem fault and crash testing in a disposable environment on the
supported Linux and FreeBSD filesystems, and injected syscall failures around
write, sync, chmod, rename and the directory sync.

## RA6X-043 — Receive-buffer fallback can skip the promised minimum — FIXED

**Changed.** Halving an arbitrary request could step straight past the
documented floor without ever asking for it — 100000 halves to 50000, below
65536 — so a system able to grant the minimum failed startup with an error
saying the minimum had been tried. The loop now clamps to `minRecvBuffer` and
attempts it **exactly once** as the last try, terminates immediately on a
non-capacity error (anything but `ENOBUFS`/`EINVAL` will fail identically at
every size), and reports the sizes actually attempted in both the warning and
the failure. `setReadBuffer` takes a small `bufferSetter` interface so the
kernel can be faked. Current syscall hints, startup failure when even the
minimum is unavailable, Linux's doubled `SO_RCVBUF` reporting and the
configured maximum are unchanged.

**Files.** `internal/server/listener.go`,
`internal/server/astra6_review_test.go`.

**Verification.** `TestAstra6ReceiveBufferFallbackReachesTheMinimum` injects a
kernel that rejects 100000 but accepts 65536 and requires the floor to be
reached and granted; further subtests require the floor to be attempted
exactly once when everything fails, a power-of-two request to still work, a
request equal to the floor to be a single attempt, and a non-capacity error to
stop immediately.

## RA6X-036 — Presence-sensitive refclock validation silently ignores explicit settings — FIXED

**Changed.** Validation tested decoded *values*, so `baud = 0` on a bare PPS
block was indistinguishable from absence and passed, while `baud = 9600` was
rejected — the diagnostic depended on what the operator happened to write.
`Parse` now decodes the document a second time into a generic map and records
which keys each `[[refclock]]` actually contained; validation tests presence.
Both directions are covered: GPS-only keys (`baud`, `pps`, `pps_edge`,
`pps_offset`, `nmea_offset`, `sentences`) on a `type = "pps"` block, and
PPS-only keys (`edge`, `offset`) on a `type = "gps"` block.

`pps = "none"` needed the explicit policy the finding asks for. It is
intentionally supported — an NMEA-only GPS — so `pps_edge` and `pps_offset`
are then inapplicable in exactly the way the GPS keys are on a bare PPS block,
and are rejected with a message that says what to set instead. One rule rather
than two, and an operator who meant to enable PPS is told rather than silently
getting NMEA only.

The rule applies to *parsed* configuration only: a `Config` built in Go cannot
observe presence and its zero values are legitimate defaults, which the field's
doc comment states. Existing key names, valid defaults, correct
`pps_offset`/`nmea_offset` behaviour and every shipped example are unaffected.

**Files.** `internal/config/config.go`, `internal/config/config_test.go`.

**Verification.** The review's own fixture
`TestVerification021RejectsExplicitGPSOnlyZeros` — all six cases — now passes.
`TestRefclockKeysAreRejectedByPresence` tables absent, zero, empty and nonzero
variants for both types, the `pps = "none"` case, a set of valid blocks that
must keep parsing, and a programmatically built `Config` that must not be
affected. Six literal-`Refclock` cases in `TestValidateRules` were replaced by
these TOML-driven ones, because a Go literal is exactly the case the rule does
not apply to; `TestAstra6ShippedExamplesStillParse` continues to parse
`deploy/carillon.toml.example`.
