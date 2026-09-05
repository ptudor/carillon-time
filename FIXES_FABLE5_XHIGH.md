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

## RF5X-003 — Directed-broadcast requests answered — PARTLY FIXED (Linux), FreeBSD half SKIPPED

**Changed (items 2–5).** `destination()` now returns a third result, `martian`.
On Linux it compares the two halves of `IP_PKTINFO`: `ipi_addr` is the header
destination and `ipi_spec_dst` the local address, equal only for a unicast
request; a directed broadcast has the broadcast address in the first and the
interface address in the second, and is now dropped and counted `martian`
before decoding. The reply's control message is built from `ipi_spec_dst`, not
`ipi_addr`, so a non-local source can never be put on a reply. IPv6 has no
broadcast and multicast was already covered. The existing `martianDestination`
address checks are untouched, no new `result` label was added, and the reply
source-address selection for unicast on multi-homed hosts is unchanged. A
per-OS `martianReceiveFlags(flags)` hook was added next to `enablePacketInfo`
and is wired into `serve()`.

**Item 1 (FreeBSD) SKIPPED: the specification names constants FreeBSD does not
have.** The fix says to treat `flags & (unix.MSG_BCAST | unix.MSG_MCAST)` as
martian. `MSG_BCAST` and `MSG_MCAST` are NetBSD/OpenBSD constants (`0x100` /
`0x200`); `golang.org/x/sys/unix` defines them in `zerrors_netbsd_*.go` and
`zerrors_openbsd_*.go` and in **no** FreeBSD file — x/sys generates these
directly from the system headers, so FreeBSD does not define them and does not
report broadcast delivery in `msg_flags`. `GOOS=freebsd go build` fails with
`undefined: unix.MSG_BCAST`. FreeBSD's `IP_RECVDSTADDR` gives only the header
destination, with no local-address companion to compare against, so the Linux
technique does not carry over either. Recognising a *directed* broadcast there
needs a different mechanism — enumerating the interfaces' broadcast addresses
at listen time, say — which is a design choice this finding does not
authorise. `martianReceiveFlags` on FreeBSD is an honest no-op with a comment
saying so. **This leaves the more serious half of the finding open**: the
review's own analysis is that on FreeBSD the reply actually leaves the host
with a broadcast source, whereas on Linux the send merely fails.

**Files.** `internal/server/pktinfo_linux.go`, `pktinfo_freebsd.go`,
`pktinfo_other.go`, `listener.go`, `pktinfo_linux_test.go` (new),
`DESIGN.md` §7.2.

**Verification.** PARTIAL. `TestDestinationRejectsDirectedBroadcast` and
`TestDestinationUnicastRepliesFromTheLocalAddress` build a hand-made
`IP_PKTINFO` control message with `Addr != Spec_dst` and assert the martian
result, and that a unicast reply's source comes from `Spec_dst`. They are
`//go:build linux` and **compile** here (`GOOS=linux go test -c` passes) but
cannot be **run** on this darwin host — they need `gummi`. `go vet` passes for
darwin, linux and freebsd. The wire proof (one 48-byte mode-3 datagram to the
subnet broadcast, `carillonctl serverstats` showing `martian`, `tcpdump`
showing no reply) remains a target-host step.

## RF5X-009 — recv_buffer above kern.ipc.maxsockbuf aborts FreeBSD startup — FIXED

**Changed.** `listenOne` calls a new `setReadBuffer`, which asks for the
configured size and, on `ENOBUFS` or `EINVAL`, halves the request until the
kernel accepts it, down to a 64 KB floor (`minRecvBuffer`, mirroring
`config.MinRecvBuffer`; `config` imports `server`, so it cannot be imported
back). A reduced grant logs once at WARN naming the sysctl to raise via a new
`server.RecvBufferSysctl()`; only a refusal at the floor is fatal. `-check`
gained a matching warning whenever `recv_buffer` is set on FreeBSD. The
effective-size read-back and logging, and the Linux behaviour, are unchanged.

