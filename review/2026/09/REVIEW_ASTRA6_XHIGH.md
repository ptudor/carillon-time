# Astra6 exhaustive code review

Review date: 2026-09-05. Baseline: `8060697` on `main`. Analysis only; production source, tests, dependencies, and deployment configuration are unchanged.

The review covers every file tracked at the baseline: **136 files, 27,109 lines**, including **104 Go files (38 test files)**, all build/deployment material, the design, and all earlier review evidence. The only repository change is this report. Findings use baseline line numbers or function names. Previously reported defects are included when still present; earlier “fixed” claims were checked against code and reproductions rather than accepted as proof.

There are **59 findings: 25 High, 27 Medium, 7 Low, and no Critical**. Six are explicitly marked **Needs investigation** because receiver behavior, interoperability, durability, or an intended operational policy must be established before prescribing the final behavior. Other findings distinguish direct reproductions from static traces and prior target-host evidence. Severity reflects plausible correctness, availability, or data-integrity impact, not a claim that every deployment reaches the path.

### Coverage and method

Every baseline file was read, including build-tagged implementations and test/reproduction assets. Data flows traced included network/serial/kernel timestamps → source windows → selection → loop actions → clock syscalls; source cancellation/restart and step generations; status publication → wire/control/HTTP/metrics/TSV; and config → constructor → deployment privileges/files/devices. No source, test, dependency, deployment file, hardware state, service, or host clock was modified.

| Area | Files | Lines | Review coverage |
|---|---:|---:|---|
| Root files | 6 | 2,011 | .gitignore, CLAUDE.md, DESIGN.md, Makefile, go.mod, go.sum |
| cmd/carillon | 2 | 853 | Startup, dependency construction, signals, service lifecycle, query mode, tests |
| cmd/carillonctl | 2 | 335 | Argument/error/output behavior and tests |
| internal/buildinfo | 1 | 38 | Build identity and clock-era lower bound |
| internal/clock | 13 | 1,062 | Fake/read-only/platform clocks, frequency/status/step ABI, precision, all tests |
| internal/config | 5 | 1,796 | TOML/defaults, semantic/file/device/platform checks, tests |
| internal/control | 4 | 989 | Socket lifecycle, framing, cancellation, DTOs, tests |
| internal/discipline | 9 | 3,150 | Filter, selection/cluster/combination, loop/state machine, simulations/tests |
| internal/engine | 2 | 1,494 | Ownership, queues, epochs, actions, drift, publication, tests |
| internal/leap | 2 | 272 | Parsing, authority, expiry/boundary behavior, tests |
| internal/monitor | 6 | 956 | HTTP/ACLs, health/status model, metrics, lifecycle, tests |
| internal/ntp, including auth | 6 | 1,211 | Packet/time encoding, CMAC/key loading, parser/auth/fuzz tests |
| internal/pps | 10 | 606 | Linux/FreeBSD devices and ABI, lifecycle, build tags, tests |
| internal/refclock | 7 | 2,049 | PPS qualification/recovery, NMEA parsing/windows/validity, simulations/tests |
| internal/serial | 6 | 397 | Platform termios/line discipline, read/poll/error handling, tests |
| internal/server | 18 | 2,282 | Decode/ACL/auth/limiting/replies, ancillary data, listeners, counters, tests |
| internal/sockts | 5 | 209 | Linux/FreeBSD timestamp enable/parse, fallback, tests |
| internal/source | 4 | 1,746 | DNS, authenticated exchanges, filtering, polls/KoD, source contract, tests |
| internal/stats | 2 | 651 | Queues, row semantics, retention, rotation/error handling, tests |
| deploy | 8 | 1,482 | Example config, service definitions, Apache, OpenWrt, acceptance/runbooks |
| Existing review material | 18 | 3,520 | Four Markdown reports and fourteen reproduction/verification assets |
| **Total baseline** | **136** | **27,109** | **All tracked files; this report excluded** |

Ignored generated executables were not treated as source files. No vendored dependency source is present. Dependencies were checked through manifests and govulncheck; this is not an exhaustive manual review of third-party implementations.

### Verification performed

The development host is darwin/arm64 with Go 1.27.0; go.mod declares Go 1.25.0. Commands used dedicated temporary caches/build directories. Socket-dependent probes initially hit sandbox bind restrictions and were rerun with the necessary execution permission; those environmental failures are not counted as code findings.

| Check | Result and limits |
|---|---|
| Baseline `CGO_ENABLED=1 go test -race ./...` and `go vet ./...` | Pass. Nineteen packages considered, seventeen with native tests; buildinfo and serial have no tests for this host's active files. Cached successes were retained where applicable. No detected Go data race; logical ordering defects still reproduce. |
| `make test GO=/opt/local/bin/go` on this host | Pass after permitting temporary local sockets. Deployment-platform cgo/race incompatibility is separately reproduced in RA6X-055. |
| Pure-Go builds of both commands | Pass for linux/amd64, linux/arm64, freebsd/amd64, freebsd/arm64; eight temporary binaries. Cross-building does not verify runtime syscall ABI or devices. |
| Existing `verification_fable5_xhigh/run_probes.py -race` | Fourteen top-level tests fail and four pass. Failures map to RA6X-005/006/007/008/016/017/024/027/030/036/050/051. Passing blocked-write, wrapped-EOF, existing holdover-expiry, and alternate-prefer probes were retained as positive evidence. |
| Existing takeover runner, `baseline` and `distance` | Both reproduce delayed-feedback and drift-persistence failures. Distance ranking improves several cases but still fails a symmetric RTT-growth case; it is not a complete fix. |
| Existing `probe_rfc_dispersion.py` | Diagnostic overlay fails the falseticker test with two steps and the source-removal test; unknown-frequency convergence passes. This supports designing priming/admission together rather than applying the old local change. |
| Existing `probe_signals.py` | Pass with clock.Fake: two SIGHUPs preserve control service and log one warning; SIGTERM exits 0 within six seconds. No real clock or privileged NTP listener used. |
| New embedded Astra6 fixtures | Eighteen top-level counterexamples validated through the embedded overlay runner; the subsequently added queued-time regression was also run and fails as described in RA6X-059. Nineteen total top-level tests are embedded, including six Infinity subcases. Expected failures demonstrate baseline behavior; no production patches applied. |
| Safe `go list -test -tags hwtest` with cgo | Package loading rejects the Linux PPS and FreeBSD PPS/timex C-import test files, as in RA6X-054. No hardware test executed. |
| govulncheck v1.6.0, rebuilt with Go 1.27 | Linux/amd64 and FreeBSD/amd64 source scans both report “No vulnerabilities found” against the fetched Go vulnerability database. The installed Go-1.26-built scanner initially could not parse Go 1.27 and was rebuilt only in /private/tmp. A clean database scan does not rule out the project-specific protocol/lifecycle findings here. |
| Report integrity | Sequential IDs, required fields, all probe fixtures, coverage arithmetic, severity totals, and fix-order coverage checked; git diff limited to this report. |

Hardware PPS/GPS behavior, native Linux/FreeBSD serial execution, FreeBSD broadcast packet captures, filesystem crash durability, and the module's exact minimum Go version were not executed in this session. When a finding cites prior target-host results, that provenance is explicit. Static checks and cross-builds do not replace those remaining verifications.

## RA6X-001 — Delayed filter observations destabilize the discipline loop

**Severity:** High

**Location:** `internal/discipline/filter.go:73–147`; `internal/source/ntp.go:449–488,533–556`; `internal/discipline/system.go:337–361`; `internal/discipline/loop.go:201–262`.

**Problem:** Advancing a sample timestamp does not make the observation current. The eight-stage minimum-delay filter can release successive observations seven polls old. System passes the historical offset to Loop as present-time feedback, replacing residual phase and integrating frequency using delivery time. Corrections applied since observation are not accounted for. A correct initial frequency and an unbiased reference can therefore produce large clock and frequency errors.

**Evidence:** `System.reselect` checks only `sel.System.At <= s.lastUsedAt`, then calls `loop.Update(sel.Offset, sel.System.Poll, now, ...)`. The existing `takeover-repro/discipline_test.go.txt` simulates the actual Filter → System → Loop chain with symmetric changing RTT and documents 448-second-old observations at poll 6, 358.227 ppm peak frequency error, and 167.077 ms final-hour RMS. `TestLoopConvergesWhateverTheUpdateSpacing` supplies current offsets and does not cover delayed feedback. Switching the system source also resets `lastUsedAt` to zero, allowing an already used source observation to be applied again on reselection.

**Fix specification:** Define observation-time semantics across filter, combination, and loop. Either reject observations too old for the selected gains or propagate them to the current clock using recorded applied corrections and an explicit uncertainty model. Track consumption per source rather than resetting a single timestamp on source switches. Evaluate gain stability with actual filtered observation ages, variable polling, source changes, missing updates, and PPS/NMEA windows. A ranking-only change is insufficient: the existing distance-ranking experiment still fails with steeper symmetric RTT growth. Preserve offset sign, frequency/slew clamps, step policy, source names, and public status schema; document the algorithm decision before implementation.

**Verification:** Run `python3 review/2026/09/takeover-repro/reproduce.py baseline`; make its delayed-feedback assertions pass. Add regressions for switching away from and back to an unchanged observation, adaptive polls, startup frequency measurement, and delayed refclock window estimates. Assert actual clock error and applied frequency, not only update count.

## RA6X-002 — The filter can withhold useful corrections for many polls

**Severity:** High

**Location:** `internal/discipline/filter.go:85–93,143–147`; `internal/source/ntp.go:477–486`; `internal/discipline/system.go:342–357`; `DESIGN.md:561–592`.

**Problem:** A low-delay observation suppresses later filter outputs until eviction or demotion. The loop receives no feedback while its last pending correction decays, even though valid replies continue. The settling repair permits service during this interval but does not resolve the blind discipline interval.

**Evidence:** `Filter.Add` returns `updated=false` when the winning observation was already reported; `NTP.hit` returns nil; System does not run the loop. Existing starvation/takeover fixtures reproduce gaps of 512 s at fixed poll 6 and 3072 s at poll 10. The design's assertion that an observation older than the Allan intercept must also lose on raw delay is incorrect: line 90 explicitly changes its rank to `MaxDistance + dispersion`. Demotion happens on arrivals, and the eight-arrival ring also bounds retention. The design's 11-minute calculation for a 10 ms RTT advantage omits the half-delay factor.

**Fix specification:** Resolve together with RA6X-001: deliver sufficiently current discipline evidence at a cadence compatible with loop gains, while avoiding repeated integration of one unmodified estimate. Preserve the filter's outlier protection and bounded memory. Correct the mechanism and quantitative examples in DESIGN and the corresponding acceptance explanation; retain historical measurements but distinguish observations from inferred causes.

**Verification:** Exercise the fixed-poll and jittered/adaptive-poll gap fixtures in `takeover-repro/discipline_test.go.txt`, plus the existing starvation regression. Bound both maximum feedback age and actual oscillator/phase error through a full client/server chain.

## RA6X-003 — Time-based source expiry never runs on engine ticks

**Severity:** High

**Location:** `internal/discipline/system.go:293–335,462–469`; `internal/discipline/select.go:164–184`; `internal/engine/engine.go:368–374`.

**Problem:** Selection ages uncertainty and rejects over-distance sources only on a measurement or explicit lifecycle event. `System.Tick` performs phase slewing and an already-entered holdover timeout, without reselecting. If all producers become silent, a synchronized source remains selected indefinitely; the holdover timer never starts. With long configured polls, a source can also remain eligible past its distance limit between polls.

**Evidence:** `RootDistance(now)` adds `Phi * age`; `candidate()` rejects distance ≥1.5 s. Neither is called by Tick. Repeated Tick calls alone leave `StateSynced`, `SystemSource`, and per-source selection distance unchanged, even after the old observation has aged beyond the limit. Refclock reopen loops provide a production route to indefinite silence (RA6X-004).

**Fix specification:** Perform time-driven source eligibility/selection checks on ticks without re-integrating already consumed measurements. Define source-specific freshness/missing-slot deadlines and begin holdover at the actual loss boundary. Publish aged distances consistently. Preserve configured holdover duration, legitimate sparse NTP polls, preference rules, and immutable snapshots; coordinate observation reuse with RA6X-001.

**Verification:** Synchronize a fake System, stop all measurements, advance only Tick past freshness/root-distance expiry and holdover expiry, and require HOLDOVER then UNSYNCED. Repeat with another surviving source and with a long legal poll interval; assert wire and kernel synchronization agree.

## RA6X-004 — Device reconnect loops retain reachable, selectable estimates

**Severity:** High

**Location:** `internal/refclock/pps.go:257–265,280–313`; `internal/refclock/nmea.go:207–213,217–249`.

**Problem:** A hard device error enters a private reconnect loop without emitting reach zero or invalidating the previous estimate. During repeated open failures, the engine receives no loss information. A disconnected PPS/GPS can remain system source; an old NMEA estimate can continue numbering a live PPS. This is separate from ordinary fetch/read timeouts, which do emit measurements.

**Evidence:** The error branches update only `Info.LastError` and call `reopen(ctx)`. Reach, windows, and previous-sample state are cleared only after a successful open; failed retries only update LastError. Neither reconnect loop sends an engine measurement. Combined with RA6X-003, a sole disconnected refclock can remain nominally synchronized indefinitely.

**Fix specification:** On hard device loss, immediately revoke the estimate and report reach zero before waiting to reopen. Keep reporting appropriate source health while unavailable. Reset temporal/framing/sequence state before accepting the new device's first sample, and require ordinary priming/qualification again. Retain the source identity, cumulative counters, context cancellation, bounded backoff, and distinction between a missed pulse and a vanished device.

**Verification:** Use fake PPS/serial readers that first lock, then fail, and openers that fail indefinitely. Confirm prompt fallback/holdover and PPS unqualification while LastError remains visible. Restore the opener and verify re-priming without admitting pre-disconnect measurements.

## RA6X-005 — Re-priming PPS does not invalidate the selector's old lock

**Severity:** High

**Location:** `internal/refclock/pps.go:363–369,485–515,541–553`; `internal/discipline/select.go:129–153`.

**Problem:** The new consecutive-rejection and sequence-restart recovery paths empty the PPS window and clear stability but return an ordinary invalid measurement. Such a measurement intentionally retains the previous estimate in SourceState. Reach is still nonzero, so the unlocked PPS stays selected.

**Evidence:** `reject` calls `resetWindow()` after four rejections without setting `m.Invalidate`; `sequenceRestart` has the same omission. `SourceState.apply` clears validity only for `Invalidate=true`. Existing `TestVerification001ReprimeInvalidatesSelection` reproduces the PPS remaining system source at octal reach 360 after four spikes and 376 after a counter restart, with an empty unstable window. Prior IDs: RF5X-001 and RF5X-026.

