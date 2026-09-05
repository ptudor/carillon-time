# Verification of REVIEW_FABLE5_XHIGH.md

Verified 2026-09-05. **19 PASS, 17 FAIL** across all 36 finding IDs. FAIL
includes incomplete fixes, regressions, an incorrect added test, misleading
documentation, and the two retained skips; these are distinguished below.

The reviewed production tree is `79a77cc`. The original review examined
`85353ae`; its implementation log is [FIXES_FABLE5_XHIGH.md](FIXES_FABLE5_XHIGH.md).
This verification includes the subsequent drift-persistence and SETTLING
corrections in `5561238` and `751c23c`, rather than judging their superseded
implementations. The parallel session's takeover review is separate; its
files and commits were preserved.

PASS means the repair and relevant constraints passed the stated checks;
it does not claim unperformed hardware acceptance. Existing tests were read,
the changed production paths and callers were inspected against the original
commit, and independent assertions were added for uncovered boundaries.
No deployed daemon, clock, interface, sysctl, or service configuration was
changed. New work consists of this report and reproducible verification
artifacts. The two skipped repairs remain explicitly open for the reasons
below, not silently counted as fixed.

## Per-finding decisions

| Finding | Result | Verification and constraint assessment |
|---|---|---|
| RF5X-001 | **FAIL** | Slew tracking, genuine 10 ms spike rejection, recovery, and the closed-loop PPS simulation pass. New four-rejection reset clears the window without invalidating the selector's old estimate; an unlocked PPS remains system source. See detail below. |
| RF5X-002 | **FAIL** | The queued-old-generation and ordinary mid-exchange tests pass. Generation does not cover the step syscall interval, NMEA stamps after its arrival-clock read, and leap detection follows measurement application. Independent probes reproduce each gap. Existing measurement names and `Source.Reset()` signature remain intact. |
| RF5X-003 | **FAIL — retained skip on FreeBSD** | Linux broadcast/unicast control-message tests pass on Linux. FreeBSD still accepts a directed broadcast and constructs a broadcast reply source; reproduced on FreeBSD. Existing address checks, result labels, IPv6 handling, and other-platform no-op behavior remain intact. |
| RF5X-004 | **FAIL** | Normal pending-slew shutdown and suppression of a repeated refused frequency write pass. Shutdown before the first tick leaves 12.5 ppm in the actuator when the base is 17.3828125 ppm. This interaction with RF5X-011 escapes `Loop.Applied()`. Exit still neither steps nor changes synchronized status; drift encoding is unchanged. |
| RF5X-005 | **PASS** | Existing engine leap tests and `TestSystemResyncSkipsSettling` pass: first fresh update restores SYNCED, server mapping stays synchronized, and the kernel warning clears. Independent `TestVerification005HoldoverStillExpires` confirms expiry still works. Reset sweep remains. Full leap-boundary sample safety is still blocked by RF5X-002. |
| RF5X-006 | **PASS** | `TestSimSettling` and `TestSystemLeavesSettlingWithoutAFreshLoopUpdate` pass; the latter checks the correction added after the fix log. Progress can complete without reapplying a filter output. Configuration defaults to 1 and rejects values below 1. `lastUsedAt`, genuine-unsynced wire replies, and waitsync's SYNCED predicate remain. |
| RF5X-007 | **FAIL — diagnostic regression** | Wrong-edge rejection and agreeing-offset qualification pass. However, `disagrees_with` and `disagreement_seconds` survive loss of the numbering source, producing an obsolete disagreement while status is unqualified. The 0.4 s guard, preferred PPS combine bypass, and unqualified event path are preserved. |
| RF5X-008 | **PASS** | The required-key spoofed-unsigned flood test passes, including on Linux and FreeBSD: bad authentication does not consume the authenticated bucket. Non-required addresses still limit before verification; authenticated rate-limit results and KoD timing are retained. The subsequently added crypto-NAK response path has a separate regression under RF5X-035. |
| RF5X-009 | **FAIL — retry-floor edge** | The 256 MiB oversized-buffer test succeeds on both target platforms. But halving a non-power-of-two request can skip 65536 bytes entirely and fail without trying the promised minimum. Effective-size reporting and normal Linux behavior remain; FreeBSD check/config advice was added. |
| RF5X-010 | **PASS** | `TestSystemSecondStepResetsSettling` checks that every step resets the new evidence counters and three subsequent measurements are required when configured. The review contradicts RF5X-006 about preserving the default: the default change to 1 is explicitly required by RF5X-006, not an accidental violation. |
| RF5X-011 | **FAIL** | Jittered simulation and existing tick tests pass, but accounting uses the newly computed transient for time spent under the previously issued word. Independent issued-word probe fails even at a 1.5 s interval, below the disputed stall clamp. Step/slew clamps and nominal exponential convergence remain. |
| RF5X-012 | **FAIL — retained skip** | One-sample dispersion is still 0.0005 s rather than approximately 7.938 s. Applying only the specified empty-stage change reproduces both reported simulation failures. Jitter, staleness, and `Output.Samples` were left unchanged in that experiment. See the explicit deferral decision below. |
| RF5X-013 | **FAIL — added test breaks both target suites** | The zero-read error branch itself prevents the spin. But `TestReadTimeoutReportsEOF` fails on Linux (`POLLHUP`, 0x10) and FreeBSD (`POLLIN|POLLHUP`, 0x11), since existing hangup handling returns device-unavailable before `read`. The log's compile-only verification missed this. |
| RF5X-014 | **PASS** | Fake-clock daemon smoke test sends SIGHUP twice: control requests continue, exactly one warning is logged, and SIGTERM exits 0 within six seconds. SIGINT/SIGTERM subscription is unchanged. Choice is documented in DESIGN; the specifically requested rc.d/systemd explanatory comments were not added. |
| RF5X-015 | **PASS** | Fresh-engine omission test, control conversions, CLI tests, monitor snapshot and metric tests pass. All five specified optional timestamps use pointers; populated timestamps still serialize normally and field names are unchanged. `now` remains a required timestamp, appropriate for a live snapshot. |
| RF5X-016 | **PASS** | DESIGN §6.3 and the example now distinguish fixed cluster floor 3 from the minimum needed to choose a system source, and describe comparison with minimum peer jitter. No clustering behavior was changed for this repair. |
| RF5X-017 | **FAIL — server-change regression** | A noisy update and reach recovery honor RATE; demands above configured maximum work. However, the raised maximum overwrites `cfg.PollMax` and survives re-resolution to a different server. Independent probe: configured 6 becomes 13 and stays 13. DENY/RSTR behavior remains. |
| RF5X-018 | **PASS** | Connected UDP, `Write`, ICMP-unreachable handling, nonce/origin checks, endpoint validation, kernel timestamp parsing, and fresh ephemeral sockets are present. Query/source tests and the quick-unreachable test pass on the development host; platform results are recorded below. |
| RF5X-019 | **FAIL — valid drift file deleted** | Stale/fresh temporary cleanup test passes for the conventional filename. An old configured file named `.drift-calibrated` is itself removed by startup cleanup. Reproduced through `Engine.New`. The atomic write implementation remains unchanged, but valid configured state is lost. |
| RF5X-020 | **PASS** | Median precision tests pass for 1 ns, 30 ns, 1 µs, 1 ms, frozen clocks, sparse 1 ns outliers, and the read budget. `PrecisionFromSeconds` and the [-30, -6] clamp are unchanged. No hardware precision value is inferred from the fake-clock tests. |
| RF5X-021 | **FAIL — explicit zero keys still ignored** | Nonzero GPS-only keys are rejected and FreeBSD selects the separate PPS tty for its sysctl check. All six explicitly configured zero/empty GPS-only keys still validate on bare PPS. Existing valid GPS configurations and pin checking remain intact. Sysctl path selection is inspected; no hardware PPS pin was reconfigured. |
| RF5X-022 | **PASS** | `ParseServerAddress` is the sole parser; constructor and query callers supply its host/port. The surviving union table, constructor validation, and query tests pass. Parser body/accepted configuration syntax are unchanged by the export; duplicate parser is gone. |
| RF5X-023 | **PASS** | Version and last-request publication now follow authorization and limiting. The dedicated histogram test and authentication/counter partition tests pass on Linux and FreeBSD. Existing metric names remain unchanged. NAKs remain bad_auth, not accepted requests. |
| RF5X-024 | **PASS** | Prefix-sharing test passes: 20 IPv6 addresses share one /64 bucket, another /64 gets another, and IPv4 addresses remain separate. Configured prefix bounds/default, metric help, LRU bound/expiry, mapped IPv4 handling, and existing KoD interval are consistent. |
| RF5X-025 | **PASS** | `TestLoopJitterSeedAndStep` passes: first 100 ms offset seeds approximately 35.36 ms jitter, neither a step nor its next update corrupts jitter, ordinary averaging resumes. Weight 1/8 and precision floor remain. |
| RF5X-026 | **FAIL — selector not invalidated** | Backward sequence restart no longer creates billions of gaps; existing restart and normal-gap tests pass. But its new reset leaves the old PPS estimate selected at reach 376 despite an empty, unlocked window. Independent restart branch reproduces the RF5X-001 invalidation defect. |
| RF5X-027 | **PASS** | Unit diff contains only removal of broad `/run` write access and explanation; `RuntimeDirectory=carillon`, mode 0750, state path, and control path remain. Repository deployment record reports the unit already deployed successfully. This verification did not restart systemd services. |
| RF5X-028 | **PASS** | No `example.net`, `example.com`, or `example.org` placeholders remain in deploy documents or DESIGN. Query examples use `server.invalid`, including authenticated forms. Historical review quotations were retained. |
| RF5X-029 | **PASS** | UTC rotation/layout, retention with 1 day, default unlimited retention, deduplication, server-family rows, and disabled-server tests pass. Diff confirms TSV headers, columns, formatting, and async queue behavior remain unchanged. Invalid calendar directories/symlink entries are skipped by pruning; year/month pruning removes only empty directories. |
| RF5X-030 | **PASS** | The one-line `errors.Is(err, io.EOF)` repair is correct. Independent `TestVerification030WrappedEOFCompletesResponse` exercises an actual `*net.OpError` wrapping EOF and returns the complete unterminated JSON response. Size-limit behavior is unchanged. |
| RF5X-031 | **FAIL — documentation still overstates precedence** | New wording says a qualified PPS is system source whether or not preferred. That omits a surviving preferred NTP source, which still wins. `TestVerification031OtherPreferStillWins` passes and supplies the counterexample. No behavior change was needed; the prose needs that qualification. |
| RF5X-032 | **FAIL — queued data resurrects stopped source** | Basic restart/backoff test passes and Source interface is unchanged. A source-stop request can overtake its queued measurement; that measurement restores SYNCED/system source/reach 377 while Info says reach 0 and `port vanished`. The source is still down. See detail below. |
| RF5X-033 | **PASS** | Auxiliary deadline/name test and unread-waitsync test pass. Independent `net.Pipe` test forces a genuinely blocked write and confirms the new 5 s reply deadline releases it. Engine restore/drift work still precedes auxiliary wait. This bounds auxiliaries, not an uncooperative Source.Run or blocked drift filesystem. |
| RF5X-034 | **PASS** | Panic-refusal, initial-state, and holdover tests pass. Refids are now PANC, INIT, and HOLD for their respective reasons; their wire unsynchronized stratum/LI handling remains unchanged. |
| RF5X-035 | **FAIL — unlimited unauthenticated NAK replies** | Unknown-key/bad-digest NAK shape and reply-size fuzzing pass. In a require_key prefix, the new NAK branch returns before any limiter: 100 bad requests at one instant produce 100 replies despite burst=8. Authenticated traffic still works, but unauthenticated response bounds are bypassed. |
| RF5X-036 | **FAIL — running status constraint violated** | Ordinary startup noise is suppressed and established preferred-source loss still reports. A preferred source that never answers remains invisible to prefer_lost indefinitely, even after 900 s of healthy fallback service. New AND gating violates the required running semantics; the requested early-loss WARN downgrade is also absent. |

