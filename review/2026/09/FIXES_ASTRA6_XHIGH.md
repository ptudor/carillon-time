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