**Fix specification:** Communicate invalidation on every local reset that revokes the estimate, including rejection runs, sequence restart, hard loss, and future recovery paths. Keep clock-step generation resets distinct, and preserve current gap counters, reach shifting, spike-fit behavior, polling, lock hysteresis, and accepted-pulse callbacks. An old estimate must not revive before the new window has sufficient valid samples.

**Verification:** Run the existing verification fixture for both reset branches and add a full re-priming sequence; inspect System selection and wire stratum/LI as well as `Info.Refclock.Stable`.

## RA6X-006 — Generation stamping does not bracket clock-step execution

**Severity:** High

**Location:** `internal/engine/engine.go:529–542`; `internal/source/ntp.go:316–343`; `internal/refclock/nmea.go:189–193,260–267,336–345`; `internal/refclock/pps.go:228–246`.

**Problem:** The engine increments the generation before the syscall and leaves that same generation current afterward. A source starting after the increment but before the actual clock change labels a pre-step observation with the post-step generation. NMEA separately reads arrival time before it captures the generation. These windows defeat the intended stale-sample protection and can cause incorrect subsequent corrections.

**Evidence:** `gen.Add(1)` precedes `clk.Step`, with no completion transition. `TestVerification002GenerationCoversTheStep` captures a +2 s pre-step sample during Step under generation 2; `Engine.stale` accepts generation 2 afterward. `TestVerification002NMEAGenerationMatchesArrival` advances generation between the arrival-clock read and `$` processing and admits the old timestamp into the new window. Prior ID: RF5X-002.

**Fix specification:** Bracket the entire discontinuity with an explicit in-progress epoch or equivalent synchronization, and permit samples only within one completed stable epoch. Capture/check NMEA generation around the read that determines arrival time, not later byte framing. Ensure Reset and local window insertion cannot let old samples contaminate a new-generation aggregate. Handle step failures and queued samples; do not introduce a global blocking lock across network/device waits. Preserve the public Source interface where possible, zero-generation test compatibility, and the second-step evidence policy.

**Verification:** Make both named verification probes pass, then force generation changes before, during, and after acquisition, filter insertion, and queue delivery for all three source types. Verify a step failure does not leave acquisition permanently in progress.

## RA6X-007 — Leap transitions are detected after queued corrections execute

**Severity:** High

**Location:** `internal/engine/engine.go:360–370,522–564`.

**Problem:** Run evaluates `sys.Update(m)` before `handle` checks the leap boundary, and handle applies actions before that check. A queued observation spanning the transition can therefore step or change frequency before generation and source filters are reset. Resetting afterward cannot undo an actuator operation.

**Evidence:** `e.handle(e.sys.Update(m), m.Now)` invokes Update first; `handle` executes `res.Actions` at lines 523–543 and checks `LeapTable.Crossed` at line 551. `TestVerification002LeapCheckedBeforeQueuedMeasurement` produces an actual one-second fake-clock step from a queued pre-leap observation before the reset. Prior ID: RF5X-002.

**Fix specification:** Detect and process discontinuities before accepting a queued measurement or evaluating correction actions. Use the stable-generation protocol from RA6X-006, discard boundary-spanning evidence, and retain the post-leap Resync behavior that avoids unnecessary settling after a previously synchronized run. Cover both leapfile and survivor-authoritative leap paths; do not invent extra steps for a leap already performed by the kernel.

**Verification:** Make the named engine probe pass with zero unintended Step/SetFrequency actions. Repeat with a measurement ready on the same select iteration as the ticker, both leap directions, and a delayed engine wakeup.

## RA6X-008 — Phase debit uses the next frequency word for the previous interval

**Severity:** High

**Location:** `internal/discipline/loop.go:258–267,292–338`.

**Problem:** Tick claims to account for the transient that actually ran during elapsed time, but computes `actual` from the new Pending and new base. It then debits that new correction before issuing it. Even successive ordinary ticks disagree with the applied-word integral; intervening loop updates, changed bases, and delayed ticks make the discrepancy larger. Initial ticks also charge a nominal second before the first transient has run. Intervals beyond maxTickInterval are reduced to one second even if the old word actually remained applied throughout the stall.

**Evidence:** `total := clampFreq(l.Freq + adj*1e6)` and `actual := (total-l.Freq)*1e-6` precede `Pending -= actual*dt`; the previous `l.applied` is not used. `TestVerification011ChargesTheAppliedWord` expects 0.009902343750 s remaining after 39.0625 ppm ran for 1.5 s, but receives 0.009902572632 s. The ordinary test derives expected debit from the same new Pending expression and misses the defect. Prior ID: RF5X-011.

**Fix specification:** Track the successfully applied frequency and the accounting epoch, charge the actual held correction over the actual interval, then calculate the next word. Explicitly reconcile a new observation's Pending replacement with corrections already included in its timestamp; avoid double debit across Update and Tick. Define first-tick, zero/backward time, step, actuator-failure, clamp, and long-stall semantics. Preserve sign conventions, ±500 ppm total bound, configured phase-slew ceiling, and the absence of a base-only pulse between updates.

**Verification:** Make the issued-word probe pass and add an independent fake-actuator integral oracle for updates between irregular ticks, base changes, saturation, first tick, and step. Run full filtered closed-loop simulations after correcting the accounting.

## RA6X-009 — Losing a source during settling promotes untrusted time to holdover

**Severity:** High

**Location:** `internal/discipline/system.go`, `reselect` (327–335), `Status`; `internal/engine/engine.go`, `syncKernel`; `cmd/carillon/main.go`, NTP status callback.

**Problem:** SETTLING and SYNCED both enter HOLDOVER when selection loses its last source. HOLDOVER is treated as synchronized by the server and kernel even if the daemon never completed settling. Losing evidence can therefore increase the trust placed in the clock, including immediately after a step.

**Evidence:** The no-system branch is `case StateSettling, StateSynced: ... StateHoldover`. `TestAstra6SettlingLossDoesNotSynchronize` feeds one small valid offset with `SettleUpdates=3`, then reach zero: status becomes `holdover`, LI `none`, stratum 3, although its previous status was SETTLING/LI=3. The branch stores no prior successful-synchronization prerequisite.

**Fix specification:** Enter serviceable holdover only from a state with established, still-applicable synchronization. Loss during initial/post-step settling must remain unsynchronized and retain an appropriate reason. Keep successful SYNCED→HOLDOVER behavior and the special previously-synced leap Resync path. Preserve state names, wire field meanings, and holdover expiry; reset all settling evidence consistently when reacquiring.

**Verification:** Make the named probe pass. Repeat after an initial step, a second startup step, source disagreement, and explicit invalidation. Assert kernel Synced, wire LI/stratum, health, and waitsync agree; already-synced loss must still hold over normally.

## RA6X-010 — Failed polls count as successful settling evidence

**Severity:** High

**Location:** `internal/discipline/system.go:293–302,342–355,428–435`; `internal/discipline/measurement.go`; `internal/source/ntp.go`, `emit`.

**Problem:** Every Measurement from the selected name increments `sinceStep`, including timeouts, bad authentication, rejected packets, and invalidation notifications. The recent settling repair thus allows SYNCED without the required successful post-step/initial observations. A transport heartbeat and a valid observation have been conflated.

**Evidence:** `if m.Source == s.sysName { s.sinceStep++ }` has no quality/acquisition condition. The false-Valid path retains the previous estimate. `TestAstra6MissIsNotSettlingEvidence` feeds one valid sample and one timeout with reach 254; the timeout alone completes settling. Merely testing `Valid` is insufficient because a successful reply whose filter winner is unchanged is also emitted with `Valid=false`.

**Fix specification:** Represent successful, current-epoch acquisition separately from a newly consumable filter output and from an error heartbeat. Count only accepted acquisitions toward settling, including a good reply with unchanged winner; exclude misses, bad packets/MACs, reset notices, and stale epochs. Preserve the fix that makes settling independent of minimum-delay winner turnover and the configurable count.

**Verification:** Cover successful unchanged-winner replies, timeouts, bad MACs, unusable stratum, PPS spikes, reset notifications, and post-step epochs. Make the named probe pass while keeping `TestSystemLeavesSettlingWithoutAFreshLoopUpdate` meaningful under the new measurement semantics.

## RA6X-011 — Panic-at-startup bypasses the never-step setting

**Severity:** High

**Location:** `internal/discipline/loop.go`, `Update` (182–191), `stepAllowed`; `DESIGN.md` §6.6; `deploy/carillon.toml.example`, `[step]`.

**Problem:** `panic_at_startup=true` directly issues the first step above the panic threshold even with `limit=0`. The explicit never-step configuration can therefore move the host clock by thousands of seconds. The existing tests exercise the two options independently and miss their interaction.

**Evidence:** `TestAstra6NeverStepIncludesPanicStartup` configures both options and supplies +2000 s; `Update.Stepped` is true. The early panic exception invokes `step` before the ordinary step-budget check.

**Fix specification:** Apply the step policy consistently to the panic exception. `limit=0` must never generate ActionStep; exceeding panic with stepping forbidden must be refused rather than silently converted into an enormous slew. Preserve the one-time large-startup correction for configurations that permit stepping, normal startup limits, and backward-step symmetry. Document the precedence explicitly.

**Verification:** Test the cross product of limit 0/positive/-1, panic_at_startup false/true, first/later update, and positive/negative offsets around both thresholds. Make the named probe pass.

## RA6X-012 — Panic refusal is logged but does not stop the daemon as specified

**Severity:** Medium

**Location:** `internal/discipline/system.go`, `reselect` panic branch; `internal/engine/engine.go`, `handle`, `logEvent`, `Run`; `DESIGN.md:790–794`.

**Problem:** The documented fatal panic gate is implemented as an ordinary event and an UNSYNCED state change. Run continues, allowing future corrections and ticks; an earlier pending slew is not necessarily canceled. Operators relying on a nonzero exit and service-manager backoff do not get it.

**Evidence:** System returns `EventPanicRefused` without an error/action; engine's event loop logs it and returns normally. The design explicitly requires logging the offset and exiting 1. `TestSimPanicRefused` checks state in a continuing simulation and never exercises daemon termination.

**Fix specification:** Propagate a typed fatal refusal through the engine to main, cancel sources/auxiliaries, restore the allowed base frequency, and exit nonzero. Preserve PANC diagnostic semantics up to shutdown, the deliberate startup exception, and no-step-on-exit. Ensure every path invoking discipline results propagates the fatal result, including source-stop handling (RA6X-017).

**Verification:** Use the real main/engine lifecycle with a fake actuator, supply an out-of-policy offset, and assert prompt nonzero exit, one refusal log, no further phase corrections, and correct frequency restoration. Test startup exception and a refusal after prior synchronization separately.

## RA6X-013 — A frozen transient frequency is accepted as stable drift

**Severity:** High

**Location:** `internal/engine/engine.go`, `noteFrequency`, `frequencySettled`, `maybeWriteDrift`; `review/2026/09/takeover-repro/engine_test.go.txt`.

**Problem:** The persistence gate measures the spread of repeated base-frequency readings, without requiring fresh reference evidence. A bad transient left unchanged during filter starvation or source silence appears perfectly stable after 900 seconds and overwrites the known-good drift file. Subsequent restarts inherit the wrong correction.

**Evidence:** `TestTakeoverFrozenFrequencyMustNotOverwriteDrift` reproduces a previously stored 6.125 ppm being replaced by 30.539062 ppm after feedback stops. Tick-time readings populate the stability history even with no loop updates. This defeats the gate's stated purpose, despite `TestEngineDoesNotPersistAMovingFrequency` passing for continuously changing values.

**Fix specification:** Require independent fresh accepted discipline evidence spanning the stability interval, bounded sample ages, and a trustworthy synchronized state. Flat repeated reads alone must never establish stability. Reset/partition evidence on source/epoch changes and rejected corrections. Preserve the last known-good file on insufficient evidence, existing scalar drift format, atomic replacement, and configurable spread/window. Coordinate with RA6X-001/002/003/008 before choosing thresholds.

**Verification:** Make the takeover drift probe pass; cover zero feedback, stale-but-reachable sources, holdover, source changes, and an upstream still slewing. Also prove independently supported stable observations eventually permit hourly and shutdown writes.

## RA6X-014 — NaN in the drift file reaches the clock-control state

**Severity:** High

**Location:** `internal/engine/engine.go:233–245`, `New`; `internal/discipline/loop.go`, `NewLoop`/`clampFreq`; `internal/clock/sysclock_common.go`, `freqWord`.

**Problem:** `ParseFloat` accepts NaN; range comparisons do not reject it. The engine then marks the frequency known and initializes the loop with NaN. Clamps based on floating-point min/max retain NaN, and converting it to a kernel integer is not a valid bounded frequency conversion. This can cause an actuator failure or an extreme correction and also break JSON monitoring.

**Evidence:** `TestAstra6DriftRejectsNaN` writes `NaN\n` to a temporary drift file and `readDrift` returns NaN with no error. The read check only tests `< -500 || > 500`; ordinary garbage/out-of-range tests omit non-finite values.

**Fix specification:** Reject every non-finite persisted frequency before trusting it; route it through the existing invalid-drift fallback/diagnostic policy. Add defensive finite checks at actuator conversion boundaries so invalid internal state cannot reach a syscall. Preserve valid ±500 ppm values, scalar file format, and legitimate kernel-frequency fallback behavior. Do not silently treat NaN as a measured zero.

**Verification:** Test NaN spellings accepted by strconv, ±Inf, out-of-range values, whitespace, boundary values, and normal round trips. Assert a fake actuator never receives a non-finite/out-of-range word and status remains encodable.

## RA6X-015 — Startup temporary cleanup can delete the configured drift file

**Severity:** High

**Location:** `internal/engine/engine.go`, `sweepDriftTemps` (253–276), `New`, `writeDrift`.

**Problem:** Cleanup assumes every old `.drift-*` entry beside the configured drift file is disposable. The filename is user-configurable, so the real calibration file can match that pattern. Unrelated files from another instance in the same directory are also eligible for deletion.

**Evidence:** `TestVerification019SweepPreservesConfiguredDrift` sets the actual path to `.drift-calibrated`, ages it beyond one minute, and observes its deletion by `New`. The existing sweep test only uses the basename `drift`.

**Fix specification:** Always exclude the configured destination by canonical identity and limit cleanup to regular temporary files provably owned by this writer's namespace. Avoid following symlinks or deleting other instances' active/recoverable files. Preserve atomic drift writes, existing valid configured filenames, and the last known-good contents; use a destination-specific temporary naming scheme if necessary.

**Verification:** Make the named probe pass. Plant matching/nonmatching files, symlinks, directories, the configured destination itself, and another writer's temporaries; only safe abandoned temporaries may disappear.

## RA6X-016 — Shutdown before the first tick leaves the wrong base frequency applied