## Reproduced failures and surrounding-code issues

### RF5X-001 / RF5X-026: resetting the PPS window does not revoke its estimate

`PPS.reject` and `sequenceRestart` call `resetWindow`, setting `stable=false`
and emptying the window. Their returned measurement has neither `Valid` nor
`Invalidate`. `SourceState.apply` therefore retains its old valid estimate;
reach is still nonzero. In the independent real-PPS-to-System test the PPS
stays system source with reach **360** after four spikes and **376** after
a sequence restart, although its window is empty. Previously those new
re-prime paths did not exist. A reset that loses lock must communicate
invalidation immediately while keeping the specified reach/counter semantics.

The original slew-cascade repair is otherwise effective, and the original
1 µs floor, window sigma/hysteresis helper, accepted-pulse callback, emitted
field definitions, and counter names are retained. The failure is in carrying
the new reset state across the source/selector boundary.

### RF5X-002: the generation boundary has uncovered intervals

Three deterministic probes fail:

1. `TestVerification002GenerationCoversTheStep`: a source starts inside
   `Clock.Step`, after `gen.Add(1)` but before the fake clock changes. It
   captures a +2 s pre-step offset under generation 2; generation 2 is still
   current after Step and Reset complete, so `Engine.stale` accepts it.
   The boundary must bracket the actuator operation, with an explicit
   in-progress state or equivalent synchronization.
