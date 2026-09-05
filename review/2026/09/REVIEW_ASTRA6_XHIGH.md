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

**Evidence:** `reject` calls `resetWindow()` after four rejections without setting `m.Invalidate`; `sequenceRestart` has the same omission. `SourceState.apply` clears validity only for `Invalidate=true`. Existing `TestVerification001ResetMustInvalidateSelection` reproduces the PPS remaining system source at octal reach 360 after four spikes and 376 after a counter restart, with an empty unstable window. Prior IDs: RF5X-001 and RF5X-026.

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

**Evidence:** `total := clampFreq(l.Freq + adj*1e6)` and `actual := (total-l.Freq)*1e-6` precede `Pending -= actual*dt`; the previous `l.applied` is not used. `TestVerification011ChargesIssuedTransient` expects 0.009902343750 s remaining after 39.0625 ppm ran for 1.5 s, but receives 0.009902572632 s. The ordinary test derives expected debit from the same new Pending expression and misses the defect. Prior ID: RF5X-011.

**Fix specification:** Track the successfully applied frequency and the accounting epoch, charge the actual held correction over the actual interval, then calculate the next word. Explicitly reconcile a new observation's Pending replacement with corrections already included in its timestamp; avoid double debit across Update and Tick. Define first-tick, zero/backward time, step, actuator-failure, clamp, and long-stall semantics. Preserve sign conventions, ±500 ppm total bound, configured phase-slew ceiling, and the absence of a base-only pulse between updates.

**Verification:** Make the issued-word probe pass and add an independent fake-actuator integral oracle for updates between irregular ticks, base changes, saturation, first tick, and step. Run full filtered closed-loop simulations after correcting the accounting.