**Files.** `internal/server/listener.go`, `internal/server/listener_test.go`,
`internal/config/config.go` (cross-reference comment), `cmd/carillon/main.go`,
`DESIGN.md` §7.1, `deploy/carillon.toml.example`.

**Verification.** PASS on darwin. `TestListenAcceptsAnOversizedReceiveBuffer`
asks for 256 MB on loopback and requires `Listen` to succeed with a positive
effective buffer. The FreeBSD-specific path (an actual `ENOBUFS` from
`sbreserve_locked`) cannot be exercised here — `twocom` with
`recv_buffer = 4194304` and default sysctls is the remaining step, per the
review.

## RF5X-008 — Rate limiting before MAC verification starves the authenticated association — FIXED

**Changed.** For a source address inside a `require_key` prefix, `Handle` now
verifies the MAC before consulting the limiter, and verified requests get
their own token bucket keyed by `(prefix, key id)` rather than sharing the
address's bucket with anyone able to forge it. An unverified request from such
an address is dropped as `bad_auth` before the limiter is touched, so a
spoofed flood costs the attacker one CMAC per packet and cannot reach the
authenticated bucket at all. Addresses outside `require_key` keep the original
order (limiter, then authentication), which caps the extra CMAC work to the
handful of `/32`s `require_key` names. The reply-length invariant, the
`bad_auth`/`rate_limited` meanings and the KoD throttle are unchanged.

**Files.** `internal/server/responder.go`, `internal/server/ratelimit.go`,
`internal/server/responder_test.go`, `DESIGN.md` §7.2.

**Verification.** PASS. `TestAuthenticatedPeerSurvivesASpoofedFlood` sends 100
unsigned requests from a `require_key` `/32` in one second and asserts all 100
are `bad_auth` and none `rate_limited`, then that the peer's correctly signed
poll is still served — the review's stated test.

## RF5X-023 — Version histogram and last_request recorded before the ACL checks — FIXED

**Changed.** `c.versions[...]` and `storeLatest(&c.lastRequest, ...)` moved
from just after the ACL to just before the reply is built, after the limiter
and authentication. Metric names are unchanged; the help text for
`carillon_server_client_version_total` and `DESIGN.md` §10.4 now say what it
counts.

**Files.** `internal/server/responder.go`, `internal/server/responder_test.go`,
`internal/monitor/metrics.go`, `DESIGN.md` §7.2, §10.4.

**Verification.** PASS. `TestVersionHistogramCountsOnlyAcceptedRequests` sends
a burst past the token bucket and asserts the histogram and `last_request` do
not move while the requests are being rate-limited.

## RF5X-024 — IPv6 rate limiting per /128 — FIXED

**Changed.** The rate-limit table is keyed by a `bucketKey{addr, keyID}` whose
address is masked to `rate_limit_v6_prefix` for IPv6 (new `[serve]` knob,
default 64, validated 32..128) and left whole for IPv4. The clients gauge
therefore counts /64s for v6, which the metric help and `DESIGN.md` §10.4 now
say. The IPv4 behaviour and the KoD throttle are unchanged.

**Files.** `internal/server/ratelimit.go`, `internal/server/responder.go`,
`internal/config/config.go`, `cmd/carillon/main.go`,
`deploy/carillon.toml.example`, `internal/monitor/metrics.go`, `DESIGN.md`
§7.2, §10.4, `internal/server/responder_test.go`.

**Verification.** PASS. `TestRateLimiterKeysIPv6ByPrefix` puts 20 addresses
from one `/64` through the limiter and asserts they share one bucket, that a
second `/64` is a second bucket, that two IPv4 addresses are still two
buckets, and that an authenticated request gets its own.

## RF5X-035 — No crypto-NAK for an unknown key id — FIXED

**Changed.** A request carrying a MAC that cannot be authenticated — an
unknown key id, or a digest that does not verify — is now answered with a
crypto-NAK: the 48-byte header plus a four-byte zero key id, counted
`bad_auth`, unauthenticated. Previously an unknown key id got a plain 48-byte
reply and a bad digest got silence. A *correctly* signed request under a key
the `require_key` rule does not permit, and a request with no trailer at all,
keep the existing drop: neither is a key the server does not know, so a NAK
would tell the client nothing its own configuration does not. `ntp.CryptoNAKSize`
was exported for the reply builder and the tests.