**Severity:** Medium

**Location:** `internal/engine/engine.go`, `Run` initial SetFrequency and `restoreBaseFrequency`; `internal/discipline/loop.go`, `Applied`.

**Problem:** The initial kernel frequency write bypasses Loop's applied-state bookkeeping. If multiple measurements change the base before the first tick and shutdown occurs then, `Applied()` reports no write and restoration returns early. The kernel retains the startup value rather than the final base estimate reported by status.

**Evidence:** `TestVerification004RestoresBaseBeforeFirstTick` reproduces actual fake-kernel frequency 12.5 ppm versus final base 17.3828125 ppm. `restoreBaseFrequency` returns when `!ok`; only Tick sets Loop.haveApply. Prior RF5X-004 tests allow a tick first.

**Fix specification:** Track successful actuator writes in the engine or initialize the loop's applied state after the startup write; use that authoritative state during restoration. Coordinate with RA6X-008. Preserve suppression of a redundant retry after SetFrequency itself was refused, no stepping on exit, and the independent drift persistence gate.

**Verification:** Make the named probe pass with the ticker deliberately delayed. Cover zero updates, several pre-tick updates, initial write failure, and normal mid-slew shutdown; compare the actual last actuator write with the intended final base.

## RA6X-017 — Source-stop events can be overtaken by queued measurements and swallow fatal errors

**Severity:** High

**Location:** `internal/engine/engine.go`, `runSource`, `sourceStopped` (487–507), `Run` request/measurement cases.

**Problem:** Measurements and lifecycle events use different channels with no per-run identity/order. After a source's stop event marks it unreachable, a previously queued valid measurement can revive it. Restart also reuses the same object without an engine-owned sample epoch. Additionally, sourceStopped logs and discards errors returned by handle, unlike normal Run paths; a selection change there can issue an actuator action whose failure is meant to be fatal.

**Evidence:** `TestVerification032QueuedMeasurementCannotReviveStoppedSource` consumes the stop first, then an old good sample and finds the dead source selected again. The global clock generation cannot distinguish runs without a clock step. `sourceStopped` uses `if err := e.handle(...); err != nil { e.log.Error(...) }` without returning the error.

**Fix specification:** Serialize lifecycle and measurement delivery or stamp a source-run identity and reject measurements after that run stops. Reset acquisition state before restart and require fresh evidence to clear health errors. Propagate every fatal result from source-stop reselection to Run's common shutdown path. Preserve configured source names, cumulative telemetry, retry backoff, and retention of stopped sources in status.

**Verification:** Make the queued-measurement probe pass under both channel orderings, repeated restarts, and source-name reuse. Make a fallback selection during sourceStopped trigger a fake actuator failure and assert Run terminates correctly. Run race tests, but retain deterministic ordering tests because this is a logical race.

## RA6X-018 — Canceling an NMEA reconnect panics the daemon

**Severity:** High

**Location:** `internal/refclock/nmea.go:179–215,217–249` (`Run`, `reopen`).

**Problem:** `reopen` closes/clears the reader and returns nil when the context is canceled. Run interprets nil as successful reopening, loops, and dereferences the nil reader before checking cancellation. The panic in a source goroutine terminates the whole daemon and bypasses normal base-frequency restoration and stats flushing.

**Evidence:** `TestAstra6NMEACancelDuringReconnect` uses a failing fake reader whose Close cancels the context and an unavailable opener. Run panics with `invalid memory address or nil pointer dereference`. PPS's corresponding Run explicitly checks context after reopen; NMEA does not.

**Fix specification:** Make cancellation distinguishable from successful reopen, or check it before every dereference and after reconnect returns. Ensure no path returns success with a nil reader and no path calls Run again on a closed reader without reopening. Preserve cancellation as a clean source stop, exponential retry behavior, and exactly-once resource cleanup.

**Verification:** Make the named probe pass with no panic. Cancel before Run, during ReadTimeout, during each backoff, and immediately after successful reopen; ensure engine shutdown restores frequency and terminates promptly.

## RA6X-019 — NMEA reacquisition reuses a stale pre-outage median

**Severity:** High

**Location:** `internal/refclock/nmea.go`, `tick`, `noteArrival`, `acceptLine`, `resetWindow`.

**Problem:** Ordinary silence can empty reach without clearing the NMEA offset window. When data returns, one fresh sentence immediately qualifies a mostly historical window and stamps its median as current. The host may have slewed or drifted during the outage, making that estimate wrong and falsely precise.

**Evidence:** `TestAstra6NMEASilenceRequiresFreshWindow` fills 16 zero-offset samples, advances timeout slots to reach zero, and feeds one sample at +100 ms. It emits `Valid=true`, offset 0, with the stale zero-MAD majority. Unlike PPS timeout, NMEA tick never calls resetWindow.

**Fix specification:** Invalidate and clear observation/lag windows when freshness is lost, including sparse arrivals that skip enough slots without an intervening tick. Require the normal minimum of fresh accepted sentences before requalification. Publish stable/locked status consistently. Preserve sentence selection, offset calibration sign, source identity, and cumulative counters.

**Verification:** Make the named probe pass. Test timeout-driven loss, a long gap followed directly by data, ordinary short packet loss, and recovery while the engine slews using another source. Assert no pre-outage sample contributes after re-priming.

## RA6X-020 — One future GPS date can suppress all subsequent correct time

**Severity:** High

**Location:** `internal/refclock/nmea.go:321–347`, `resetWindow`, `reopen`; `internal/refclock/nmea_sentence.go`, ZDA/RMC calendar parsing.

**Problem:** The only date plausibility guard rejects dates older than the build. A checksum-valid future date is remembered in lastStamp before the engine accepts or refuses the correction. All subsequent earlier/correct timestamps are silently dropped; window resets and successful reconnects retain lastStamp. Recovery can require restarting the daemon or waiting decades.

**Evidence:** `TestAstra6NMEARecoversAfterFutureDate` accepts ZDA year 2099, resets the window, then supplies correct year 2026. The good sentence is rejected because lastStamp remains `2099-08-23`. Even a panic-refused correction has already poisoned the source's deduplication state.

**Fix specification:** Define acquisition-era plausibility and recovery rules that work on hosts with invalid RTCs. Do not permanently commit an implausible forward jump as the anti-replay watermark without corroboration. Reset/re-establish timestamp ordering when device/clock epochs change, with bounded repeated-evidence recovery from bad dates. Preserve same-second duplicate suppression and GPS-rollover rejection; do not permit arbitrary old replays as a shortcut.

**Verification:** Make the named probe recover; test future-year glitch followed by correct data, device reboot/replacement, leap boundary, repeated seconds, a legitimate initial RTC correction, and a rejected panic correction. Verify health explains rejected chronology rather than merely timing out.

## RA6X-021 — ZDA can discipline time while the receiver reports an invalid fix

**Severity:** High — **Needs investigation: receiver-specific time-validity policy**

**Location:** `internal/refclock/nmea_sentence.go`, `parseZDA`; `internal/refclock/nmea.go:296–335,367–385`; `DESIGN.md`, GPS invalid-fix behavior.

**Problem:** Every structurally valid ZDA is treated as valid time. A recent RMC V or GGA quality 0 changes status to FixValid=false but does not prevent ZDA from forming valid stratum-1 measurements. ZDA preference can then suppress RMC. The code demonstrably admits contradictory data; whether a given receiver's ZDA remains trustworthy without navigation validity needs hardware/protocol confirmation.

**Evidence:** `TestAstra6ZDAHonorsKnownInvalidTime` feeds RMC V then four ZDAs and obtains a valid measurement while `Info.Refclock.FixValid=false`. No time-validity freshness or precedence rule connects those fields to ZDA admission.

**Fix specification:** Establish and document a time-validity rule for supported receivers, distinguishing valid UTC without a position solution from an invalid/unknown UTC solution. Track status age and use authoritative receiver validity for ZDA; fail closed on explicit invalid UTC. Preserve intentionally supported NMEA-only/time-only operation where its UTC validity can be established. Do not blindly require a 3D position fix or label all ZDA invalid.

**Verification:** Capture supported receivers during cold start, antenna loss, reacquisition, and valid timing-only operation; confirm their RMC/GGA/ZDA semantics. Turn the probe into policy-specific tests for fresh invalid status, stale status, no status, and valid UTC without position.

## RA6X-022 — Leap warnings are coupled to loop updates and lack a fileless boundary reset

**Severity:** High

**Location:** `internal/discipline/system.go:342–418`; `internal/engine/engine.go`, `handle` leap branch and `publish`; `internal/source/ntp.go`, `hit`/`emit`.

**Problem:** Survivor leap majority is recomputed only after a nonignored loop update from the selected source. Warning changes in other survivors can be ignored while its filter winner stays unchanged. Separately, the only engine boundary reset is conditional on a LeapTable: the supported upstream-authoritative path does not reset samples/generation after the kernel's leap and may retain the warning until a later update.

**Evidence:** `TestAstra6LeapUpdatesWithoutSystemFeedback` makes two of three survivors announce insertion while the preferred system observation is unchanged; stored LI stays none. `s.leap = majorityLeap(sel.Survivors)` is below the early returns. Engine tests all synthesize a leapfile; no fileless transition test exists.

**Fix specification:** Publish accepted protocol metadata independently from new phase-feedback consumption and recompute leap consensus on relevant source changes. Track the pending UTC boundary for either authority and process it before measurements using RA6X-006/007. Clear warnings and reset boundary-spanning windows exactly once while preserving valid post-leap holdover/resync. Preserve leapfile authority when usable and configured; do not let PPS with no calendar information vote down upstream warnings.

**Verification:** Make the majority probe pass; test unchanged system winner, only non-system sources changing LI, loss during a pending leap, and insertion/deletion without a leapfile. Assert timely kernel flags, correct wire LI, and zero spurious steps.

## RA6X-023 — An expired authoritative leapfile can suppress valid upstream warnings

**Severity:** Medium — **Needs investigation: expired-authority service policy**

**Location:** `internal/leap/leap.go`, `Indicator`; `internal/engine/engine.go`, `handle` and `publish`; `cmd/carillon/main.go`, `configurationWarnings`; `DESIGN.md:841–850`.

**Problem:** The design deliberately retains an expired table, but engine use of its authority never expires. Once the table no longer contains a newly announced leap, Indicator returns none and overrides a correct survivor majority; the wire and kernel continue as synchronized despite monitor health being unhealthy. Startup warnings alone do not reach operators whose daemon crosses expiry much later.

**Evidence:** `Indicator` never checks Expiry; `handle` and `publish` override synchronized/holdover LI with its result when LeapTable is nonnil. Expiry is exposed to monitoring and checked by startup warnings only. This is an explicit policy gap, not an accidental failure to reload a changed file.

**Fix specification:** Specify a safe expired-authority policy. Retain the loaded table/provenance for diagnostics as the design requires, but do not silently assert valid current leap knowledge: either cease synchronized service or allow explicitly configured fallback to current survivor consensus. Emit runtime expiry/near-expiry state transitions. Preserve default authority selection and restart-based configuration loading unless a separately designed reload is adopted.

**Verification:** Advance a fake clock through warning/expiry dates while running; supply a later majority-announced leap absent from the file. Assert the selected documented fail-safe policy, timely diagnostics, and consistent kernel/wire/health state. Never silently change authority merely to make the test pass.

## RA6X-024 — Filter startup uncertainty omits all unfilled stages

**Severity:** Medium

**Location:** `internal/discipline/filter.go`, `NewFilter`, dispersion accumulation; `internal/source/ntp.go`, `hit`; `internal/discipline/select.go`, `SourceState.apply`.

**Problem:** The filter weights only real samples and omits uncertainty for absent stages. A single packet can look orders of magnitude more certain than an unprimed eight-stage filter warrants, affecting candidate admission, weighting, and served root dispersion. The prior deliberate skip remains unresolved.

**Evidence:** `TestVerification012UnfilledFilterDispersion` measures 0.0005 s after one 0.001 s-dispersion observation; dummy MAXDISP stages would contribute about 7.938 s. The ordinary `TestFilterSingleSample` asserts the current low value. The existing literal-RFC overlay also demonstrates why changing just the sum is unsafe: frozen quality publication can prevent admission or temporarily elect a lone falseticker.

**Fix specification:** Design priming uncertainty and quality refresh together with RA6X-001/002/010/025. Include absent-stage uncertainty or document a justified alternative conservative admission policy; publish evolving quality without reintegrating old offsets. Preserve eight-stage bounds and outlier rejection. Do not apply the old one-line dispersion patch or weaken falseticker/step assertions to accommodate it.

**Verification:** Run the existing dispersion probe and `probe_rfc_dispersion.py` as diagnostic baselines. Add one-through-eight sample uncertainty checks, equal-delay priming, concurrent honest/false sources, and restart tests. Require zero false steps and eventual convergence with correct conservative uncertainty.

## RA6X-025 — Filtered offsets are paired with metadata from a different packet

**Severity:** Medium

**Location:** `internal/source/ntp.go`, `hit` (449–488), `emit` (533–556); `internal/discipline/system.go:409–418`; `internal/discipline/filter.go`, stage data.

**Problem:** A historical filter winner supplies offset/delay/At, while emit uses stratum, root delay/dispersion, reference identity/time, precision, and leap fields from the latest exchange. The resulting measurement need not describe any actual sample. Upstream reference changes at a stable network address are not a filter-reset boundary. When System starts using an old candidate, its advertised root dispersion also omits the elapsed age already included in selection distance.

**Evidence:** Filter stages carry only offset/delay/dispersion/time. `hit` obtains `out` then passes the latest exchange packet to emit; System sets `rootDisp = sys.RootDisp + sys.Dispersion + sel.Jitter` and `lastUpdate=now`, erasing source-age contribution at handover. Fresh metadata and historical feedback cannot safely share one timestamp without an explicit model.

**Fix specification:** Distinguish latest association metadata from observation-specific quality, retaining enough stage metadata to assess a selected observation honestly. Reset or conservatively revalidate history when the upstream's reference/quality changes materially. Age uncertainty to the publication epoch at selection changes, without double aging on later ticks. Preserve external status names and wire format, and coordinate with the observation-age design in RA6X-001.

**Verification:** Feed packets with distinct delays, offsets, root dispersion, and reference identities so an older stage wins; verify consistent quality bounds and timely current LI/reach metadata. Switch to an aged survivor and assert advertised dispersion is at least its correctly aged bound.

## RA6X-026 — FreeBSD answers directed broadcasts as unicast requests

**Severity:** Medium

**Location:** `internal/server/pktinfo_freebsd.go`, `martianReceiveFlags` and `destination`; `internal/server/martian.go`, `martianDestination`.