2. `TestVerification002NMEAGenerationMatchesArrival`: `NMEA.Run` reads the
   arrival wall time before `consume` reads the generation at `$`. A step
   between those reads lets an old timestamp enter the new-generation window.
   The probe records `Received=1`, `offsets=[0]`, generation 2 instead of
   discarding it. The eventual Reset does not make this admission correct.
3. `TestVerification002LeapCheckedBeforeQueuedMeasurement`: the Run receive
   arm checks the old generation and evaluates `sys.Update(m)` before
   `handle` notices the wall-clock leap boundary. A queued pre-leap sample
   produces an actual **[1s]** fake-clock step before the reset. Leap detection
   must run before consuming queued measurements/issuing correction actions.

Ordinary generation-change-during-exchange and engine queue rejection tests
do not cover these intervals. NMEA has no independent generation tests in the
normal suite. Also, the defensive second-step gate counts valid filter
outputs rather than raw post-step filter samples, which can delay a justified
second step when a previous low-delay sample still wins; it is not exactly
the requested `Filter.Len() >= 2` criterion.

### RF5X-004 / RF5X-011: shutdown tracking and elapsed-slew accounting

`Engine.Run` writes the initial frequency directly, without setting
`Loop.haveApply`. RF5X-011 removed frequency actions from Update. Three quick
updates can therefore change the base before any tick, then shutdown skips
restoration because `Applied()` says nothing was issued. The probe leaves
**12.5 ppm**, with final base **17.3828125 ppm**. Track successful actuator
writes at the engine boundary, including initialization, or otherwise account
for this pre-first-tick case.