**Files.** `internal/ntp/packet.go`, `internal/server/responder.go`,
`internal/server/responder_test.go`, `DESIGN.md` §7.2.

**Verification.** PASS. `TestAuthentication` asserts a 52-byte reply that
decodes as `IsCryptoNAK()` for both a corrupted digest and an unknown key id,
and that it is never longer than the request. `FuzzReplyNeverAmplifies` gained
the assertion the review asks for — a NAK is only ever sent for a request of
at least 52 bytes — and ran clean for 21 s / 2.8 M executions.

## RF5X-007 — A qualified prefer PPS never enters the intersection; a wrong-edge PPS is undetected — FIXED

**Changed.** PPS qualification now requires *agreement*, not just the presence
of a numbering source near zero. `Select` collects the surviving numbering
sources that pass the existing `|θ| < 0.4 s` test, and each PPS source must
satisfy `|θ_n − θ_pps| ≤ λ_n + max(4·ψ_n, 1 ms)` against at least one of them
(`ppsAgreement`). A locked, stable PPS that fails is marked
`StatusFalseticker` rather than `StatusUnqualified`, which makes `reselect`
emit `EventFalseticker` and the engine log at WARN once; the transition
Unqualified→Falseticker no longer emits the misleading `EventPPSQualified`.
`SourceState`/`SourceStatus` carry `DisagreesWith`/`Disagreement`, surfaced as
`disagrees_with`/`disagreement_seconds` on the control socket and as an
AGREEMENT column in `carillonctl refclock`, and a disagreeing pulse reads as
not qualified and not locked. The 0.4 s guard band, the PPS bypass of the
combine step once qualified, and the no-numbering-source `EventPPSUnqualified`
path are unchanged.

**Files.** `internal/discipline/select.go`, `internal/discipline/system.go`,
`internal/discipline/select_test.go`, `internal/control/protocol.go`,
`cmd/carillonctl/main.go`, `DESIGN.md` §5.2.

**Verification.** PASS. `TestSelectPPSMustAgreeWithItsNumberingSource` is the
review's stated test: NTP at θ = 0.000 with a PPS at θ = 0.120 and 1 µs jitter
gives `StatusFalseticker`, the NTP source as system source, `PreferLost` true,
and `disagrees with ntp by 0.120`; NTP at θ = 0.100 ± 2 ms with a PPS at
θ = 0.101 stays qualified and drives the clock. The existing
`TestSelectPPSQualification` was corrected — its NTP source was 0.1 s from its
PPS, which is exactly the wrong-edge signature. The hardware check (swap
`edge`, confirm the daemon logs the disagreement and does not go to stratum 1)
remains a target-host step.

## RF5X-011 — Loop.Tick assumes exactly one second; Update drops the transient — FIXED

**Changed.** `Loop.Tick(now)` now computes the real elapsed time since the
previous tick and debits `Pending -= actual·dt` / `slewed += actual·dt`
instead of assuming one second. `dt` is clamped to `[0, maxTickInterval]`
(2 s); a longer gap is treated as one second, because over-debiting the phase
would leave a correction that was never applied believed done. The first tick
of a run is charged one second. `Loop.Update` no longer emits a frequency
action at all — writing the base alone removed the transient the last tick
applied and paused the slew for up to a second — so the following tick carries
the new base plus the transient for the new pending phase. The ±`MaxSlewPPM`
and ±500 ppm clamps are unchanged, and `TestLoopTickSlew`'s exponential
approach still holds with `dt = 1`.

`internal/discipline` still does no logging (hard rule 6), so the stall the
review asks to be logged is only clamped here; the operator sees it as a
tick-to-tick gap in `loop.tsv`.

**Note on the verification.** The fix specification says to clamp `dt` to
`[0, 2 s]` and treat anything larger as one second, but its verification asks
for a 3 s tick to debit `3 × adj`. Those cannot both hold. The clamp is the
normative half, so the test uses a 2 s late tick (charged in full), a gap
beyond the clamp (charged one second), and a zero-length tick (charged
nothing).