**Problem:** The FreeBSD receive path does not reject IPv4 interface broadcast destinations. It copies the received destination into IP_SENDSRCADDR, potentially replying with a broadcast source address. A broadcast request can solicit multiple servers; the martian counters and claimed unicast-only policy are incorrect on this platform.

**Evidence:** `martianReceiveFlags` always returns false; `destination` returns false for every IP_RECVDSTADDR value; the shared check recognizes only limited broadcast and multicast. The FreeBSD source comment explicitly acknowledges the directed-broadcast hole, while the following destination comment incorrectly says the flags check catches it. The existing FreeBSD fixture failed in the prior target-host verification. This review did not send broadcasts or execute FreeBSD binaries.

**Fix specification:** Identify directed broadcasts using receive-interface/address information and current interface broadcast configuration, with refresh on network changes and an explicit policy when metadata is unavailable. Reject before reply construction and count exactly one martian outcome. Preserve valid unicast addresses ending in .255, non-/24 subnets, IPv6, wildcard binding, and multihomed reply-source selection. Do not import nonexistent FreeBSD MSG_BCAST constants or use address-suffix heuristics.

**Verification:** Run the existing FreeBSD martian fixture and controlled target-host packet captures for /24 and non-/24 directed broadcasts, limited broadcast, multicast, and valid secondary unicast addresses. Require zero broadcast replies and correct unicast reply source.

## RA6X-027 — Required-key authentication failures bypass all response limiting

**Severity:** Medium

**Location:** `internal/server/responder.go:197–204`, `cryptoNAK`; `internal/server/ratelimit.go`.

**Problem:** Invalid MACs from a require_key prefix can produce unlimited crypto-NAKs before the token bucket is consulted. A sender able to use or spoof such a source can force authentication work and replies at arrival rate. Moving verification before the legitimate peer's bucket fixed bucket poisoning but left this other path unbounded. These small replies are reflection traffic; the demonstrated issue is not packet-size amplification.

**Evidence:** `TestVerification035CryptoNAKFloodIsBounded` receives 100 NAKs from 100 instantaneous bad-MAC requests with burst 8. The branch returns `h.cryptoNAK(...)` before `h.limiter.allow`. Require-key prefixes are not restricted to a handful of individual addresses despite the explanatory comment.

**Fix specification:** Bound invalid-authentication work and NAK emission using a separate bounded budget that cannot consume legitimate authenticated clients' tokens. Preserve silent rejection of missing/wrong-but-valid keys where currently intentional, MAC verification before trusting key identity, ACL order, response-size bounds, and exactly-one terminal outcome accounting. Ensure all reply paths have a documented budget.

**Verification:** Make the existing flood probe pass; flood absent, unknown, wrong, and known-invalid keys, then require a valid required-key request to succeed. Test IPv6 prefix aggregation, maximum table size, and independent listeners.

## RA6X-028 — Optional authenticated clients share the unsigned client's bucket

**Severity:** Medium

**Location:** `internal/server/responder.go:216–234`; deployment examples using client keys without matching `serve.require_key`.

**Problem:** When a key is optional, verification happens after bucket selection, so replyKey is nil and all requests from the address use key ID zero. Spoofed unsigned traffic can exhaust the allowance of a correctly authenticated client. Only the require_key configuration receives the protection claimed by the address-plus-key limiter abstraction.

**Evidence:** `TestAstra6OptionalAuthHasIndependentBucket` consumes the unsigned allowance and then gets no normal response to a valid optional-key request from the same address. `limitKey := h.limiter.key(addr, keyIDOf(replyKey))` precedes optional verification.

**Fix specification:** Give verified optional-key traffic its intended authenticated bucket, while retaining a separate admission budget that bounds crypto work from unauthenticated traffic. Coordinate with RA6X-027 rather than moving every verification ahead of every limit. Preserve optional authentication: configuring keys alone must not require all clients to authenticate. Never use an unverified trailer ID to allocate privileged tokens.

**Verification:** Make the named probe pass; verify unsigned, valid optional, valid required, and invalid-MAC traffic cannot improperly consume each other's protected allowance and cannot create unbounded bucket entries.

## RA6X-029 — Authenticated clients cannot authenticate this server's RATE replies

**Severity:** Medium

**Location:** `internal/server/responder.go:219–225`; `internal/source/ntp.go`, `exchange` authentication before `IsKiss`.

**Problem:** RATE replies are always unsigned, even after successful required-key verification. Carillon's authenticated client consequently records bad authentication and never executes RATE backoff. Two instances can therefore fail to honor their own authenticated rate-control protocol.

**Evidence:** The RATE path explicitly passes nil as the reply key. `TestAstra6AuthenticatedRATEIsAuthenticated` verifies a valid authenticated request's RATE response and finds no valid MAC. The client rejects missing MACs before interpreting the kiss code.

**Fix specification:** Sign RATE replies to requests whose MAC and permitted identity have been verified; use the same established key and packet framing as normal authenticated responses. Integrate optional-key handling with RA6X-028. Preserve unsigned-client compatibility, KoD emission limits, anti-amplification bounds, origin echo, and silent drops where no KoD is due. Do not sign responses merely because a request claims a key ID.

**Verification:** Make the named probe pass and add a full client/server test: exhaust the authenticated bucket, receive and authenticate RATE, then observe the client's next scheduled transmission honor the backoff.

## RA6X-030 — RATE backoff is not consistently applied or cleared

**Severity:** Medium

**Location:** `internal/source/ntp.go`, `Run`, `handleKiss`, `ensureResolved`; `internal/source/poll.go`, `pollInterval`.

**Problem:** A RATE inside iburst changes poll but does not stop the remaining two-second burst requests. RATE also mutates the configured PollMax; changing DNS address clears only kodMinPoll, so a replacement server inherits the previous server's expanded maximum and current long poll. The ±5% jitter can schedule below a newly asserted minimum interval.

**Evidence:** The burst loop exits only for denial/cancellation. `handleKiss` assigns `n.cfg.PollMax = want`; address change resets the filter and kodMinPoll but not PollMax/current poll. `TestVerification017AddressChangeDropsKissMaximum` reproduces the persistent maximum. `pollInterval` applies symmetric jitter without a floor.

**Fix specification:** Separate immutable configured bounds from association-specific effective bounds and the next-send deadline. End/reschedule iburst when RATE is accepted, enforce the chosen minimum on actual send times, and clear the old server's policy when endpoint identity changes. Preserve randomized scheduling, bounded DNS retry, ordinary iburst behavior, and legitimate configured long polls. Specify which state survives re-resolution to the same endpoint.

**Verification:** Make the existing address-change probe pass. Capture send times for RATE on the first/middle/final burst request, repeated RATE, DNS replacement, same-address refresh, and normal adaptive polling; assert no request precedes the effective deadline.

## RA6X-031 — An untrusted RATE can suppress polling for over a day

**Severity:** Medium

**Location:** `internal/source/ntp.go`, `handleKiss`; `internal/discipline/loop.go`, `MaxPoll`.

**Problem:** A correctly matched but unauthenticated RATE can raise a normally short configured poll to exponent 17: 131072 seconds, about 36.4 hours. Randomized origins make blind off-path injection difficult, but an on-path sender or faulty server can still cause this avoidable availability loss. Ordinary configured long polling and externally demanded backoff are different trust decisions.

**Evidence:** The only cap on the server's requested exponent is discipline.MaxPoll=17, and it overrides PollMax. [RFC 8633 §5.4](https://www.rfc-editor.org/rfc/rfc8633.html#section-5.4) recommends bounding accepted RATE backoff and a maximum exponent no greater than 13; that recommendation is stricter than this implementation. Origin matching is already present and must remain.

**Fix specification:** Define a conservative cap for remotely demanded backoff, particularly unauthenticated RATE, independent of the legal operator-configured poll range. Preserve origin/address validation and authenticated DENY/RSTR semantics. Retain bounded continued polling or explicitly retire an unusable association with visible state; do not silently sleep for an attacker-selected day-scale interval.

**Verification:** Test matched/mismatched origins and extreme signed poll values, authenticated and unsigned RATE, repeated RATE, and configured PollMax above/below the remote-backoff cap. Confirm actual retry deadlines and fallback/holdover visibility.

## RA6X-032 — Control socket startup can delete ordinary files or unlink a live daemon

**Severity:** High

**Location:** `internal/control/server.go:44–69`, `Listen`; `internal/control/control_test.go`, stale-socket test.

**Problem:** Any existing path that does not accept a Unix-socket connection within 500 ms is removed. A mistaken control path destroys a regular file; permissions, backlog exhaustion, or a timeout can also cause a live socket to be unlinked. The latter permits a second listener at the same pathname and defeats the daemon's single-instance check.

**Evidence:** `os.Stat` does not check type, and every DialTimeout failure flows to os.Remove. `TestAstra6ListenPreservesRegularFile` creates a file containing a sentinel and confirms Listen replaces it with a socket. The ordinary stale-socket test itself uses a regular file, encoding the unsafe assumption.

**Fix specification:** Use Lstat and reject non-socket paths, including symlinks. Only remove an owned stale socket after a narrowly classified connection-refused condition; permission errors and timeouts must fail startup without unlinking. Prevent check/unlink/bind races from removing a replacement inode or allowing competing clock owners, using an appropriate ownership/locking scheme. Preserve mode 0660, socket path, ErrInUse discoverability, and legitimate recovery from an abandoned socket.

**Verification:** Make the named probe pass; use a real abandoned Unix socket for stale cleanup. Test regular files, directories, symlinks, permission denial, a busy live listener, and two concurrent starters. Confirm file contents and the original listener remain intact on every refusal.

## RA6X-033 — Control calls ignore cancellation after connecting

**Severity:** Medium

**Location:** `internal/control/client.go:27–110`, `Call` and `WaitSync`.

**Problem:** DialContext handles cancellation only while dialing. Once connected, a cancellable context with no deadline does not interrupt a blocked response read. An indefinite waitsync can hang after cancellation. WaitSync's own timeout is sent to the server but is not installed as a client context deadline; an unresponsive server may exceed it by ten seconds.

**Evidence:** `TestAstra6CallHonorsCancellation` connects to a controlled listener, cancels, and finds Call still blocked after 200 ms; closing the connection is needed for cleanup. Call copies an existing deadline once but has no cancellation callback. WaitSync passes the original ctx to Call and relies on Request.Timeout.

**Fix specification:** Tie established connection lifetime to context cancellation and derive a bounded context for nonzero WaitSync timeouts. Stop callbacks and close connections on every exit. Return errors that retain errors.Is cancellation/deadline semantics. Preserve timeout=0 as intentionally indefinite while ctx remains live and preserve retries only for missing/refused startup sockets.

**Verification:** Make the named probe pass. Test cancellation before dial, after connect, during write/read, and a server that never answers; bound elapsed WaitSync time and check for leaked goroutines or callbacks.

## RA6X-034 — Abandoned waitsync requests accumulate and can deadlock listener failure

**Severity:** Medium

**Location:** `internal/control/server.go`, `Serve`, `handle`, `dispatch(CmdWaitSync)`; `internal/engine/engine.go`, `Wait`.

**Problem:** Each accepted client gets a goroutine. A zero-timeout waitsync clears deadlines and waits only on the daemon context, without detecting client disconnect. Local clients can accumulate indefinitely while unsynchronized. If Accept then fails for a non-timeout error, Serve waits for these handlers before returning, but their context is not cancelled until the caller learns Serve failed: an error-path deadlock.

**Evidence:** The fatal Accept branch calls `s.wg.Wait()` without cancelling a handler context; dispatch passes the parent ctx directly to Engine.Wait. The five-second reply deadline helps only after Wait has finished. Normal request permissions restrict exposure to local socket-authorized users, but disconnect leaks also occur without hostile behavior.

**Fix specification:** Give Serve a child context cancelled on every exit and bind each waiting request to connection closure. Bound concurrent clients/waiters or apply a documented admission policy. Keep writes bounded and ensure fatal listener failure reaches main promptly. Preserve legitimate long waitsync requests and the one-request/one-response protocol.

**Verification:** Repeatedly connect, send waitsync 0, and disconnect while unsynchronized; require waiter counts to return to baseline. Inject fatal Accept failure with a live waiter and require prompt Serve return, handler cleanup, and daemon cancellation.

## RA6X-035 — Positive infinity and unrepresentable durations pass configuration validation

**Severity:** Medium

**Location:** `internal/config/config.go`, `Validate`, `validateServe`; `internal/server/responder.go`, `NewHandler`; `cmd/carillon/main.go`, float-to-duration configuration conversion; `cmd/carillonctl/main.go`, waitsync argument parsing.

**Problem:** Several checks reject NaN through comparisons but accept positive infinity. Infinite holdover prevents expiry, infinite stability spread disables its guard, and infinite rate/burst values undermine limiting. Huge finite seconds can overflow time.Duration; tiny positive seconds can round to zero and silently select a default. CLI waitsync also needs finite/range validation before duration conversion.

**Evidence:** DriftStableSeconds/Spread, HoldoverMax, Step.Panic, RateLimitPPS, and RateBurst use one-sided positive comparisons with no IsInf check. Main directly casts DriftStableSeconds*1e9 to time.Duration. TOML supports `inf`; public constructor callers can also provide math.Inf(1). This differs from refclock offsets, whose validation explicitly checks IsNaN/IsInf.

**Fix specification:** Validate finiteness and meaningful representable ranges at config, CLI, and public constructor boundaries. Reject overflow and unintended zero-after-rounding rather than defaulting after conversion. Keep explicit zero meanings such as CLI waitsync 0 and stats keep_days 0. Preserve existing valid settings, TOML names, defaults, and documented frequency clamps; cover all float fields systematically.

**Verification:** `TestAstra6ConfigurationRejectsInfinity` confirms all six listed infinite settings pass a valid baseline config. Make it pass, then table-test NaN, ±Inf, maximum representable seconds, just-overflowing finite values, sub-nanosecond values, and normal boundary settings through both Parse and constructors. Ensure invalid configs fail before sockets/devices/clock mutations and limiter state never becomes nonfinite.

## RA6X-036 — Presence-sensitive refclock validation silently ignores explicit settings

**Severity:** Low

**Location:** `internal/config/config.go`, `Parse`, `validateGPSRefclock`, refclock-type validation.

**Problem:** Validation relies on decoded values instead of whether a type-specific key was present. Explicit zero/empty values for keys belonging to another refclock type silently pass. This contradicts strict validation and can hide a misplaced calibration or GPS configuration. PPS-specific settings with pps=none likewise need an explicit documented policy.

**Evidence:** The six cases in `TestVerification021RejectsExplicitGPSOnlyZeros` still fail. For example, GPS validation rejects offset only when `r.Offset != 0`; a supplied offset=0 is indistinguishable from absence. Nonzero equivalents are rejected, making diagnostics value-dependent.