Separately, `Loop.Tick` describes its debit as the previous transient, but
computes `actual` from the *new* Pending/base before applying it. With a
39.0625 ppm previously issued word over 1.5 s, the probe expects remaining
phase **0.009902343750 s** and gets **0.009902572632 s**. The existing tick
test computes its expected value from the same new Pending/tau expression,
so it cannot catch this accounting error. Updates that replace Pending and
change the base between ticks make the mismatch more consequential.

The review itself inconsistently asks both to charge 3 s in full and to
replace intervals greater than 2 s with 1 s. The fix log openly chose the
latter. That conflict is not the reason for FAIL: the issued-word probe fails
within the unambiguous interval range. Stall logging was also not implemented;
`loop.tsv` records loop updates, not every ticker event, so the fix log's
suggested observation method is insufficient to identify individual stalls.

### RF5X-007 / RF5X-036: misleading source diagnostics

Disagreement fields are only assigned inside the branch with usable numbering
sources. After the sole numbering source loses reach, Select sets the PPS to
unqualified but retains `disagrees_with="ntp"`, delta 0.12. Clear those fields
at each selection before recomputing current agreement.

`reportPreferLost` requires both `preferSeen` and some survivor. A source
that was down before startup never sets `preferSeen`, so the status remains
healthy with respect to preference forever despite fallback service. The
review allowed suppression until the preferred source was seen **or** some
source became a survivor. Preserve suppression before service exists, then
report actual preference loss even if that source has never answered.

### RF5X-009: halving does not necessarily visit the minimum

For a valid configured request of **100000** bytes, `setReadBuffer` tries
100000; its next loop value is 50000, below `minRecvBuffer=65536`, so it exits.
A kernel willing to accept 65536 but refusing 100000 is never offered the
minimum. This is a direct code-path proof; such a specially limited kernel
was not synthesized. The target tests use 256 MiB, a power of two, and miss
this case. Clamp the next attempt to the floor, make that final attempt once,
and only then return the refusal.

### RF5X-013: target-only test regression

The added test's closed pipe follows the existing `POLLHUP` error path before
the newly added zero-byte read handling:

```
Linux:   serial: poll pipe: device unavailable (events 0x10), want an io.EOF
FreeBSD: serial: poll pipe: device unavailable (events 0x11), want an io.EOF
```

Both errors already drive NMEA's reopen path; neither causes the reported
spin. Repair the test to accept a non-timeout hangup error for a closed pipe,
and exercise the zero-read branch with a suitable read seam if specifically
requiring `errors.Is(err, io.EOF)`. This is a verification-test failure, not
evidence that the EOF production fix causes a spin. USB detach/reopen was not
physically exercised.