**Files.** `internal/discipline/loop.go`, `loop_test.go`, `sim_test.go`,
`DESIGN.md` §6.4.

**Verification.** PASS. `TestLoopTickChargesRealElapsedTime` covers the three
cases above. `TestSimTolerantOfTickJitter` runs the five-hour convergence
simulation with every simulated second varying by ±200 ms (true time and the
loop's accounting both use the real interval) and asserts the steady-state RMS
is unchanged: 27.0 µs with an even ticker, 28.1 µs with the jitter.

## RF5X-025 — First-update jitter equals the whole initial offset — FIXED

**Changed.** The first loop update seeds the jitter estimate as
`max(precision, |θ|/√8)` — the same exponential average run from the precision
floor, as ntpd does — instead of taking the whole offset. The update following
a step no longer folds in its difference at all, because `step()` zeroes
`lastOffset` and the difference then describes nothing. The averaging constant
is unchanged.

**Files.** `internal/discipline/loop.go`, `loop_test.go`, `DESIGN.md` §6.4.

**Verification.** PASS. `TestLoopJitterSeedAndStep` asserts a 100 ms initial
offset gives ≈35 ms of jitter (0.100/√8), that a step leaves the estimate
untouched, that the update after it does too, and that ordinary updates still
move it.

## RF5X-013 — NMEA reader would spin on a zero-byte read — FIXED

**Changed.** `serial.ReadTimeout` reports `n == 0 && err == nil` as an
`io.EOF` wrapped in a `serial:` error, so `NMEA.Run` takes its reopen path
instead of looping straight back into `poll(2)`, finding the descriptor
readable-at-EOF, and spinning at 100 % CPU with nothing in the log.

**Files.** `internal/serial/read_unix.go`,
`internal/serial/read_unix_test.go` (new).

**Verification.** COMPILES, not run here. `TestReadTimeoutReportsEOF` uses
`os.Pipe()` with the write end closed and asserts the result is an `io.EOF`
and not `ErrTimeout`. It is `//go:build linux || freebsd` (the file it tests
is), so it cannot run on this darwin host; `GOOS=linux go test -c` and
`GOOS=freebsd go vet` pass. The USB-unplug check remains a target-host step.

## RF5X-014 — SIGHUP terminates the daemon — FIXED

**Changed.** `main` installs a handler that ignores `SIGHUP` and logs once at
WARN saying carillon has no reload and a restart is what picks up a changed
configuration. Go's default for an unhandled signal is immediate termination:
no drift-file write, no base frequency restored (RF5X-004), sockets closed by
the OS. `SIGTERM`/`SIGINT` handling is unchanged.

**Files.** `cmd/carillon/main.go`, `DESIGN.md` §12.

**Verification.** PASS (build and suite). The signal itself is not exercised
by a unit test — sending a real signal to the test process would be a hard
rule 1 hazard — the handler is three lines and `signal.Ignore` semantics are
the standard library's.

## RF5X-015 — omitempty on time.Time emits year-1 timestamps — FIXED

**Changed.** `Refclock.LastPulse`, `Refclock.LastSentence`,
`Tracking.LeapExpiry`, `Tracking.RefTime` and `Source.LastRx` are `*time.Time`
filled through the existing `optionalTime` helper, matching `LastRequest`/
`LastServed`. `reftime` gained `omitempty` so it disappears rather than
serialising `null`. Field names are unchanged. The Prometheus collector uses
`optionalTimestampSeconds` for the two it reads, and `carillonctl` checks for
nil instead of `IsZero()`.

**Files.** `internal/control/protocol.go`, `internal/monitor/metrics.go`,
`cmd/carillonctl/main.go`, `internal/control/control_test.go`,
`internal/monitor/model_test.go`.

**Verification.** PASS. `TestFreshEngineOmitsNeverHappenedTimestamps` marshals
a fresh engine's tracking/sources/refclocks, decodes into `map[string]any`,
asserts `reftime` and `leapfile_expires` are absent, and fails on any
`0001-01-01` anywhere in the payload. `TestSnapshotOf` asserts the same two
are nil.

## RF5X-016 — Cluster floor is 3, not min_survivors — FIXED (documentation)

**Changed.** `DESIGN.md` §6.3 now says clustering keeps at least **3**
(RFC 5905 `NMIN`, a fixed floor and not a knob) and that `min_survivors` is
the number of survivors required before a system source is declared, with the
warning not to set it above the number of sources configured. The discard rule
is described as comparing against the smallest filter jitter among the
survivors, which is what the code and the RFC do. The example config's comment
was rewritten to match. No code change, as specified.

**Files.** `DESIGN.md` §6.3, `deploy/carillon.toml.example`.

**Verification.** Doc review.

## RF5X-019 — Crash between CreateTemp and Rename leaves .drift-* files — FIXED

**Changed.** `engine.New` calls `sweepDriftTemps`, which globs
`.drift-*` beside the drift file and removes matches older than a minute,
logging each at DEBUG. A younger temporary — one another instance may be
writing right now — is left alone. The atomic write itself is untouched.

**Files.** `internal/engine/engine.go`, `internal/engine/engine_test.go`.

**Verification.** PASS. `TestEngineSweepsStaleDriftTemporaries` plants an
hour-old temporary and a fresh one and asserts only the stale one goes, and
that the drift file itself survives.

## RF5X-026 — A backwards PPS sequence counter produces a 4-billion-slot gap — FIXED

**Changed.** `accept` treats an unsigned sequence delta above
`maxSequenceGap` (3600) as a device-side counter restart rather than an hour
of missed pulses: `sequenceRestart` forgets the previous sample, clears the
pending misses and the window, logs once at WARN, and the pulse is counted one
`Glitches`. Normal gap accounting below the threshold is unchanged.

**Files.** `internal/refclock/pps.go`, `internal/refclock/pps_test.go`.

**Verification.** PASS. `TestPPSSequenceRestartIsNotFourBillionGaps` runs the
counter to 1000, feeds sequence 5, and asserts `Gaps` is unchanged, `Glitches`
went up by one, the window was dropped, and the following pulses re-prime it
normally.

## RF5X-027 — systemd unit grants write access to all of /run — FIXED

**Changed.** `ReadWritePaths=/var/lib/carillon` only. `RuntimeDirectory=`
already makes `/run/carillon` writable under `ProtectSystem=strict`, and
nothing writes outside that and `/var/lib/carillon`.
`RuntimeDirectoryMode` is unchanged.

**Files.** `deploy/systemd/carillon.service`.

**Verification.** Config review; `systemctl restart carillon && carillonctl version`
on `gummi` remains a target-host step.

## RF5X-028 — Routable placeholder hostname in the docs — FIXED

**Changed.** `server.example.net` → `server.invalid` in `deploy/README.md` and
`deploy/ACCEPTANCE.md`, including the authenticated `-keys` form.

**Files.** `deploy/README.md`, `deploy/ACCEPTANCE.md`.

**Verification.** PASS. `grep -rn 'example\.(net|com|org)'` over the tree
matches only the review document itself.

## RF5X-030 — readLine compares err.Error() == "EOF" — FIXED

**Changed.** `errors.Is(err, io.EOF)`, with `io` imported. A complete but
unterminated response wrapped in a `*net.OpError` used to be reported as
"reading response: EOF".

**Files.** `internal/control/client.go`.

**Verification.** PASS. Existing control tests.

## RF5X-031 — A non-prefer PPS becomes the system source — FIXED (documentation)

**Changed.** The review's first option: `DESIGN.md` §6.3 now states that a
qualified, locked, agreeing PPS source has stratum 0 and a tiny λ, so it sorts
first and is the system source whether or not it is marked `prefer`; `prefer`
additionally makes θ_sys its offset alone rather than the distance-weighted
mean. It says this matches ntpd's treatment of a refclock, is intended, and
that a pulse which should be visible but never used is configured `noselect`.
No behaviour change, as the specification allows.

**Files.** `DESIGN.md` §6.3.

**Verification.** Doc review.

## RF5X-017 — A RATE kiss's poll is not honoured as a new minimum — FIXED

**Changed.** The source keeps `kodMinPoll`, set from a RATE kiss and cleared
only when re-resolution yields a *different* address. `adaptPoll`'s lower
bound is now `pollFloor()` = `max(cfg.PollMin, kodMinPoll)`, and the
reachability-recovery reset uses it too, so a noisy update or a lost server
can no longer drop the poll below what the server demanded. A demand above
`PollMax` raises the effective maximum (capped at `discipline.MaxPoll`) with a
log line, instead of being silently clamped. DENY/RSTR handling is unchanged.

**Files.** `internal/source/ntp.go`, `internal/source/source_test.go`,
`DESIGN.md` §5.4.

**Verification.** PASS. `TestKissRATEPollIsANewMinimum` feeds a kiss demanding
poll 8, then a large offset through `adaptPoll`, then a reachability recovery,
and asserts the poll never drops below 8. `TestKissRATEBeyondPollMax` sends a
demand of 13 to a source configured `poll_max = 10` and asserts the emitted
poll is 13.

## RF5X-018 — Unconnected client sockets cost a full timeout per dead server — FIXED

**Changed.** `exchange` uses `net.DialUDP` and `conn.Write` instead of
`net.ListenUDP` and `WriteToUDPAddrPort`, as `DESIGN.md` §5.4 already
specified. `ECONNREFUSED` from either the write or the read maps to a new
`errUnreachable`, which `pollOnce` counts as a miss (with the timeouts, so
re-resolution still triggers) without waiting. The nonce/origin check, the
reply source-address check, kernel timestamping and the ephemeral-port
randomisation are unchanged.

**Files.** `internal/source/ntp.go`, `internal/source/source_test.go`,
`DESIGN.md` §5.4.

**Verification.** PASS. `TestUnreachablePortFailsFast` queries a port nothing
is listening on with a 5 s timeout and asserts the error is `errUnreachable`
and that it arrived in under 100 ms. (It skips rather than fails on a platform
that does not deliver the ICMP to the socket; on this darwin host it does.)

## RF5X-022 — Two divergent splitHostPort implementations — FIXED

**Changed.** `config.splitHostPort` is exported as
`config.ParseServerAddress` and is now the only parser. `source.NTPConfig`
gained `Host`/`Port`, filled by `main.go` from `Server.HostPort()`, and
`NewNTP` validates them rather than parsing; `source.Query` takes a host and a
port. `source.splitHostPort` and `containsColon` are deleted. A configuration
that passed `-check` can therefore no longer fail in `NewNTP`, after the clock
and the serial devices are already open. Accepted syntax is unchanged —
config's parser was already the stricter of the two and is what `-check` uses.

**Files.** `internal/config/config.go`, `internal/source/ntp.go`,
`internal/source/poll.go`, `cmd/carillon/main.go`,
`internal/source/source_test.go` (table deleted),
`internal/config/config_test.go`, `internal/server/integration_test.go`.

**Verification.** PASS. `source.TestSplitHostPort` is gone;
`config.TestHostPort` carries the union of both tables (every case from the
deleted one was already present or equivalent) and is documented as covering
the single parser. `TestNewNTPValidation` now checks the empty-host and
zero-port rejections.

## RF5X-020 — Precision measured as the minimum non-zero delta — FIXED

**Changed.** `measurePrecision` now collects the gaps between readings that
actually *differ* and reports the median, which is what RFC 5905 §7.3 and
ntpd mean by precision, instead of the smallest difference ever seen. Two
back-to-back `clock_gettime` calls on a TSC-backed clock can differ by one
nanosecond, so the minimum reported 2^-30 on any modern host whatever its real
resolution. Total clock reads are bounded (`precisionMaxReads`) so a
millisecond-resolution VM clock costs a few milliseconds at startup rather
than spinning. `ntp.PrecisionFromSeconds` and the `[-30, -6]` clamp are
unchanged, and a clock that never moves still reports -30.

**Files.** `internal/clock/precision.go`, `internal/clock/precision_test.go`.

**Verification.** PASS. The existing stepping-clock cases (1 µs, 30 ns, 1 ns,
1 ms) and the frozen-clock case all still hold.
`TestMeasurePrecisionIsNotTheMinimum` feeds a clock that ticks in microseconds
but shows a one-nanosecond difference every 50th read and asserts -20, not
-30. `TestMeasurePrecisionCoarseClockTerminates` checks the read budget.
Measured on this host: **2^-25**, where the old code reported 2^-30 — the
review predicted "-25…-23".

## RF5X-021 — Configuration validation gaps — FIXED

**Changed.** (a) `Validate` now fails when `type = "pps"` carries any of
`baud`, `pps`, `pps_edge`, `pps_offset`, `nmea_offset` or `sentences`, naming
the key; none of them are defaulted for a bare PPS refclock, so a non-zero
value can only have been written by the operator, and silently ignoring it
contradicts the strict-configuration promise. (b) The FreeBSD platform check
accepts an absolute `pps` path on a `gps` refclock — a second callout tty
carrying the pulse only, which `main.go` already supports and the Linux check
already accepts — and runs the `dev.uart.<N>.pps_mode` sysctl check against
*that* device's unit; the dcd/cts pin check is kept and applies only when a
pin was named. Existing valid configurations are unaffected.

**Files.** `internal/config/config.go`,
`internal/config/refclock_check_freebsd.go`,
`internal/config/config_test.go`.

**Verification.** PASS. Six new `TestValidateRules` cases, one per GPS-only
key. `TestGPSWithSeparatePPSDeviceValidates` covers (b)'s configuration
shape; the sysctl half is FreeBSD-only and `GOOS=freebsd go vet` passes, with
the manual check on `twocom` remaining, as the review notes a fake-sysctl test
is impractical.

## RF5X-029 — Statistics accumulate in one flat directory with no retention — FIXED

**Changed.** Daily files are written to `<dir>/YYYY/MM/DD/<kind>.tsv` — three
zero-padded segments, so paths sort lexically and each directory stays small —
created at rotation. `[stats] keep_days` (default 0 = keep everything,
validated non-negative) removes day directories older than the limit at
rotation time, whole UTC days, and prunes the month and year directories it
empties. TSV columns and the header line are unchanged.

**Files.** `internal/stats/writer.go`, `internal/stats/writer_test.go`,
`internal/config/config.go`, `cmd/carillon/main.go`,
`deploy/carillon.toml.example`, `deploy/README.md`, `DESIGN.md` §12.

**Verification.** PASS. The existing write and rotation tests were moved to
the dated paths. `TestRecorderPrunesExpiredDays` plants a day outside
`keep_days = 1` and one inside it and asserts only the expired one goes, with
today's file written.  `TestRecorderKeepsEverythingByDefault` asserts
`keep_days = 0` removes nothing.

---

## Session checkpoint — 2026-09-05

Findings addressed so far, in the review's suggested fix order: RF5X-001,
004, 002 (with 012 **skipped**), 005/010/006/034/036, 003 (Linux half; FreeBSD
half **skipped**) and 009, 008/023/024/035, 007, 011/025, then the Lows
013, 014, 015, 016, 017, 018, 019, 020, 021, 022, 026, 027, 028, 029, 030, 031.

**Still to do:** RF5X-032 (a source whose goroutine exits should be marked
unreachable and restarted with backoff, not deleted from status) and RF5X-033
(no deadline on the final `wg.Wait()`; `control.Server.reply` needs a write
deadline). Nothing else from the review is outstanding.

`go vet ./...` passes for darwin, linux and freebsd; `go test -race ./...`
passes on darwin.

**Note on an unrelated flake.** `source.TestReResolveAfterTimeouts` failed once
during this work with *9 misses without re-resolving*. It is a race in the
test, not in the daemon and not one of the review's findings: a reply from
server A that was already in flight when the test stopped A answering resets
the daemon's consecutive-timeout counter, while the test keeps counting misses
cumulatively. The same logic is present at the baseline commit `85353ae`;
40 repeats of the test and 12 full-suite runs are clean on both.