**Fix specification:** Retain TOML key presence and reject explicitly supplied inapplicable keys consistently, with a precise source/type/key error. Distinguish parsing policy from programmatically constructed config defaults. Preserve valid defaults, existing key names, correct pps_offset/nmea_offset behavior, and intentionally supported pps=none settings; document any compatibility exception rather than silently ignoring a key.

**Verification:** Run the existing six-case fixture, then test absent, zero, empty, and nonzero variants for both types and GPS with/without PPS. Confirm all shipped examples continue parsing.

## RA6X-037 — DNS address choice can pin an association to an unusable endpoint

**Severity:** Medium

**Location:** `internal/source/ntp.go`, `resolve`, `ensureResolved`, `pollOnce`.

**Problem:** Only the first DNS answer is ever selected. Re-resolution may return the same unusable first answer despite a healthy alternative. Worse, network-unreachable and other general network errors do not increment consecutiveTimeouts, so they never trigger re-resolution at all. A dual-stack or multihomed hostname can remain unusable indefinitely.

**Evidence:** resolve returns addrs[0]. ensureResolved returns early while haveAddr && consecutiveTimeouts<resolveAfterTimeouts. Only errTimeout and ECONNREFUSED update that counter; ENETUNREACH/EHOSTUNREACH reach the generic error branch. There is no list of alternate addresses or attempted-endpoint state.

**Fix specification:** Retain/rotate usable DNS answers on relevant reachability failures, distinguish temporary DNS failure from endpoint failure, and periodically retry without abandoning a healthy association unnecessarily. Reset endpoint-specific filter/RATE state when changing peers (RA6X-030). Preserve literal IP behavior, bounded retries, address-family support, request authentication, and one active exchange per source.

**Verification:** Use injected lookup/exchange seams with first-answer-unreachable/second-answer-healthy, stable answer ordering, DNS changes, all answers failing, transient DNS failure with a still-good cached address, and recovery. Require eventual use of the healthy endpoint without tight retry loops.

## RA6X-038 — Selection has no local timing-loop rejection

**Severity:** Medium

**Location:** `internal/source/ntp.go`, `exchange` and `emit`; `internal/discipline/select.go`, `candidate`; `internal/discipline/system.go`, reference-ID publication.

**Problem:** A peer reporting this host as its reference is still eligible. After losing a real upstream, two mutually configured instances can begin selecting each other's retained time and misrepresenting independence while stratum/uncertainty eventually grow. Configured self-addresses are also not rejected. Distance/stratum limits bound some outcomes but do not replace detecting the loop at admission.

