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