### RF5X-017 / RF5X-019 / RF5X-021: configuration edge cases

- RATE handling modifies `cfg.PollMax` itself. Changing the resolved address
  resets only `kodMinPoll`; the old peer's effective maximum becomes the new
  configuration. Keep the configured maximum immutable and the peer's
  negotiated bound separately; clear the latter on address change.
- Drift sweep matches every `.drift-*` path, including the actual configured
  drift file. `Engine.New` initially reads `.drift-calibrated`, then deletes it
  if older than a minute. Exclude the configured file explicitly, not just
  the conventional basename used by the existing test.
- GPS-only validation tests values rather than TOML presence. `baud=0`,
  `pps=""`, `pps_edge=""`, `pps_offset=0.0`, `nmea_offset=0.0`, and
  `sentences=[]` are all silently accepted on bare PPS. Track explicit key
  presence during decoding to implement the strict promise; defaults alone
  cannot distinguish absent from explicitly empty.

### RF5X-031 / RF5X-032 / RF5X-035: precedence, lifecycle, response bounds

The RF5X-031 sentence in DESIGN §6.3 needs “when no other preferred source
survives.” A preferred NTP source remains system source while a qualified,
unpreferred PPS is a survivor. The preceding paragraph already describes
that precedence correctly. This is documentation drift, not a new selector
behavior defect.

For RF5X-032, measurements and source lifecycle requests have different
channels. A stop request can be selected before a measurement sent earlier
by that source. `sourceStopped` sets reach 0, but the later measurement
reselects the stopped source and reports **SYNCED / reach 377**, while the
Info overlay still says **reach 0 / port vanished**. Suppress measurements
from stopped runs, including across restart, using ordered lifecycle delivery
or a per-run identity. Re-check the related error path: `sourceStopped`
currently only logs an error returned by `handle`, even though actuator
failures from the normal measurement arm are fatal. That new lifecycle path
must propagate fatal actuator errors as well.

For RF5X-035, a valid-looking MAC with an unknown key under a require_key
address gets its crypto-NAK before `limiter.allow`. The probe receives 100
52-byte replies for 100 requests at one instant with burst 8; all 100 are
bad_auth and none are rate_limited. The byte-length invariant still holds,
but the configured response-rate bound does not. Apply a separate bounded
unauthenticated/NAK reply budget without consuming authenticated-peer tokens,
and preserve a single terminal outcome counter per datagram.

## Decisions on the skipped items

### RF5X-003: retain the FreeBSD skip, with a concrete replacement design needed

The proposed `MSG_BCAST`/`MSG_MCAST` fix cannot be implemented as written.
Checked the actual FreeBSD 15.0-RELEASE-p12 `/usr/include/sys/socket.h`:
those flags are absent; 0x100 is `MSG_EOF`, and 0x200 is unused. Defining the
OpenBSD/NetBSD numbers locally would silently implement the wrong test.
`IP_RECVDSTADDR` supplies only the destination and cannot perform Linux's
destination/local-address comparison.

The alternative is feasible, and lack of authorization is **not** the reason
to defer it. It needs live interface/broadcast information, preferably tied
to receive-interface metadata and updated on interface/address changes.
A static startup census can miss a newly configured subnet's broadcast;
rejecting every address absent from that stale census instead can reject
legitimate unicast on a newly configured address. Guessing from the final
octet also breaks /23, /31, /32, aliases, and other valid unicast layouts.

This verification retains the skip because a correct replacement needs that
cache/refresh/failure policy and multi-address tests, rather than an untested
substitute that violates the review's explicit unicast-source constraint.
The risk is established, not hypothetical: the FreeBSD synthetic ancillary
test returns destination **192.168.1.255**, `martian=false`, and a reply
IP_SENDSRCADDR containing that address. No broadcast packets were sent to
the deployed NTP service. The Linux half is verified; the FreeBSD half stays
**FAIL/open**. The comments in `listener.go` and `pktinfo_freebsd.go` claiming
receive flags catch this case should also be corrected when implementing it.

### RF5X-012: retain the dispersion skip; the reported conflict is reproducible