**Evidence:** Exchange checks origin, authentication, stratum, LI, timestamps, delay, and distance; neither it nor candidate compares the peer's RefID with local interface identities. RefID is retained only for status and propagation. [RFC 5905's fitness test](https://www.rfc-editor.org/rfc/rfc5905.html#appendix-A.5.5.3) includes a local-reference loop check. IPv6 reference IDs require the protocol's hashed-address representation.

**Fix specification:** Carry sufficient local endpoint identity to reject direct self-synchronization and a peer whose reference points back to this daemon. Define handling for multihomed hosts, IPv4-mapped addresses, IPv6 hash collisions, NAT, and stratum-1 textual IDs; avoid treating all equal upstream references as loops. Preserve ordinary shared-upstream configurations and read-only querying of the local server.

**Verification:** Simulate two instances with a real reference, remove it, and require the circular candidate to be rejected promptly. Test IPv4/IPv6, multiple local addresses, legitimate peers sharing a third reference, local query, and reference changes.

## RA6X-039 — Device identity checks use path spelling instead of the underlying device

**Severity:** Medium

**Location:** `internal/config/refclock_check_linux.go`, `checkRefclockPlatform`; `internal/config/config.go`, refclock validation/checks; `internal/pps/pps_linux.go`, `Open`, `linuxPPSPath`.

**Problem:** Linux's same-tty GPS/PPS prohibition is enforced only by string equality. Aliases can bypass it and allow N_PPS to replace the NMEA tty discipline. Independently configured refclocks can also open one serial/PPS device twice, compete for bytes, or overwrite device parameters. Conversely, a valid PPS symlink whose basename is not pps followed by digits is misclassified as a tty and fails to open.

**Evidence:** The platform check compares `r.PPS == r.Device`; linuxPPSPath recognizes only a basename pattern. No global device-identity ownership map exists. These branches are confirmed statically; alias/hotplug behavior and shared FreeBSD device operation need target-device verification.

**Fix specification:** Resolve/classify device identity using actual device metadata/capabilities, track ownership across configured refclocks, and reject incompatible duplicate use. Allow the explicitly supported FreeBSD GPS/PPS sharing arrangement. Preserve operator-facing aliases and hotplug/reconnect behavior; revalidate identity when reopening an alias that can be retargeted. Do not solve this by globally forbidding symlinks.

**Verification:** Use controlled aliases and platform-specific fake/open seams for one tty under two names, duplicate GPS blocks, differing PPS edges on one device, and a /dev/ppsN alias. On hardware, confirm supported FreeBSD sharing and restoration of tty state after every failure.

## RA6X-040 — Negative root-delay interoperability needs an explicit representation policy

**Severity:** Medium — **Needs investigation: peer/version interoperability**

**Location:** `internal/ntp/time.go`, `Short.Seconds`; `internal/ntp/packet.go`, RootDelay; `internal/source/ntp.go`, root-distance validation.

**Problem:** RootDelay uses the same unsigned short-format conversion as RootDispersion. A peer encoding a small negative root delay is interpreted as roughly 65536 seconds and rejected. Historic NTP documentation permits signed root delay, whereas RFC 5905 describes short format as unsigned; resolve the actual supported v1–v4 wire behavior before changing the shared type.

**Evidence:** A raw RootDelay of 0xffff0000 decodes as +65535 rather than -1. [RFC 4330 §4](https://www.rfc-editor.org/rfc/rfc4330.html#section-4), superseded by RFC 5905, explicitly describes signed root delay and possible small negative values. Current code has no field-specific signed conversion. No affected live-peer capture was obtained in this review.

**Fix specification:** Check supported ntpd/chrony implementations and relevant protocol versions. If signed root delay is required for interoperability, add a field-specific decode/encode policy and safe distance handling; keep dispersion unsigned and nonnegative. Preserve public numeric units and packet sizes, and reject genuinely excessive or malicious negative values rather than allowing them to cancel uncertainty.

**Verification:** Compare actual peer packets and add raw-bit tests around zero, small negatives, maximum positives, and negative-delay-plus-dispersion combinations for each supported version. Confirm ordinary unsigned root dispersion remains unchanged.

## RA6X-041 — Clock actions are converted to durations without range checks

**Severity:** High

**Location:** `internal/engine/engine.go:529–536,629–635`; `internal/refclock/nmea.go`, offset construction; `internal/config/config.go`, refclock offset validation.

**Problem:** A finite offset need not fit in time.Duration. Engine casts seconds*1e9 directly before stepping, so an extreme value reaches the actuator as an unrelated duration. Finite but enormous calibration values are accepted, ZDA permits year 9999, and time.Time.Sub itself saturates on very large differences. PanicAtStartup can bypass the usual magnitude refusal. This is an actuator-boundary defect even after configuration validation is strengthened.

**Evidence:** `TestAstra6StepRejectsUnrepresentableDuration` supplies a +1e20-second action to Engine.handle with a fake clock. On this arm64 toolchain, handle returns nil and records a +2562047h47m16.854775807s step, roughly 292 years. Other conversion implementations need not produce the same out-of-range result. Kernel error-duration conversions use the same unchecked pattern. No real clock was touched.

**Fix specification:** Validate finiteness and exact representable bounds before each actuator conversion, with overflow-safe conversion and explicit refusal/error propagation. Enforce plausible acquisition/correction rules before losing information through Time.Sub saturation. Preserve legitimate large startup corrections, offset sign, nanosecond units, configured step policy, and backend APIs. Never clamp an unrepresentable phase correction into a centuries-long step.

**Verification:** Make the named fake-actuator probe pass. Cover both signs, NaN/Inf, representable limits and neighboring floating values, huge finite calibration, a future-year sentence, and a legitimate RTC-at-1970 bootstrap. Assert no actuator call on rejection and safe unsynchronized/error status.

## RA6X-042 — NMEA leap-second parsing normalizes or rejects the same instant inconsistently

**Severity:** Medium

**Location:** `internal/refclock/nmea_sentence.go`, `parseNMEA`, `parseNMEAClock`; `internal/refclock/nmea.go`, lastStamp duplicate suppression.

**Problem:** RMC converts 23:59:60 into the following midnight, while ZDA rejects the date rollover produced by the same conversion. RMC's normalized timestamp then collides with the actual next midnight in lastStamp. Second 60 is also allowed at arbitrary minutes without a leap policy. Leap-boundary samples can be misdated, skipped, or admitted with a one-second error.

**Evidence:** parseNMEAClock permits second<=60; RMC validates the calendar date before adding the time, but ZDA validates the final normalized date. Both use time.Date, which cannot represent UTC second 60 distinctly. Additionally, fractional digits beyond nine are discarded before validating their contents, allowing malformed suffixes to pass as time.

**Fix specification:** Represent or deliberately reject leap-boundary sentences consistently and connect their admission to the established leap/epoch policy (RA6X-006/007/022). Do not silently map a leap second to an ordinary next-day sample. Validate the complete fractional field before truncating supported precision. Preserve valid RMC/ZDA timestamps, accepted talkers, checksum requirements, supported subsecond precision, and same-second duplicate suppression.

**Verification:** Test both sentence types at 23:59:59, 23:59:60, and 00:00:00 around insertion and ordinary days; include deletion, arbitrary-minute second 60, and malformed long fractions. Verify the true midnight sample remains usable and no spurious correction occurs.

## RA6X-043 — Receive-buffer fallback can skip the promised minimum

**Severity:** Low

**Location:** `internal/server/listener.go:158–179`, `setReadBuffer`.

**Problem:** Halving an arbitrary requested size can jump below minRecvBuffer without trying it. A system able to provide the documented minimum can therefore fail startup, even though the error says the request was tried down to that minimum.

**Evidence:** With want=100000 and a 65536-byte floor, a refusal at 100000 produces next size=50000 and exits. No request for 65536 occurs. The prior target verification records this fallback gap; this review confirms the loop statically.

**Fix specification:** Clamp the final fallback attempt to the minimum exactly once, terminate on non-capacity errors, and report actual attempted/effective sizes. Preserve current syscall hints, startup failure when even the minimum is unavailable, Linux doubled SO_RCVBUF reporting, and the configured maximum.

**Verification:** Inject a buffer setter that rejects 100000 but accepts 65536. Test power-of-two and non-power-of-two requests, an initial request equal to the floor, all attempts failing, and an immediate non-capacity error.

## RA6X-044 — Synchronous persistence can freeze discipline while the NTP server serves stale synchronization

**Severity:** High

**Location:** `internal/engine/engine.go`, `Run`, `maybeWriteDrift`, `writeDrift`, `publishStatus`; `cmd/carillon/main.go`, NTP Handler.Status closure; `internal/server/responder.go`, status use.

**Problem:** Drift-file create/write/fsync/rename runs synchronously on the sole engine goroutine. A slow filesystem blocks ticks, source handling, and status publication. The UDP listener continues answering from the last immutable snapshot with Synced=true and frozen root dispersion; the last slew word may also remain applied much longer than intended. The server has no independent snapshot-freshness check. A blocking observer/logger presents the same architectural hazard.

**Evidence:** Run calls maybeWriteDrift in its ticker branch; writeDrift calls f.Sync directly. The server closure maps StateSynced/StateHoldover to Synced without checking publication age. Monitor health has a five-second stale test, but that does not affect wire service. This path is confirmed statically; no filesystem stall was induced on the real daemon.

**Fix specification:** Isolate persistence behind a bounded, single-owner worker with immutable validated frequency candidates and observable failures; preserve atomic replacement and stable-write gating. Add an independent monotonic age limit to served status, conservatively aging uncertainty or serving unsynchronized when the engine is stale. Make observer nonblocking requirements enforceable/documented. Coordinate applied-word accounting with RA6X-008 and shutdown with RA6X-045; do not let delayed writes overwrite a newer accepted candidate.

**Verification:** Block an injected drift writer while driving a fake oscillator and an in-process responder. Require ticks/measurement handling to continue, bounded queueing, and no synchronized response from an expired snapshot. Test slow success, permanent error, worker shutdown, and reordered candidates.

## RA6X-045 — The shutdown deadline excludes the engine and frequency restoration

**Severity:** High

**Location:** `internal/engine/engine.go:378–383`, `restoreBaseFrequency`; `cmd/carillon/main.go`, eng.Run and auxiliaries.wait ordering.

**Problem:** Shutdown waits for every source goroutine before restoring base frequency, and final persistence can also block before Run returns. Main's deadline starts only afterward. A stuck source/driver or filesystem can prevent restoration and exit indefinitely; a service manager's eventual kill can leave the phase-slew transient in the kernel. The documented bounded shutdown is therefore incomplete.

**Evidence:** The sequence is cancel → wg.Wait → restoreBaseFrequency → maybeWriteDrift → return, followed by main's auxiliary deadline. No source-drain timeout protects the clock cleanup. Current source interfaces depend on implementations honoring cancellation and device timeout behavior; these expectations are not a bound.

**Fix specification:** Stop accepting updates and restore the base/appropriate kernel status promptly under engine ownership before waiting on untrusted blocking components. Bound source/auxiliary drain and best-effort persistence within an overall shutdown policy, with explicit error reporting. Preserve single-writer clock ownership, no exit-time step, ordinary clean shutdown, and the existing no-frequency-retry rule after a rejected frequency syscall where applicable.

**Verification:** Use a fake source that intentionally ignores cancellation and a separately blocked drift writer. Cancel while a nonzero transient is applied; require prompt fake-clock restoration and bounded process shutdown, with a diagnostic identifying the stuck component. Do not use a real clock or driver hang to test this.

## RA6X-046 — Statistics retention trusts an unsynchronized wall clock

**Severity:** High

**Location:** `internal/stats/writer.go`, `recordServer`, `file`, `prune`; `cmd/carillon/main.go`, stats Config.Now wiring.

**Problem:** Any new dated file invokes destructive retention using that row's wall date, even before the clock is trusted. A future RTC at boot can erase all retained historical statistics on a minute tick or shutdown server row. A future PPS timestamp or other misdated row can trigger the same deletion independently of server statistics.

**Evidence:** `TestAstra6UntrustedWallTimeDoesNotPrune` plants a 2026 statistics file, creates a recorder with Now=2099 and keep_days=7, and calls recordServer without any synchronized engine state. The planted file is deleted. prune removes whole dated directories based solely on the supplied at value. keep_days=0 avoids this path but is not the only supported configuration.

**Fix specification:** Separate the timestamp used to label observations from a trusted retention horizon. Do not advance destructive retention from unsynchronized, implausibly jumping, or source-provided wall time. Define when validated clock history makes a new horizon safe, including boot and large steps. Preserve existing TSV paths/columns, logging of unsynchronized observations, keep_days=0, and intentional UTC-day retention semantics; do not disable all recording until synchronization.

**Verification:** Make the named probe pass. Test future and past RTCs, startup/shutdown before sync, a corrected RTC, misdated PPS rows, forward/backward steps, and trusted ordinary day rotation. Confirm only expired owned statistics are removed once the horizon is trusted.

## RA6X-047 — Statistics omit source loss and state transitions without loop updates

**Severity:** Medium — **Needs investigation: intended diagnostic coverage**

**Location:** `internal/stats/writer.go:131–145`, `process`; `DESIGN.md`, statistics contract.

**Problem:** Loop/source rows are emitted only when Updates changes. During filter starvation, holdover entry/expiry, repeated failures, or source shutdown, the most useful health changes may never be recorded. This is consistent with the documented per-update sampling rate but conflicts with using these files to reconstruct outage and discipline behavior; the limitation should be an explicit operational decision.

**Evidence:** Both writeLoop and writeSources are inside `st.Updates > 0 && st.Updates != r.lastUpdates`. Engine publishes ticks and lifecycle changes, but the recorder discards them for these files. Server traffic rows do not record the missing discipline/source state.

**Fix specification:** Decide and document a bounded event/heartbeat recording contract. Record material state/reach/source changes even if no new phase observation is accepted, without pretending they are loop updates. Preserve existing TSV column meanings and consumers; use an explicitly identified event stream or compatible repeated snapshots rather than silently redefining Updates. Keep I/O off the engine and bound outage log volume.

**Verification:** Simulate synchronized → source loss → holdover → unsynchronized → recovery with no intervening loop updates. Require the chosen recording mechanism to reconstruct the transitions and distinguish them from new measurements, with a bounded row rate.

## RA6X-048 — Per-pulse statistics can silently coalesce accepted PPS events

**Severity:** Medium

**Location:** `internal/engine/engine.go`, `publishStatus`; `internal/refclock/pps.go`, Info publication; `internal/stats/writer.go`, `process` lastPulse logic; `DESIGN.md:1351–1354`.

**Problem:** The claimed per-accepted-pulse file is generated from each source's latest Info when the engine publishes a snapshot, not from the accepted pulse event itself. If multiple pulses arrive before queued measurements are processed, every snapshot can see only the newest pulse. Earlier accepted pulses disappear without the recorder's dropped counter increasing. Measurements and diagnostic pulse data can also describe different events.

**Evidence:** publishStatus calls Source.Info at snapshot time; Recorder reads Refclock.LastPulse/Sequence and deduplicates repeated LastPulse. The engine queue carries Measurement, not an immutable pulse record, and Info retains only the latest pulse. A source advances independently of engine consumption, making coalescing possible even when the recorder queue never fills.

**Fix specification:** Carry accepted-pulse identity/data through an immutable event path with bounded buffering and explicit drop accounting, or explicitly change the diagnostic contract to sampled pulse snapshots. Preserve nonblocking clock discipline, sequence wrap handling, existing TSV columns, and cumulative source counters. Do not infer zero loss from recorder-queue drops alone.

**Verification:** Hold engine processing while a fake PPS source accepts several sequential pulses, then drain without filling the recorder queue. Require one correct row per accepted pulse or a documented loss count that accounts for every omitted event; verify no mismatched timestamp/offset/sequence tuples.

## RA6X-049 — JSON output failures are reported as successful empty responses

**Severity:** Medium

**Location:** `internal/monitor/server.go:172–176`, `writeJSON`; `internal/control/server.go`, `reply`; `cmd/carillonctl/main.go`, JSON encoding.

**Problem:** HTTP status 200 is committed before encoding, and the encoding error is discarded. Nonfinite state, reachable through RA6X-014/035, yields an empty success response. Control serialization logs and closes without a structured error, while the CLI ignores encoding/write errors and can exit successfully with incomplete output.

**Evidence:** `TestAstra6JSONEncodingFailureIsNotSuccess` passes a NaN-valued object to writeJSON and obtains HTTP 200 with an empty body. The function discards Encoder.Encode's result. The control/CLI paths similarly lack a useful client-visible failure outcome.

**Fix specification:** Serialize before committing success headers, return a bounded stable 500/error response on internal encoding failure, and log appropriate diagnostics without exposing secrets. Return nonzero CLI status on failed JSON output. Validate finite state upstream as well; do not silently turn invalid numeric data into healthy zero values or change valid schema/units.

**Verification:** Make the named probe pass; test NaN/Inf in tracking/source fields, a failing output writer, and normal status responses. Assert valid JSON and correct non-success status/error signaling, with the same schema for valid data.

## RA6X-050 — A preferred source that never answers is never reported lost

**Severity:** Medium

**Location:** `internal/discipline/select.go:238–251,310–337`; monitor health and prefer transition events.

**Problem:** PreferLost requires the preferred source to have been reachable at least once. A miswired, misconfigured, or permanently unavailable preferred GPS/upstream can remain absent forever while fallback service is reported healthy. Avoiding noisy startup alerts accidentally suppresses a real persistent failure.

**Evidence:** `reportPreferLost := preferConfigured && preferSeen && ...`; preferSeen comes only from everReachable. `TestVerification036ReportsNeverReachablePreferAfterFallback` advances successful fallback operation while the preferred source never reaches and observes PreferLost=false.

**Fix specification:** Suppress the initial no-source acquisition phase or a documented grace interval, then report an unavailable configured preferred source once fallback is established, regardless of whether it ever answered. Preserve exactly-once loss/recovery transition logs, fallback selection, noselect behavior, and the ability of another valid preferred source to satisfy the policy in the lower-level selector API.

**Verification:** Make the existing probe pass. Test initial acquisition, permanent preferred-source absence, delayed first answer, later loss/recovery, and the existing multiple-prefer selector compatibility case.

## RA6X-051 — PPS disagreement diagnostics outlive the comparison that produced them

**Severity:** Low

**Location:** `internal/discipline/select.go`, Select and ppsAgreement; `internal/control/protocol.go`, `RefclocksOf`.

**Problem:** Losing the numbering source can leave a previously disagreeing PPS marked with a current-looking DisagreesWith/Disagreement value even though it is now unqualified or unreachable and no comparison exists. Operators can chase edge/calibration errors when the present problem is numbering loss.

**Evidence:** These fields are assigned only inside the PPSQualified branch. Candidate rejection and the no-numbering path do not clear them. `TestVerification007ClearsObsoleteDisagreement` still fails; the CLI unconditionally renders the retained text.

**Fix specification:** Clear current disagreement diagnostics whenever the comparison is no longer applicable; if retaining historical diagnostics is useful, expose them explicitly as historical with an observation time. Preserve qualification, locking, source selection, and existing current-field meanings.

**Verification:** Make the existing fixture pass. Test disagreement → numbering loss → recovery/agreement, PPS reach zero, noselect, and clock-step invalidation; assert status and displayed reason describe the current state.

## RA6X-052 — Monitoring server lifecycle does not fully own its listener and shutdown

**Severity:** Low

**Location:** `internal/monitor/server.go:119–142`, Close and Serve; main's constructor error cleanup.

**Problem:** Close before Serve calls only http.Server.Close, which has not registered the already-bound listener, so the port remains occupied. During Serve cancellation, the AfterFunc runs Shutdown asynchronously; Serve may return before active handlers drain, and a shutdown timeout is logged without a forced close. Callers cannot rely on the advertised lifecycle completion.

**Evidence:** `TestAstra6CloseBeforeServeReleasesListener` binds a monitor, calls Close without Serve, and cannot rebind its address. The underlying listener must be closed explicitly for probe cleanup. Serve stops the cancellation callback but never joins an already-running callback.

**Fix specification:** Own and close the bound listener in all pre-Serve/error/Close paths. Join shutdown completion before reporting Serve finished, and force-close remaining connections after the bounded graceful deadline. Make repeated/concurrent cleanup safe. Preserve handlers, ACLs, status codes, and normal graceful request completion.

**Verification:** Make the named probe pass; test construction followed by Close, startup failure after monitor binding, cancellation before Serve, repeated Close, and an active blocked handler at shutdown. Require released ports/connections and bounded completion.

## RA6X-053 — Monitoring freshness and last-activity ordering break across wall-clock steps

**Severity:** Low

**Location:** `internal/monitor/model.go`, SnapshotOf and healthOf; `internal/server/stats.go`, storeLatest/later; `internal/engine/engine.go`, Status publication.

**Problem:** A daemon designed to step wall time uses wall subtraction for snapshot freshness and maximum Unix time for last activity. A backward step makes an old snapshot appear fresh by clamping negative age to zero; last_request/last_served may remain pinned to a pre-step future timestamp. started_at is recomputed as current wall time minus uptime and changes when the clock steps, despite describing one process instance.

**Evidence:** SnapshotOf strips monotonic information with UTC conversion before healthOf computes age. storeLatest refuses any timestamp below the previous UnixNano maximum. There is no immutable process-start wall timestamp or publication-monotonic timestamp in the status model. RA6X-044 makes reliable snapshot age safety-relevant.

**Fix specification:** Track monotonic publication/event ordering independently of display wall timestamps, capture process identity/start metadata once with documented semantics, and preserve latest-event wall time even across steps. Handle concurrent listener updates by event ordering rather than maximum wall value. Preserve JSON/Prometheus field names and units; document what started_at means when boot time was initially wrong.

**Verification:** Simulate forward/backward steps between publication and serving, between accepted requests, and after startup. Assert stale detection uses elapsed time, recent events replace older events, and instance identity remains stable.

## RA6X-054 — ABI verification tests cannot be built because they import C in test files

**Severity:** Medium

**Location:** `internal/pps/pps_linux_cgo_test.go`; `internal/pps/pps_freebsd_cgo_test.go`; `internal/clock/timex_freebsd_cgo_test.go`.

**Problem:** The intended native-header comparisons for hand-written syscall layouts never run: Go rejects import C in _test.go files. Ordinary pure-Go compilation therefore gives false confidence about the independently declared PPS/timex ABI, a particularly important boundary for a clock daemon.

**Evidence:** Safe package-loading commands, without executing tests or hardware operations, fail for Linux with `use of cgo in test pps_linux_cgo_test.go not supported` and for FreeBSD with corresponding PPS and timex filenames. The failure happens before checking native headers or comparing any layout.

**Fix specification:** Put minimal C-backed layout helpers in non-test files guarded by the appropriate platform, cgo, and explicit verification tags, or compile a standalone C layout probe and compare its output. Keep production and cross-builds pure Go with CGO_ENABLED=0. Separate header-only ABI checks from tests that can touch a device or clock, so safe verification does not require enabling hardware mutations.

**Verification:** On native Linux and FreeBSD, compile and run only the layout comparisons for supported architectures against system headers. Require sizes, alignments, offsets, constants, and ioctl argument sizes to match; also rebuild ordinary CGO_ENABLED=0 binaries and confirm no C dependency enters them.

## RA6X-055 — The documented race-test Make target fails on deployment platforms

**Severity:** Medium

**Location:** `Makefile`, global CGO_ENABLED export and test target.

**Problem:** Make exports CGO_ENABLED=0 but its test recipe requests -race. On Linux and FreeBSD the Go race build requires cgo, so make test stops before executing the suite. Successful macOS testing with this particular toolchain does not validate the target-host command.

**Evidence:** With Go 1.27, `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -race ./internal/discipline` and the FreeBSD equivalent both return `go: -race requires cgo`. Native darwin/arm64 make test succeeds with this toolchain, so this is not claimed as a failure on every platform.

**Fix specification:** Enable cgo specifically for supported native race-test invocations and require a suitable native compiler/runtime; retain CGO_ENABLED=0 for production builds. Document unsupported cross-race execution and keep the Go-version support matrix accurate. Do not weaken the test target by dropping -race silently.

**Verification:** Run make test natively on Linux and FreeBSD with the supported Go/compiler setup, plus the development host. Re-run all four pure-Go production cross-builds and inspect that their linking properties remain unchanged.

## RA6X-056 — The serial EOF regression asserts the wrong kernel event path

**Severity:** Low

**Location:** `internal/serial/read_unix_test.go`, TestReadTimeoutReportsEOF; `internal/serial/read_unix.go`, POLLHUP handling.

**Problem:** Closing a pipe writer causes poll to report HUP, which production intentionally converts to a device-unavailable error before calling read. The test insists on io.EOF, so it fails on the supported hosts while not exercising the repaired zero-length-read path at all. This is a test defect; the actual HUP path already avoids the busy loop.

**Evidence:** The prior Linux/FreeBSD target results show this assertion failing. Current ReadTimeout checks POLLERR/POLLHUP/POLLNVAL before unix.Read. This review cross-built the code but did not execute those kernels' serial tests.

**Fix specification:** Keep a real closed-pipe test asserting prompt non-timeout disconnect, and add an injected poll/read seam or equivalent controlled fixture that actually produces n=0, err=nil after readable polling. Preserve existing production disconnect classification unless an explicit error-contract change is intended; do not alter correct HUP behavior solely to satisfy this assertion.

**Verification:** Run both tests on Linux and FreeBSD. Confirm the zero-read path wraps io.EOF, HUP is a non-timeout error, cancellation remains bounded, and neither path busy-spins.

## RA6X-057 — Source-count validation does not express an achievable or independent quorum

**Severity:** Medium — **Needs investigation: quorum/configuration policy**

**Location:** `internal/config/config.go`, Validate; `internal/discipline/select.go`, Select/intersect/cluster; `internal/source/ntp.go`, endpoint resolution.

**Problem:** Validation accepts min_survivors larger than the available selectable sources, all-noselect configurations, and bare PPS without any numbering source. These can remain permanently unsynchronized with no startup explanation. Conversely, distinct source names resolving to the same endpoint count as separate votes; GPS NMEA/PPS also share a physical clock. The actual meaning of a source quorum is under-specified.

**Evidence:** Validate only checks that some source block exists, names are unique, and MinSurvivors>=1. Selection counts SourceState entries. No physical/endpoint identity contributes to the vote count, and cluster's stopping floor is a fixed three rather than a general configured quorum policy. A high minimum can legitimately choose to refuse after clustering; that behavior alone is not proof of a clustering bug.

**Fix specification:** Define whether min_survivors means surviving associations or independent clocks and document dependent refclock roles. Diagnose statically impossible synchronization configurations, while preserving any intentionally supported monitoring-only configuration through an explicit policy. Detect/report duplicate resolved endpoints and avoid presenting them as independent agreement if independence is promised. Do not automatically raise/lower quorum or keep a falseticker just to meet the count.

**Verification:** Test impossible minima, all-noselect, sole bare PPS, valid PPS plus numbering, multiple names for one endpoint, DNS convergence/divergence, and four-or-more-source clustering. Require actionable diagnostics and behavior consistent with the documented quorum definition.

## RA6X-058 — Drift replacement lacks an explicit power-loss durability contract

**Severity:** Low — **Needs investigation: supported-filesystem durability**

**Location:** `internal/engine/engine.go:275–305`, writeDrift.

**Problem:** The temporary file is synced before chmod/rename, but the containing directory is never synced after replacement. Atomic visibility during normal execution does not itself establish that the new directory entry survives power loss on every supported filesystem. A last-known-good calibration can revert or disappear after a crash even though writeDrift returned success.

**Evidence:** The syscall sequence ends at os.Rename; there is no parent-directory sync or stated filesystem-specific durability guarantee. This review did not simulate power loss. The source promises atomic replacement, so the unresolved point is whether restart calibration requires durable replacement too, not whether rename is normally atomic.

**Fix specification:** Decide the required persistence guarantee and verify it on supported Linux/FreeBSD filesystems. If successful persistence must survive power loss, order metadata changes and file/directory syncing appropriately and handle unsupported/error cases explicitly. Preserve atomic reader visibility, known-good contents on pre-rename failure, file mode, and stable-write gating; coordinate with RA6X-015/044 so durability work cannot block discipline.

**Verification:** Use filesystem fault/crash testing in a disposable environment and injected syscall failures around write, sync, chmod, rename, and directory sync. Require either the old complete value or the newly committed complete value according to the documented guarantee, with no falsely reported durable success.

## RA6X-059 — Queued measurement timestamps rewind the engine's processing time

**Severity:** Medium

**Location:** `internal/engine/engine.go:361–366`, `publishStatus`; `internal/discipline/system.go`, Update/reselect; source Measurement.Now producers.

**Problem:** Engine uses the producer's enqueue-time Now as the current time for selection, loop processing, and publication. Buffered or cross-source measurements can arrive after a newer tick/measurement, making global time move backward. Candidate age is understated, loop intervals become inconsistent, and uptime/root-uncertainty snapshots regress even while the actual monotonic clock advances. Epoch checks do not reject this ordinary queue delay.

**Evidence:** The receive branch executes `e.handle(e.sys.Update(m), m.Now)`; System.Update calls reselect(m.Now). `TestAstra6QueuedMeasurementDoesNotRewindEngineTime` publishes at monotonic 100, then processes a queued Now=1 observation through the same calls and sees Uptime fall from 1m40s to 1s. Measurement.At already exists for observation time but processing time is not independently established at dequeue.

**Fix specification:** Establish a nondecreasing engine processing clock at consumption, retaining the actual observation/acquisition timestamps separately. Evaluate freshness and deadlines at processing time and reject or compensate delayed feedback according to RA6X-001/025; merely rewriting At would conceal staleness. Define ordering for lifecycle events, ticks, and multiple producers, preserving correct generation boundaries and public time units. Do not let old events make a source younger or restart a past timeout.

**Verification:** Make the named probe pass. Deliver interleaved sources and queued samples around ticks, holdover expiry, source stop/restart, and clock steps; require monotonic uptime/deadlines, honest observation ages, and no duplicated or backward-time feedback integration.

## Reproducible Astra6 probes

These review fixtures were run only in a temporary copy with fake clocks and temporary files/sockets. They are embedded here to keep the repository change limited to this report. The assertions express desired behavior and **fail on the reviewed baseline**. The receiver-validity assertion for RA6X-021 demonstrates the admitted contradictory state; finalize the receiver policy before making that assertion normative. These are focused counterexamples, not complete future regression suites.

From the repository root, the following runner extracts the nine fixtures into a temporary directory and adds them to the Go build with an overlay. It does not edit any repository source or existing tests. It requires Python 3, a compatible Go installation, native race support, and permission to bind temporary local sockets. Short temporary paths avoid Unix-socket path-length skips on macOS. Exit status 1 is expected on this baseline.

```python
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

root = Path.cwd()
report = root / "review/2026/09/REVIEW_ASTRA6_XHIGH.md"
fixtures = re.findall(
    r"^### Probe file: `([^`]+)`\n\n```go\n(.*?)^```$",
    report.read_text(), re.MULTILINE | re.DOTALL,
)
assert len(fixtures) == 9, "Review fixture inventory changed; inspect before running"
with tempfile.TemporaryDirectory(prefix="ra6x-", dir="/tmp") as name:
    temporary = Path(name)
    mapping = {}
    packages = set()
    for relative, code in fixtures:
        path = Path(relative)
        assert path.parts[0] == "internal" and ".." not in path.parts
        assert path.name.startswith("astra6") and path.name.endswith("_test.go")
        assert not (root / path).exists(), f"Refusing to hide existing file: {path}"
        target = temporary / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(code)
        mapping[str(root / path)] = str(target)
        packages.add("./" + str(path.parent))
    overlay = temporary / "overlay.json"
    overlay.write_text(json.dumps({"Replace": mapping}))
    env = os.environ.copy()
    env["CGO_ENABLED"] = "1"
    env["TMPDIR"] = "/tmp"
    command = [shutil.which("go") or "/opt/local/bin/go", "test", "-race",
               "-overlay", str(overlay), "-run", "^TestAstra6", "-count=1", "-v",
               *sorted(packages)]
    raise SystemExit(subprocess.run(command, cwd=root, env=env).returncode)
```

### Probe file: `internal/config/astra6_review_test.go`

```go
package config

import (
	"math"
	"testing"
)

func TestAstra6ConfigurationRejectsInfinity(t *testing.T) {
	mutations := map[string]func(*Config){
		"drift_seconds": func(c *Config) { c.Daemon.DriftStableSeconds = math.Inf(1) },
		"drift_spread":  func(c *Config) { c.Daemon.DriftStableSpreadPPM = math.Inf(1) },
		"holdover":      func(c *Config) { c.Discipline.HoldoverMax = math.Inf(1) },
		"panic":         func(c *Config) { c.Step.Panic = math.Inf(1) },
		"rate":          func(c *Config) { c.Serve.RateLimitPPS = math.Inf(1) },
		"burst":         func(c *Config) { c.Serve.RateBurst = math.Inf(1) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			c := Default()
			c.Servers = []Server{{Name: "a", Address: "192.0.2.1", PollMin: 6, PollMax: 10}}
			c.Serve.Allow = []string{"127.0.0.0/8"}
			c.Serve.Listen = []string{"127.0.0.1:123"}
			if err := Validate(c); err != nil {
				t.Fatalf("setup: %v", err)
			}
			mutate(c)
			if err := Validate(c); err == nil {
				t.Fatal("infinite setting accepted")
			}
		})
	}
}
```

### Probe file: `internal/control/astra6_review_test.go`

```go
package control

import (
	"context"
	"net"
	"os"
	"testing"
	"time"
)

func TestAstra6ListenPreservesRegularFile(t *testing.T) {
	p := socketPath(t)
	if err := os.WriteFile(p, []byte("valuable contents"), 0600); err != nil {
		t.Fatal(err)
	}
	srv, err := Listen(p, newEngine(t), nil, "v", nil)
	if srv != nil {
		defer srv.ln.Close()
	}
	b, readerr := os.ReadFile(p)
	if err == nil || readerr != nil || string(b) != "valuable contents" {
		t.Fatalf("existing file replaced: Listen=%v, read=%v, contents=%q", err, readerr, b)
	}
}

func TestAstra6CallHonorsCancellation(t *testing.T) {
	p := socketPath(t)
	ln, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := Call(ctx, p, Request{Command: CmdWaitSync}); done <- e }()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b := make([]byte, 256)
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err = c.Read(b); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		c.Close()
		<-done
		t.Fatal("Call still blocked 200 ms after context cancellation")
	}
}
```

### Probe file: `internal/discipline/astra6_review_test.go`

```go
package discipline

