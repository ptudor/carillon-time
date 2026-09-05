# Astra6 exhaustive code review

Review date: 2026-09-05. Baseline: `8060697` on `main`. Analysis only; production source, tests, dependencies, and deployment configuration are unchanged.

This is an incremental review checkpoint. The final revision will include the complete coverage record, verification results, severity table, and dependency-aware fix order. Findings refer to the baseline's line numbers. Earlier findings are included when their underlying defects remain in the current tree; references identify prior reproductions rather than treating earlier fix claims as proof.

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

**Problem:** Tick claims to account for the transient that actually ran during elapsed time, but computes `actual` from the new Pending and new base. It then debits that new correction before issuing it. Even successive ordinary ticks disagree with the applied-word integral; intervening loop updates, changed bases, and delayed ticks make the discrepancy larger. Initial ticks also charge a nominal second before the first transient has run.

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

**Verification:** Make the takeover drift probe pass; cover zero feedback, stale-but-reachable sources, holdover, source changes, and an upstream still slewing. Also prove genuinely stable observations eventually permit hourly and shutdown writes.

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
**Location:** `internal/discipline/system.go:342–418`; `internal/engine/engine.go`, `handle` leap branch, `effectiveStatus`; `internal/source/ntp.go`, `hit`/`emit`.

**Problem:** Survivor leap majority is recomputed only after a nonignored loop update from the selected source. Warning changes in other survivors can be ignored while its filter winner stays unchanged. Separately, the only engine boundary reset is conditional on a LeapTable: the supported upstream-authoritative path does not reset samples/generation after the kernel's leap and may retain the warning until a later update.

**Evidence:** `TestAstra6LeapUpdatesWithoutSystemFeedback` makes two of three survivors announce insertion while the preferred system observation is unchanged; stored LI stays none. `s.leap = majorityLeap(sel.Survivors)` is below the early returns. Engine tests all synthesize a leapfile; no fileless transition test exists.

**Fix specification:** Publish accepted protocol metadata independently from new phase-feedback consumption and recompute leap consensus on relevant source changes. Track the pending UTC boundary for either authority and process it before measurements using RA6X-006/007. Clear warnings and reset boundary-spanning windows exactly once while preserving valid post-leap holdover/resync. Preserve leapfile authority when usable and configured; do not let PPS with no calendar information vote down upstream warnings.

**Verification:** Make the majority probe pass; test unchanged system winner, only non-system sources changing LI, loss during a pending leap, and insertion/deletion without a leapfile. Assert timely kernel flags, correct wire LI, and zero spurious steps.

## RA6X-023 — An expired authoritative leapfile can suppress valid upstream warnings

**Severity:** Medium — **Needs investigation: expired-authority service policy**  
**Location:** `internal/leap/leap.go`, `Indicator`; `internal/engine/engine.go`, `effectiveStatus`; `cmd/carillon/main.go`, `configurationWarnings`; `DESIGN.md:841–850`.

**Problem:** The design deliberately retains an expired table, but engine use of its authority never expires. Once the table no longer contains a newly announced leap, Indicator returns none and overrides a correct survivor majority; the wire and kernel continue as synchronized despite monitor health being unhealthy. Startup warnings alone do not reach operators whose daemon crosses expiry much later.

**Evidence:** `Indicator` never checks Expiry; `effectiveStatus` always assigns its result when LeapTable is nonnil. Expiry is exposed to monitoring and checked by startup warnings only. This is an explicit policy gap, not an accidental failure to reload a changed file.

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