The underreported dispersion is real. The independent one-/four-sample
assertion fails. [RFC 5905 §10](https://www.rfc-editor.org/rfc/rfc5905#section-10)
initializes all eight stages with dummy MAXDISP tuples and gives approximately
7.94 s after one good sample and 0.94 s after four. Its text also has a
stale-epoch guard; it does not justify indiscriminately integrating an old
filter output again.

`probe_rfc_dispersion.py` applies only the review's literal empty-stage
dispersion change through an overlay, preserving jitter, the staleness guard,
and `Output.Samples`. Results against the current tree:

```
TestSimConvergesFromUnknownFrequency  PASS (49.99 ppm, sixth-hour RMS 27.0 us)
TestSimFalseticker                   FAIL (steps=2)
TestSystemRemoveSource               FAIL (state unsynced)
```

The fix log's claim of incompatible startup/candidate behavior is therefore
substantiated. Its “permanently” wording overstates the simple frozen-stage
argument: filter ring replacement can eventually choose another sample.
Nevertheless the observed simulation failures are enough to reject that
literal patch. Keep this skipped until candidate admission and refreshed
filter-quality publication are designed together while ensuring that old
offsets cannot be double-integrated. Do not label the current dispersion
RFC-compliant or weaken these simulations just to get a passing suite.

## Executed checks and reproduction

- Development host: darwin/arm64, Go 1.25 module floor. Existing
  `go test -race ./...` and `go vet ./...` pass. The normal suite does not
  include the independent failing probes.
- Cross-platform vet passes for linux/arm64 and freebsd/arm64 with
  `CGO_ENABLED=0`. Actual amd64 test binaries compile for both targets.
- `gummi`: Linux 6.19.14-200.fc43.x86_64. Server and configuration package
  suites pass. Serial package fails only its added closed-pipe EOF test.
- `twocom`: FreeBSD 15.0-RELEASE-p12 amd64. Configuration suite passes;
  serial fails the same added test. Server loopback round-trip and end-to-end
  tests time out. Both also fail with an independently built **85353ae**
  server test binary on this same host, so they are not attributed to these
  fixes. FreeBSD handler tests and oversized-buffer test pass; the new
  synthetic broadcast probe fails as described above.
- Both targets also pass the selected NTP client tests:
  `TestUnreachablePortFailsFast` (no platform skip),
  `TestGenerationBumpDiscardsReply`, `TestGenerationStampedOnMeasurements`,
  `TestKissRATEPollIsANewMinimum`, and `TestKissRATEBeyondPollMax`.
- Reply-size fuzzing passes: `FuzzReplyNeverAmplifies`, requested 5 s,
  719637 executions, approximately 6.3 s including finalization.
- `probe_signals.py`: PASS. Real main/signal/control/shutdown wiring is used,
  with exactly two overlay substitutions: `clock.New` becomes `clock.Fake`,
  and the unrelated UDP/123 conflict probe is bypassed. Two HUPs keep serving
  control requests with one warning; TERM exits 0 within six seconds.
- `TestVerification033BlockedReplyHasDeadline`: PASS at 5.00 s using an
  unread `net.Pipe`; this forces the actual write to block.
- `TestVerification030WrappedEOFCompletesResponse`: PASS with `*net.OpError`
  wrapping EOF.
- Final independent probe run with `-race` reproduces the documented
  invariant failures; no data race is reported. These are logical failures,
  not failures inferred solely from inspection of their tests.

Independent probes are stored as `.go.txt` files in
[verification_fable5_xhigh](verification_fable5_xhigh/), with their expected
invariants asserted normally. The runner uses Go's overlay support to add
them to the appropriate internal packages without editing the parallel
session's worktree. **A nonzero exit is expected for unresolved findings.**
This is not a set of tests that passes by asserting the bugs still exist;
the explicit preferred-NTP documentation counterexample is identified as such.

Run from the repository root:

```sh
python3 review/2026/09/verification_fable5_xhigh/run_probes.py -race
python3 review/2026/09/verification_fable5_xhigh/probe_rfc_dispersion.py
python3 review/2026/09/verification_fable5_xhigh/probe_signals.py

CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 \
  python3 review/2026/09/verification_fable5_xhigh/run_probes.py \
  -c -o /tmp/server-probes.test ./internal/server
# On FreeBSD, with the resulting binary:
/tmp/server-probes.test -test.run TestVerification003 -test.v
```

Hardware-only checks still outstanding: live PPS wrong-edge/relock testing,
USB GPS detach/reopen, a captured broadcast wire test, and an actuator/ntptime
shutdown check. This report does not infer those results from compilation or
past deployment prose. The directly reproduced defects above must be resolved
before treating the fix log's overall “fixed” count as verified.