import (
	"carillon/internal/ntp"
	"testing"
)

func TestAstra6SettlingLossDoesNotSynchronize(t *testing.T) {
	cfg := simConfig()
	cfg.SettleUpdates = 3
	s := New(cfg, 0, true)
	s.AddSource("a", Options{Numbering: true})
	s.Update(Measurement{Source: "a", Now: 1, At: 1, Valid: true, Reach: 255, Poll: 6, Offset: 0.001, Delay: 0.01, Jitter: 1e-6, Stratum: 2, Leap: ntp.LeapNone})
	if s.State() != StateSettling {
		t.Fatalf("setup state=%v", s.State())
	}
	s.Update(Measurement{Source: "a", Now: 2, Reach: 0, Poll: 6})
	st := s.Status(2)
	if st.State == StateHoldover || st.Leap != ntp.LeapUnsync || st.Stratum != 16 {
		t.Fatalf("never synchronized but loss yields state=%v LI=%v stratum=%d", st.State, st.Leap, st.Stratum)
	}
}

func TestAstra6NeverStepIncludesPanicStartup(t *testing.T) {
	cfg := loopCfg()
	cfg.StepLimit = 0
	cfg.PanicAtStartup = true
	u := NewLoop(cfg, 0, true).Update(2000, 6, 1, false, true)
	if u.Stepped {
		t.Fatal("limit=0 and panic_at_startup=true issued a 2000-second step")
	}
}

func TestAstra6MissIsNotSettlingEvidence(t *testing.T) {
	cfg := simConfig()
	cfg.SettleUpdates = 1
	s := New(cfg, 0, true)
	s.AddSource("a", Options{Numbering: true})
	s.Update(Measurement{Source: "a", Now: 1, At: 1, Valid: true, Reach: 255, Poll: 6, Offset: 0.001, Delay: 0.01, Jitter: 1e-6, Stratum: 2, Leap: ntp.LeapNone})
	s.Update(Measurement{Source: "a", Now: 65, Reach: 254, Poll: 6})
	if s.State() == StateSynced {
		t.Fatal("timeout-only measurement completed settling")
	}
}

func TestAstra6LeapUpdatesWithoutSystemFeedback(t *testing.T) {
	s := New(simConfig(), 0, true)
	s.AddSource("a", Options{Numbering: true, Prefer: true})
	s.AddSource("b", Options{Numbering: true})
	s.AddSource("c", Options{Numbering: true})
	m := Measurement{Now: 1, At: 1, Valid: true, Reach: 255, Poll: 6, Delay: 0.01, Jitter: 1e-6, Stratum: 2, Leap: ntp.LeapNone}
	for _, name := range []string{"a", "b", "c"} {
		m.Source = name
		s.Update(m)
	}
	m.Now = 2
	m.At = 2
	m.Leap = ntp.LeapInsert
	for _, name := range []string{"b", "c"} {
		m.Source = name
		s.Update(m)
	}
	if s.leap != ntp.LeapInsert {
		t.Fatalf("2/3 survivors announce insertion, system still LI=%v", s.leap)
	}
}
```

### Probe file: `internal/engine/astra6_bounds_review_test.go`

```go
package engine

import (
	"carillon/internal/clock"
	"carillon/internal/discipline"
	"testing"
	"time"
)

func TestAstra6StepRejectsUnrepresentableDuration(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
	e, err := New(testConfig(""), clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	err = e.handle(discipline.Result{Actions: []discipline.Action{{Kind: discipline.ActionStep, Value: 1e20}}}, 1)
	if err == nil || len(clk.Steps) != 0 {
		t.Fatalf("unrepresentable positive step reached actuator: err=%v steps=%v", err, clk.Steps)
	}
}

func TestAstra6QueuedMeasurementDoesNotRewindEngineTime(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
	e, err := New(testConfig(""), clk, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	e.sys.AddSource("a", discipline.Options{Numbering: true})
	clk.Advance(100 * time.Second)
	if err := e.handle(e.sys.Tick(clk.Monotonic()), clk.Monotonic()); err != nil {
		t.Fatal(err)
	}
	before := e.Status().Uptime
	m := good(0.001)
	m.Source = "a"
	m.Now = 1
	m.At = 1
	if err := e.handle(e.sys.Update(m), m.Now); err != nil {
		t.Fatal(err)
	}
	if e.Status().Uptime < before {
		t.Fatalf("processing a queued measurement rewound uptime: %v -> %v", before, e.Status().Uptime)
	}
}
```

### Probe file: `internal/engine/astra6_review_test.go`

```go
package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAstra6DriftRejectsNaN(t *testing.T) {
	p := filepath.Join(t.TempDir(), "drift")
	if err := os.WriteFile(p, []byte("NaN\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if v, err := readDrift(p); err == nil {
		t.Fatalf("non-finite drift accepted: %v", v)
	}
}
```

### Probe file: `internal/monitor/astra6_review_test.go`

```go
package monitor

import (
	"carillon/internal/engine"
	"math"
	"net"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestAstra6JSONEncodingFailureIsNotSuccess(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, 200, map[string]float64{"frequency": math.NaN()})
	if w.Code == 200 {
		t.Fatalf("encoding failure returned HTTP %d with body %q", w.Code, w.Body.String())
	}
}
func TestAstra6CloseBeforeServeReleasesListener(t *testing.T) {
	s, err := Listen(Config{Listen: netip.MustParseAddrPort("127.0.0.1:0"), Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, Status: func() *engine.Status { return &engine.Status{} }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.ln.Close()
	addr := s.Addr().String()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("Close before Serve retained the bound port: %v", err)
	}
	ln.Close()
}
```

### Probe file: `internal/refclock/astra6_review_test.go`

```go
package refclock

import (
	"carillon/internal/discipline"
	"context"
	"errors"
	"testing"
	"time"
)

type astraSerial struct{ close func() }

func (r *astraSerial) ReadTimeout([]byte, time.Duration) (int, error) {
	return 0, errors.New("device disconnected")
}
func (r *astraSerial) Close() error { r.close(); return nil }

func TestAstra6NMEACancelDuringReconnect(t *testing.T) {
	n, _, _ := testNMEA(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n.reader = &astraSerial{close: cancel}
	n.opener = func() (serialReader, error) { return nil, errors.New("absent") }
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("cancellation panicked: %v", p)
		}
	}()
	if err := n.Run(ctx, make(chan discipline.Measurement, 1)); err != nil {
		t.Fatal(err)
	}
}

func TestAstra6NMEASilenceRequiresFreshWindow(t *testing.T) {
	n, _, _ := testNMEA(t)
	base := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	feed := func(stamp time.Time, mono float64, offset time.Duration) discipline.Measurement {
		m, ok := n.acceptLine(sentence("GPRMC,"+stamp.Format("150405")+",A,,,,,,,"+stamp.Format("020106")+",,,"), stamp.Add(150*time.Millisecond-offset), mono)
		if !ok {
			t.Fatal("sentence rejected")
		}
		return m
	}
	for i := 0; i < 16; i++ {
		feed(base.Add(time.Duration(i)*time.Second), float64(i+1), 0)
	}
	n.tick(100)
	if n.reach != 0 {
		t.Fatalf("setup reach=%d", n.reach)
	}
	m := feed(base.Add(100*time.Second), 101, 100*time.Millisecond)
	if m.Valid {
		t.Fatalf("one fresh sample after silence is valid with historical offset=%g, current=0.1", m.Offset)
	}
}

func TestAstra6NMEARecoversAfterFutureDate(t *testing.T) {
	n, _, _ := testNMEA(t)
	arrival := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	n.acceptLine(sentence("GPZDA,120000,23,08,2099,00,00"), arrival, 1)
	n.resetWindow()
	if _, ok := n.acceptLine(sentence("GPZDA,120001,23,08,2026,00,00"), arrival.Add(time.Second), 2); !ok {
		t.Fatalf("correct time rejected after reset; lastStamp=%v", n.lastStamp)
	}
}

func TestAstra6ZDAHonorsKnownInvalidTime(t *testing.T) {
	n, _, _ := testNMEA(t)
	arrival := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	n.acceptLine(sentence("GPRMC,120000,V,,,,,,,230826,,,"), arrival, 1)
	for i := 0; i < 4; i++ {
		stamp := arrival.Add(time.Duration(i) * time.Second)
		m, ok := n.acceptLine(sentence("GPZDA,"+stamp.Format("150405")+",23,08,2026,00,00"), stamp.Add(150*time.Millisecond), float64(i+2))
		if ok && m.Valid {
			t.Fatalf("ZDA admitted while latest RMC status V; FixValid=%v", n.Info().Refclock.FixValid)
		}
	}
}
```

### Probe file: `internal/server/astra6_review_test.go`

```go
package server

import (
	"carillon/internal/ntp"
	"net/netip"
	"testing"
	"time"
)

func TestAstra6AuthenticatedRATEIsAuthenticated(t *testing.T) {
	h, _ := newTestHandler(t, func(c *Config) {
		c.RateBurst = 1
		c.RateLimitPPS = 1
		c.RequireKey = map[netip.Prefix]uint32{netip.MustParsePrefix("192.0.2.0/24"): 1}
	})
	req := testKey.Append(request(4))
	h.Handle(req, from("192.0.2.1"), testWall, testMono)
	got := h.Handle(req, from("192.0.2.1"), testWall, testMono.Add(time.Millisecond))
	p, mac, off, err := ntp.Decode(got)
	if err != nil || p.ReferenceID != ntp.KissRATE {
		t.Fatalf("setup no RATE: %v %+v", err, p)
	}
	if !testKey.Verify(got[:off], mac) {
		t.Fatal("authenticated client's RATE response has no valid MAC")
	}
}

func TestAstra6OptionalAuthHasIndependentBucket(t *testing.T) {
	h, _ := newTestHandler(t, func(c *Config) { c.RateBurst = 1; c.RateLimitPPS = 1; c.KoD = false })
	h.Handle(request(4), from("192.0.2.1"), testWall, testMono)
	if got := h.Handle(testKey.Append(request(4)), from("192.0.2.1"), testWall, testMono); len(got) == 0 {
		t.Fatal("unsigned request drained optional authenticated client's bucket")
	}
}
```

### Probe file: `internal/stats/astra6_review_test.go`

```go
package stats

import (
	ntpserver "carillon/internal/server"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAstra6UntrustedWallTimeDoesNotPrune(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "2026", "09", "05", "loop.tsv")
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("evidence\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := New(Config{Dir: dir, KeepDays: 7, Now: func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }, Server: func() ntpserver.StatsSnapshot { return ntpserver.StatsSnapshot{} }})
	defer r.closeFiles()
	if err := r.recordServer(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("untrusted future RTC deleted retained evidence: %v", err)
	}
}
```

## Summary by severity

| Severity | Findings | Needs investigation |
|---|---:|---:|
| Critical | 0 | 0 |
| High | 25 | 1 |
| Medium | 27 | 4 |
| Low | 7 | 1 |
| **Total** | **59** | **6** |

The following index includes every finding, grouped by severity. “Investigation” identifies the six findings whose final behavior needs the specified external evidence or policy decision.

| ID | Severity | Finding | Status |
|---|---|---|---|
| RA6X-001 | High | Delayed filter observations destabilize the discipline loop | Actionable; see evidence/verification |
| RA6X-002 | High | The filter can withhold useful corrections for many polls | Actionable; see evidence/verification |
| RA6X-003 | High | Time-based source expiry never runs on engine ticks | Actionable; see evidence/verification |
| RA6X-004 | High | Device reconnect loops retain reachable, selectable estimates | Actionable; see evidence/verification |
| RA6X-005 | High | Re-priming PPS does not invalidate the selector's old lock | Actionable; see evidence/verification |
| RA6X-006 | High | Generation stamping does not bracket clock-step execution | Actionable; see evidence/verification |
| RA6X-007 | High | Leap transitions are detected after queued corrections execute | Actionable; see evidence/verification |
| RA6X-008 | High | Phase debit uses the next frequency word for the previous interval | Actionable; see evidence/verification |
| RA6X-009 | High | Losing a source during settling promotes untrusted time to holdover | Actionable; see evidence/verification |
| RA6X-010 | High | Failed polls count as successful settling evidence | Actionable; see evidence/verification |
| RA6X-011 | High | Panic-at-startup bypasses the never-step setting | Actionable; see evidence/verification |
| RA6X-013 | High | A frozen transient frequency is accepted as stable drift | Actionable; see evidence/verification |
| RA6X-014 | High | NaN in the drift file reaches the clock-control state | Actionable; see evidence/verification |
| RA6X-015 | High | Startup temporary cleanup can delete the configured drift file | Actionable; see evidence/verification |
| RA6X-017 | High | Source-stop events can be overtaken by queued measurements and swallow fatal errors | Actionable; see evidence/verification |
| RA6X-018 | High | Canceling an NMEA reconnect panics the daemon | Actionable; see evidence/verification |
| RA6X-019 | High | NMEA reacquisition reuses a stale pre-outage median | Actionable; see evidence/verification |
| RA6X-020 | High | One future GPS date can suppress all subsequent correct time | Actionable; see evidence/verification |
| RA6X-021 | High | ZDA can discipline time while the receiver reports an invalid fix | Investigation |
| RA6X-022 | High | Leap warnings are coupled to loop updates and lack a fileless boundary reset | Actionable; see evidence/verification |
| RA6X-032 | High | Control socket startup can delete ordinary files or unlink a live daemon | Actionable; see evidence/verification |
| RA6X-041 | High | Clock actions are converted to durations without range checks | Actionable; see evidence/verification |
| RA6X-044 | High | Synchronous persistence can freeze discipline while the NTP server serves stale synchronization | Actionable; see evidence/verification |
| RA6X-045 | High | The shutdown deadline excludes the engine and frequency restoration | Actionable; see evidence/verification |
| RA6X-046 | High | Statistics retention trusts an unsynchronized wall clock | Actionable; see evidence/verification |
| RA6X-012 | Medium | Panic refusal is logged but does not stop the daemon as specified | Actionable; see evidence/verification |
| RA6X-016 | Medium | Shutdown before the first tick leaves the wrong base frequency applied | Actionable; see evidence/verification |
| RA6X-023 | Medium | An expired authoritative leapfile can suppress valid upstream warnings | Investigation |
| RA6X-024 | Medium | Filter startup uncertainty omits all unfilled stages | Actionable; see evidence/verification |
| RA6X-025 | Medium | Filtered offsets are paired with metadata from a different packet | Actionable; see evidence/verification |
| RA6X-026 | Medium | FreeBSD answers directed broadcasts as unicast requests | Actionable; see evidence/verification |
| RA6X-027 | Medium | Required-key authentication failures bypass all response limiting | Actionable; see evidence/verification |
| RA6X-028 | Medium | Optional authenticated clients share the unsigned client's bucket | Actionable; see evidence/verification |
| RA6X-029 | Medium | Authenticated clients cannot authenticate this server's RATE replies | Actionable; see evidence/verification |
| RA6X-030 | Medium | RATE backoff is not consistently applied or cleared | Actionable; see evidence/verification |
| RA6X-031 | Medium | An untrusted RATE can suppress polling for over a day | Actionable; see evidence/verification |
| RA6X-033 | Medium | Control calls ignore cancellation after connecting | Actionable; see evidence/verification |
| RA6X-034 | Medium | Abandoned waitsync requests accumulate and can deadlock listener failure | Actionable; see evidence/verification |
| RA6X-035 | Medium | Positive infinity and unrepresentable durations pass configuration validation | Actionable; see evidence/verification |
| RA6X-037 | Medium | DNS address choice can pin an association to an unusable endpoint | Actionable; see evidence/verification |
| RA6X-038 | Medium | Selection has no local timing-loop rejection | Actionable; see evidence/verification |
| RA6X-039 | Medium | Device identity checks use path spelling instead of the underlying device | Actionable; see evidence/verification |
| RA6X-040 | Medium | Negative root-delay interoperability needs an explicit representation policy | Investigation |
| RA6X-042 | Medium | NMEA leap-second parsing normalizes or rejects the same instant inconsistently | Actionable; see evidence/verification |
| RA6X-047 | Medium | Statistics omit source loss and state transitions without loop updates | Investigation |
| RA6X-048 | Medium | Per-pulse statistics can silently coalesce accepted PPS events | Actionable; see evidence/verification |
| RA6X-049 | Medium | JSON output failures are reported as successful empty responses | Actionable; see evidence/verification |
| RA6X-050 | Medium | A preferred source that never answers is never reported lost | Actionable; see evidence/verification |
| RA6X-054 | Medium | ABI verification tests cannot be built because they import C in test files | Actionable; see evidence/verification |
| RA6X-055 | Medium | The documented race-test Make target fails on deployment platforms | Actionable; see evidence/verification |
| RA6X-057 | Medium | Source-count validation does not express an achievable or independent quorum | Investigation |
| RA6X-059 | Medium | Queued measurement timestamps rewind the engine's processing time | Actionable; see evidence/verification |
| RA6X-036 | Low | Presence-sensitive refclock validation silently ignores explicit settings | Actionable; see evidence/verification |
| RA6X-043 | Low | Receive-buffer fallback can skip the promised minimum | Actionable; see evidence/verification |
| RA6X-051 | Low | PPS disagreement diagnostics outlive the comparison that produced them | Actionable; see evidence/verification |
| RA6X-052 | Low | Monitoring server lifecycle does not fully own its listener and shutdown | Actionable; see evidence/verification |
| RA6X-053 | Low | Monitoring freshness and last-activity ordering break across wall-clock steps | Actionable; see evidence/verification |
| RA6X-056 | Low | The serial EOF regression asserts the wrong kernel event path | Actionable; see evidence/verification |
| RA6X-058 | Low | Drift replacement lacks an explicit power-loss durability contract | Investigation |

## Suggested fix order and dependencies

Treat these as integration waves, with independent containment fixes proceeding alongside the larger discipline design. The primary acceptance gate is actual fake-clock error plus consistent wire/kernel/state behavior, not just a passing unit suite. Preserve the reviewed baseline/reproductions so failures cannot disappear through weakened assertions. No public API, wire format, configuration key, or TSV schema should change incidentally; each finding specifies its compatibility constraints.

| Order | Work | Findings, in suggested local order | Dependencies and acceptance gate |
|---:|---|---|---|
| 0 | Enable native verification alongside the fixes | RA6X-054, RA6X-055, RA6X-056 | Repair target race and ABI/serial checks before treating cross-build success as runtime validation. These changes can proceed alongside the immediate safeguards. |
| 1 | Guard inputs and the clock actuator | RA6X-014, RA6X-035, RA6X-041, RA6X-011, RA6X-018, RA6X-016 | Reject nonfinite/unrepresentable input before action generation/conversion; preserve never-step policy and make reconnect cancellation safe. Verify initial-frequency restoration with the fake actuator. |
| 2 | Stop destructive filesystem mistakes | RA6X-015, RA6X-032, RA6X-046 | Independent urgent fixes: constrain drift cleanup, protect the control path, and establish a trusted retention horizon. Do not defer these data-loss paths until algorithm work finishes. |
| 3 | Establish event time, epochs, and source invalidation | RA6X-059, RA6X-006, RA6X-007, RA6X-017, RA6X-004, RA6X-005, RA6X-019, RA6X-020, RA6X-021, RA6X-022, RA6X-042 | Define processing versus observation time and a step/leap protocol first. Carry source lifecycle/invalidation through it. Resolve receiver validity, chronology, and both leap authorities before accepting reacquired or boundary samples. |
| 4 | Make eligibility and synchronized state truthful | RA6X-003, RA6X-009, RA6X-010, RA6X-023, RA6X-038, RA6X-057, RA6X-050, RA6X-051, RA6X-037, RA6X-039 | Build expiry/settling on the new event contract. Decide expired leap authority and quorum semantics; fix loss diagnostics and endpoint/device recovery. Preserve successful fallback and legitimate refclock numbering. |
| 5 | Complete the coupled discipline redesign | RA6X-001, RA6X-002, RA6X-008, RA6X-024, RA6X-025, RA6X-013 | Start this design immediately; integrate after the time/epoch and eligibility contracts are established. Observation age, feedback cadence, applied-word accounting, priming uncertainty, and metadata must be validated together. Only then certify drift stability/persistence. |
| 6 | Finish fatal-error, persistence, and shutdown ownership | RA6X-012, RA6X-044, RA6X-045 | Fatal refusal relies on result/error propagation in RA6X-017. Isolate durable writes and expire stale served snapshots; restore the actuator before bounded draining. Stress stalls with fake sources/filesystems. |
| 7 | Close protocol and authenticated limiting gaps | RA6X-026, RA6X-027, RA6X-028, RA6X-029, RA6X-031, RA6X-030, RA6X-040 | Design separate unauthenticated/authenticated budgets before optional-key buckets and signed RATE. Define a remote-backoff cap, then fix scheduling/reset semantics. Verify FreeBSD broadcasts and the signed-root-delay interoperability policy on appropriate peers/hosts. |
| 8 | Finish control and monitor lifecycle/freshness | RA6X-033, RA6X-034, RA6X-052, RA6X-053 | Bind request lifetimes to cancellation and bounded server ownership. Keep processing/event age monotonic, using RA6X-059 and the stale-service policy in RA6X-044; preserve stable public schemas. |
| 9 | Complete diagnostics, durability, and remaining validation | RA6X-047, RA6X-048, RA6X-049, RA6X-058, RA6X-043, RA6X-036 | Record event/snapshot semantics explicitly, carry pulse records without hidden loss, and propagate serialization failures after input guards. Finalize durable replacement after RA6X-015/044; close fallback and TOML-presence gaps. Re-run consumers and deployment checks. |
