# carillon deep review — 2026-09-05

Reviewer: Claude Fable 5.1 (effort: xhigh). Scope: every file in the repository
at commit `85353ae` (`Record the 2700bc7 deployment`), read in full, with data
flow traced across package boundaries. `go vet ./...` and `go test -race ./...`
pass on darwin before this review; nothing was modified.

Findings marked **Verified** were reproduced with throwaway tests run against
a scratch copy of the module (not committed); the exact test bodies are quoted
in the Evidence sections so they can be re-run. Findings marked **Needs
investigation** were reasoned from the code and the platform documentation but
not executed on a target host.

Severity scale: **Critical** defeats the daemon's stated purpose or steps the
clock wrongly; **High** wrong time or wrong behaviour on a real deployment
path; **Medium** degraded service, spec deviation with operational cost, or a
security weakening; **Low** correctness edge cases, doc/code drift,
maintainability.

Line numbers refer to the files as of `85353ae`.

---

## RF5X-001 — PPS spike gate rejects every pulse once the loop starts correcting the offset the PPS reported, and never recovers

**Severity:** Critical

**Location:** `internal/refclock/pps.go` — `accept()` lines 287–295 (spike
gate), `reject()` lines 391–403 (no window reset), `timeout()` lines 242–257
(window reset only on the reachable→unreachable transition), `appendWindow()`
line 306 (only accepted samples enter the window). Design: `DESIGN.md` §5.2
"Validation".

**Problem:** The spike test compares each new pulse offset `θ` against the
median of the *current window* with limit `max(5·MAD, 1 µs)`. A rejected pulse
is not added to the window, so the window (and therefore the median and MAD)
freezes at whatever it held when rejection began. The daemon's own discipline
loop then guarantees rejection: as soon as the PPS source becomes the system
source, `Loop.Update(θ_pps)` sets `Pending = θ_pps` and `Tick` slews the local
clock by `Pending/τ` per second (τ = 4·2^poll = 64 s at poll 4). Every
subsequent kernel PPS timestamp therefore moves by `θ_pps/64` per pulse. With a
real UART (MAD ≈ 0.1–2 µs → limit 1–10 µs) any residual PPS offset above
roughly `64 × limit` (64 µs – 640 µs) at takeover produces a pulse that is
farther from the frozen median than the limit; it is rejected, the window does
not move, the next pulse is farther still, and so on. Reach decays to zero in
8 pulses, the source becomes unreachable, the numbering NTP source takes over
and slews the clock further away, and the frozen window is never refreshed:
`timeout()` resets the window only when `wasReachable && reach == 0`, which is
false once reach is already zero, and `reject()` never resets it. The only
exits are a clock step (`Reset()`) or a daemon restart, and a restart repeats
the same sequence. The residual offset at PPS takeover is set by the NTP
numbering source, i.e. hundreds of µs on a LAN and milliseconds over a WAN, so
this will fire on the first live stratum-1 deployment.

**Evidence (Verified):** scratch test in package `refclock`:

```go
func TestExpSpikeCascadeUnderSlew(t *testing.T) {
	p, clk := testPPS(t)           // PollMin = PollMax = 4
	p.cfg.LockJitter = 20e-6
	base := time.Unix(1_800_000_000, 0)
	noise := []time.Duration{-100, 50, -50, 100, 0, 80, -80, 20} // ns
	seq := uint32(0)
	accepted, rejected := 0, 0
	feed := func(theta time.Duration) {
		seq++
		clk.Advance(time.Second)
		before := p.Info().Refclock.Spikes
		p.accept(pps.Sample{Sequence: seq, Time: base.Add(time.Duration(seq)*time.Second + theta + noise[int(seq)%len(noise)])})
		if p.Info().Refclock.Spikes > before { rejected++ } else { accepted++ }
	}
	for i := 0; i < 32; i++ { feed(200 * time.Microsecond) }          // stable window
	for i := 1; i <= 300; i++ {                                        // loop slews τ = 64 s
		feed(time.Duration(200e-6 * math.Exp(-float64(i)/64) * 1e9))
	}
	for i := 0; i < 100; i++ { feed(0) }                               // steady at 0
}
```

Output:

```
stable phase: accepted=32 rejected=0 reach=11111111 stable=true
slew phase (300 pulses): accepted=0 rejected=300 reach=00000000 spikes=300 window=16
steady phase at 0 offset (100 pulses): accepted=0 rejected=100 reach=00000000
```

Even after the slew has finished and the offset is steady at zero, no pulse is
ever accepted again. The daily statistics would show `pps.tsv` stop at the
moment the PPS took over, `carillonctl refclock` would show reach 000 with a
climbing spike count, and the host would silently fall back to NTP.

**Fix specification:**

1. Stop using the frozen window as the reference for the spike test. Either
   (a) always append the sample to the window and let the median/MAD reject it
   statistically (ntpd's approach: the median filter *is* the spike rejection;
   keep the `Spikes` counter as a diagnostic by counting samples that were more
   than `5·MAD` from the median at the time), or (b) compare against a
   prediction that includes the slope: fit the last `k` accepted samples
   (`k ≥ 4`) with a line and compare `θ` against the extrapolated value, with
   the limit widened by the maximum slew the loop can apply per second
   (`MaxSlewPPM × 1e-6 × seconds since last accepted pulse`). Option (a) is
   simpler and is what the RFC 5905 refclock median filter already assumes.
2. Whatever gate remains, a rejection must not be able to persist: if `n`
   consecutive pulses are rejected (suggest `n = 4`, well under the 8 that
   empty reach), reset the window (`resetWindow()`) so it re-primes from the
   next pulses. Also make `timeout()` reset the window whenever reach is zero,
   not only on the transition.
3. Do not widen `spikeFloor`; a 1 µs floor is fine once the window tracks.
4. Must not change: the window-σ lock criterion (`ppsWindowStable`), the
   emitted `Measurement` fields, the reach-register semantics for gaps and
   glitches, `OnPulse` delivery for accepted pulses, or the `Spikes/Gaps/
   Glitches` counter names (they are metric labels).

**Verification:** Add the test above (renamed) to `internal/refclock/pps_test.go`
asserting `accepted > 250` in the slew phase and `accepted == 100` in the
steady phase, reach `0xff` at the end, and `Spikes` at most a handful. Add a
second test with a genuine 10 ms spike in a stable window and assert it is
rejected and the next good pulse is accepted. Then run the full simulation
with a PPS source added to `sim_test.go`: from a 500 µs initial offset the run
must end with the PPS as system source, reach `0xff`, and RMS under 10 µs.

---

## RF5X-002 — Measurements queued before a clock step (or leap reset) are applied after it, causing a second step of the same size

**Severity:** High

**Location:** `internal/engine/engine.go` — `Run()` measurement loop lines
269–273; `handle()` `ActionResetFilters` lines 327–331; channel created at
line 122 (`make(chan discipline.Measurement, 64)`).
`internal/source/ntp.go` — `Reset()` line 177 and the consumption point in
`hit()` line 375 (`resetRequested.Swap(false)` *before* `filter.Add` at 382).
`internal/discipline/system.go` — `Update()` lines 223–230 has no way to tell
a pre-step measurement from a post-step one. The same applies to
`refclock.PPS.Reset`/`NMEA.Reset`.

**Problem:** A `Measurement` carries no generation/epoch. Every source runs in
its own goroutine and sends into a 64-deep buffered channel. At startup with
several servers and `iburst`, all first replies arrive within milliseconds and
several measurements are queued before the engine reads the first one. The
engine processes source A's measurement, steps the clock by θ, calls
`Reset()` on all sources (which only affects samples not yet added), and then
reads source B's *already queued* measurement whose offset was computed
against the pre-step clock. B is applied, becomes the only valid candidate
(A was invalidated by the step), becomes the system source, and its θ is
again above `StepThreshold` while `Updates < StepLimit`, so the clock is
stepped a second time by the same amount. The host ends up wrong by θ in the
opposite direction, with one or no remaining steps in the budget, and slews
back at 500 ppm (2000 s per second of error). The same race exists inside a
single source: an exchange whose T1 was before the step and T4 after yields
an offset and delay wrong by the step, and because `hit()` consumes
`resetRequested` *before* adding that sample, the corrupt sample becomes the
first entry of the freshly reset filter and is emitted as a valid estimate.
After a leap-second `InvalidateSources`, a queued pre-leap measurement is
wrong by exactly 1 s; it is under the step threshold budget only by luck of
`StepLimit` having been consumed, so it is slewed at 500 ppm for one poll
interval (tens of ms of excursion) before a fresh sample corrects it.

**Evidence (Verified):** scratch test in package `engine` using the existing
`scripted` source (two sources each scripted `good(2.0)` then good samples):

```go
a := &scripted{name: "a", clk: clk, gap: 300 * time.Millisecond, script: script}
b := &scripted{name: "b", clk: clk, gap: 300 * time.Millisecond, script: script}
e, _ := New(testConfig("", SourceSpec{Source: a, ...Numbering}, SourceSpec{Source: b, ...Numbering}), clk, quietLog())
... run until Updates >= 6 ...
t.Logf("steps applied to the clock: %v", clk.Steps)
```

Output: `steps applied to the clock: [2s 2s] (count 2)` — the fake clock was
stepped +2 s twice for a +2 s initial offset. `resets: a=2 b=2` confirms the
engine did call `Reset()` after each step; it made no difference because the
second measurement was already in the channel. The existing
`TestEngineStepsSettlesAndPersistsDrift` uses a single source and cannot see
this.

**Fix specification:**

1. Add a generation counter. `discipline.Measurement` gains `Generation
   uint64`. The engine owns `gen atomic.Uint64` (or plain field plus an atomic
   getter passed to sources); it increments it in `handle()` when applying
   `ActionStep` and in the leap-crossing branch, *before* calling `Reset()`.
2. Sources stamp each measurement with the generation read at the *start* of
   the sample (NTP: read before `T1`; PPS: at the fetch; NMEA: at the `$`
   read). A source that observes a generation change between sample start and
   emit discards the sample (do not add it to the filter) and emits only a
   reach update. Provide the generation via a `func() uint64` in
   `NTPConfig`/`PPSConfig`/`NMEAConfig` so tests can drive it.
3. The engine (or `System.Update`) drops any measurement whose `Generation`
   is older than the current one, counting it (log at DEBUG, and add a
   `stale` result to `carillon_source_events_total`).
4. Keep `Reset()` as-is for the filter-state side; it is still needed.
5. Independently of the generation counter, `Loop.Update` should not be
   allowed to step twice on samples that are all first-in-filter: after a step,
   require that the next step candidate is supported by at least two
   post-step samples from the same source (`Filter.Len() >= 2`) or by two
   sources agreeing. This is defence in depth; the generation counter is the
   real fix.
6. Must not change: `Measurement` field names already consumed by
   `stats`/`control`; `Source.Reset()` signature (other implementations
   exist in tests); the step policy semantics (`StepLimit` still counts loop
   updates).

**Verification:** Promote the scratch test into `engine_test.go` and assert
`len(clk.Steps) == 1`. Add a `source` package test that starts an exchange,
bumps the generation while the fake server delays its reply, and asserts the
reply is discarded (`Info().Received` unchanged, no valid measurement). Add a
`System` test that feeds a measurement with an old generation after a step
and asserts no second step.

---

## RF5X-003 — Requests addressed to a directed broadcast address are answered; on FreeBSD the reply is sent with a broadcast source address

**Severity:** High

**Location:** `internal/server/martian.go` — `martianDestination()` lines
47–54 (checks only unspecified, multicast and `255.255.255.255`).
`internal/server/pktinfo_linux.go` `destination()` lines 36–52 returns
`ipi_addr` and copies it into `Spec_dst` for the reply;
`internal/server/pktinfo_freebsd.go` `destination()` lines 36–51 echoes
`IP_RECVDSTADDR` into `IP_SENDSRCADDR`. `internal/server/listener.go`
`serve()` lines 189–224 discards the `flags` word from `ReadMsgUDPAddrPort`
except for `MSG_TRUNC`. Design: `DESIGN.md` §7.2 step 2 promises that a
datagram to "a broadcast or multicast group" is dropped and counted `martian`,
and names directed broadcast explicitly.

**Problem:** A datagram sent to the subnet's directed broadcast address
(`192.168.1.255`, `172.19.255.255`, …) is delivered to the socket with the
broadcast address as its destination. `martianDestination` returns false for
it, the request is served and counted `served`, and the reply is sent with
`IP_SENDSRCADDR`/`ipi_spec_dst` set to the broadcast address. FreeBSD's
`in_pcbbind_setup` accepts a broadcast address as local (`ifa_ifwithaddr`
matches `ifa_broadaddr`), so the reply leaves the host with source
`192.168.1.255`: one forged datagram to the broadcast address makes every NTP
server on the segment that has this gap answer the victim at once, which is
exactly the amplification pattern §7.2 step 2 exists to prevent. On Linux the
send fails (`__ip_route_output_key` rejects a non-local source with `EINVAL`),
which is logged at DEBUG only, while the `served` counter and
`last_served` timestamp were already incremented — the outcome partition of
D15 is wrong and the operator sees served traffic that never left the host.

**Evidence (Verified):** `martianDestination(netip.MustParseAddr("192.168.1.255"))`
returns `false` (scratch test in package `server` printed
`martianDestination(192.168.1.255) = false`, likewise for `10.255.255.255` and
`172.19.255.255`). The FreeBSD source-address behaviour is from
`sys/netinet/in_pcb.c` (`in_pcbbind_setup` → `ifa_ifwithaddr_check`) and
`sys/net/if.c` (`ifa_ifwithaddr_internal` compares `ifa_broadaddr`); it should
be confirmed on `twocom` with a single `socat`/`nc -u` datagram to the LAN
broadcast address and a `tcpdump` on the segment (**Needs investigation** for
the wire proof; the code path is certain).

**Fix specification:**

1. FreeBSD: `recvmsg` sets `MSG_BCAST` (and `MSG_MCAST`) in `msg_flags` for
   datagrams received via broadcast/multicast. In `serve()`, treat
   `flags & unix.MSG_BCAST != 0 || flags & unix.MSG_MCAST != 0` as martian
   (count `martian`, `continue`) before decoding. Put this in a per-OS helper
   next to `enablePacketInfo` so the `_other` build stays a no-op.
2. Linux: `IP_PKTINFO` delivers both `ipi_addr` (header destination) and
   `ipi_spec_dst` (the local address the kernel would use for the route). For
   a unicast request they are equal; for a directed broadcast `ipi_addr` is the
   broadcast address and `ipi_spec_dst` is the interface address. In
   `destination()`, return martian (an invalid `netip.Addr` plus a boolean, or
   a sentinel) when `got.Addr != got.Spec_dst`, and always build the reply's
   `Spec_dst` from `got.Spec_dst`, never from `got.Addr`. IPv6 has no
   broadcast; multicast is already covered.
3. Keep the existing `martianDestination` checks (they are still right for
   `255.255.255.255`, multicast, unspecified).
4. Count the drop under `martian` so the D15 partition stays exact; do not
   introduce a new `result` label.
5. Must not change: the reply source-address selection for unicast on
   multi-homed hosts (replies must still leave from the address the request
   was sent to), the `_other` platform stubs, or the reply-length invariant.

**Verification:** Unit-test `destination()` per platform with a hand-built
`IP_PKTINFO` control message where `Addr != Spec_dst` and assert the martian
result; unit-test the FreeBSD flag path by calling the new helper with
`unix.MSG_BCAST`. On a target host: send one 48-byte mode-3 packet to the
subnet broadcast from another LAN host, confirm `carillonctl serverstats`
increments `martian` (not `served`) and `tcpdump` shows no reply.

---

## RF5X-004 — Shutdown leaves the per-second slew transient in the kernel frequency word

**Severity:** High

**Location:** `internal/engine/engine.go` — `Run()` exit path lines 285–288
(`cancel(); wg.Wait(); maybeWriteDrift(final)`), no actuator call.
`internal/discipline/loop.go` — `Tick()` lines 258–272 issues `Freq + adj`
(adj up to ±`MaxSlewPPM`) as the kernel frequency every second. Design:
`DESIGN.md` §12 "engine writes the drift file, leaves the frequency word
alone (it is the best estimate we have)".

**Problem:** The "frequency word" the kernel holds at any instant is the base
frequency *plus* the phase-slew transient the last `Tick` applied. The drift
file is written with the base (`sys.Frequency()` = `loop.Freq`), but the
kernel is left running at up to base ± 500 ppm. Every clean stop while a slew
is in progress (startup, after a holdover transient, after any correction
larger than a few µs) leaves the host's clock running fast or slow by the
transient until the next start writes the base back. A restart that fails
`-check` (config edit, keys permission), a stopped service during
maintenance, or a switch back to ntpd/chrony (which start from *their* drift
files and will fight a 500 ppm-off kernel word only if they write frequency
immediately) leaves the host drifting at up to 43 s/day. The engine test
`TestEngineStepsSettlesAndPersistsDrift` checks the drift file value, not the
last actuator call, so this is invisible to the suite.

**Evidence (Verified):** scratch test in package `engine` with a single
scripted source reporting +0.3 s three times (below the step threshold), then
cancelling the context:

```
base frequency (drift file value) = 0.000 ppm; last word written to kernel = 500.000 ppm; pending slew = 0.2930 s
```

**Fix specification:**

1. In `Engine.Run`, after `wg.Wait()` and before `maybeWriteDrift`, issue
   `e.clk.SetFrequency(e.sys.Frequency())` when `haveApply` is true (expose
   `Loop.Applied()` or have `System.Tick` report it) so the kernel holds the
   base estimate. Log at INFO with the base value and the pending phase that
   was abandoned. Do not attempt to finish the slew (it can be arbitrarily
   long) and do not step.
2. Also clear the kernel transient on the fatal-actuator path only if the
   failure was not `SetFrequency` itself (avoid a second error).
3. Consider (optional) `Loop.Tick` emitting `SetFrequency(base)` when the
   engine is asked to stop; either place is fine as long as it happens after
   the last `Tick`.
4. Must not change: `STA_UNSYNC` handling on exit (currently the kernel keeps
   the last synced status, which lets the RTC write-back continue; that is
   intentional), the drift-file format, or the "never step on exit" rule.

**Verification:** Extend `TestEngineStepsSettlesAndPersistsDrift` (or add a
test) to assert `clk.Frequencies[len-1] == e.Status().Frequency` after `Run`
returns while `Pending != 0`. On a host: `carillonctl tracking` showing a
non-zero pending slew, `service carillon stop`, then `ntptime`/`adjtimex
--print` must show the drift-file frequency, not ±500 ppm.

---

## RF5X-005 — A leap-second transition drops the server to LI=3 / stratum 16 for three loop updates

**Severity:** Medium

**Location:** `internal/engine/engine.go` — `handle()` leap branch lines
338–349 calls `Source.Reset()` on every source and then
`sys.InvalidateSources(now)`. `internal/discipline/system.go` —
`InvalidateSources()` lines 197–203; `reselect()` lines 277–285 (no
candidates → `StateHoldover`) and lines 319–331 (Holdover → Settling →
`settled` counted up to `SettleUpdates` = 3 before Synced).
`cmd/carillon/main.go` line 297 maps `Synced` on the wire to
`StateSynced || StateHoldover`, so `StateSettling` answers LI=3, stratum 16.

**Problem:** Invalidating every source's estimate at the leap instant leaves
the selector with no candidates; `reselect` moves SYNCED → HOLDOVER (still
served, degraded). The first fresh measurement then moves HOLDOVER →
SETTLING, which is served as *unsynchronised* (LI=3, stratum 16, root
dispersion 16 s) until three loop updates have run. Three loop updates need
three *new lowest-delay filter samples* (see RF5X-006), not three polls, so
the outage is at least 3 poll intervals and in practice much longer: at poll
7 with a PPS system source that is 6.5 minutes, at poll 10 on the colo it is
up to 51 minutes, and the deployment record shows 15 minutes on a LAN. Every
client rejects the server at exactly the moment a leap second makes a good
server most valuable, and downstream carillon instances that `prefer` this
host lose their preferred source ("preferred source is not usable") and slew
to their public survivors, which the acceptance log already documents as a
7 ms transient with a frequency excursion of tens of ppm. The existing engine
test `TestEngineLeapfileOverridesAndResetsAtTransition` asserts the HOLDOVER
state as expected behaviour and stops there, so the SETTLING window after it
is untested.

**Evidence (Verified):** scratch test in package `discipline`: bring a
`System` to SYNCED with four good measurements, call `InvalidateSources`,
then feed three good measurements:

```
before leap: state=synced
after InvalidateSources: state=holdover stratum=3 leap=none
post-leap update 1: state=settling stratum=3 leap=unsynchronized
post-leap update 2: state=settling stratum=3 leap=unsynchronized
post-leap update 3: state=synced stratum=3 leap=none
```

(`Status.Stratum` stays 3 in Settling but `server.SystemStatus.Synced` is
false there, so the wire stratum is 16.)

**Fix specification:**

1. Treat the post-leap reset as a filter refresh, not a loss of
   synchronisation. In `System`, add an explicit `Resync(now)` used by the
   engine's leap branch that invalidates source estimates and clears
   `lastUsedAt`, but leaves `state` untouched and sets a flag so that the
   first post-reset loop update goes straight back to SYNCED (or stays
   HOLDOVER-then-SYNCED) without passing through SETTLING. The frequency is
   unchanged by a leap and the residual phase error is bounded by the
   pre-leap jitter, so there is nothing to settle.
2. Alternatively keep the state machine but make `settled` survive the
   holdover caused by the leap reset (do not zero `settled` on a
   `StateSynced → StateHoldover` transition that was caused by
   `InvalidateSources`).
3. Whichever is chosen, `RF5X-002`'s generation counter must be bumped at
   the same point so pre-leap measurements are discarded rather than
   invalidated-then-applied.
4. Must not change: the kernel `STA_INS/STA_DEL` sequencing (indicator
   applies only on the last day; cleared after), `Source.Reset()` being called
   so PPS/NMEA windows drop pre-leap samples, or the holdover timeout.

**Verification:** Extend `TestEngineLeapfileOverridesAndResetsAtTransition`:
after the transition, feed one good measurement and assert `State ==
StateSynced` and `clk.Status().Synced == true` with `Leap == LeapNone`; assert
the server-side `SystemStatus.Synced` (via the same mapping `main.go` uses)
never reads false across the transition. The simulation in `sim_test.go`
should gain a leap case that counts seconds spent in `StateSettling` after
the transition and asserts zero.

---

## RF5X-006 — SETTLING → SYNCED counts *filter updates*, so a restarted serving host answers LI=3 for many minutes

**Severity:** Medium

**Location:** `internal/discipline/system.go` — `reselect()` line 294
(`if sel.System.At <= s.lastUsedAt { return }`: a loop update happens only
when the system source's filter chose a *new* sample) and lines 324–329
(`settled++` per loop update, SYNCED at `SettleUpdates` = 3).
`internal/discipline/filter.go` — `Add()` lines 143–147 (a sample is only
"new" when it beats every older sample on delay; the popcorn/staleness rule).
`cmd/carillon/main.go` line 267 (`SettleUpdates: 3`). Design: `DESIGN.md`
§6.5 SETTLING and §3 "first correction within ~10 s".

**Problem:** On a low-jitter path the first or second reply is usually the
lowest-delay sample the filter will see for many polls; later samples with
slightly larger delay are reported `updated = false` and produce no loop
update. Three loop updates can therefore take dozens of polls while the
server answers LI=3 / stratum 16 and every client and downstream instance
rejects it. The deployment record quantifies it: `twocom` "sat in settling
for 15 minutes with two loop updates" and "served 108 requests
unsynchronized in that window"; `gummi` took 5 m 25 s and, on another day,
"over eight minutes with only two loop updates". A restart of a serving host
is therefore a multi-minute outage for its clients, and with RF5X-005 the
same happens after every leap second. The criterion is also inconsistent
with §6.5's own justification ("waiting for the offset to shrink first would
keep a slowly converging client unsynced for an hour for no gain"): the
intent is to guard against a *step* within the first updates, not to wait for
three distinct low-delay samples.

**Evidence:** `deploy/ACCEPTANCE.md` lines 293–297 and 180–184 (operational);
code path as cited. The loop-update gate is correct for the *PLL* (feeding the
same sample twice would double-integrate it) but is the wrong unit for
"trusted enough to serve".

**Fix specification:**

1. Decouple "settled" from loop updates. Count SETTLING progress in
   *measurements received for the system source after the last step*
   (valid or not), or in elapsed time since the last step (e.g. `2·2^poll`
   seconds with at least one loop update and no step), and declare SYNCED
   when either (a) `StepLimit` is exhausted or the offset at the last update
   was below `StepThreshold` and (b) at least one post-step loop update has
   run. ntpd declares itself synchronised on the first clock update; chrony
   likewise. Keep a minimum of one post-step update so a step is never served
   as synced.
2. Make `SettleUpdates` configurable under `[discipline]` (default 1 with the
   time-based guard above) rather than hard-coded in `main.go`, and document
   the change in `DESIGN.md` §6.5.
3. Must not change: the loop-update gating itself (`lastUsedAt`), which
   protects the PLL; the wire behaviour while genuinely UNSYNCED (stratum 16,
   LI=3, refid INIT/STEP/HOLD); `carillonctl waitsync` semantics (SYNCED is
   still the condition it waits for).

**Verification:** `TestSimSettling` currently only asserts all three states
were seen; add an assertion that the time spent in `StateSettling` after the
last step is under two poll intervals for the sim source with delay noise.
On a host: `systemctl restart carillon && carillonctl waitsync 60` must
succeed on the LAN pair (the record shows it failing at 300 s today).

---

## RF5X-007 — A qualified prefer PPS never enters the intersection; a wrong-edge or inverted PPS drives the clock wrong at stratum 1 undetected

**Severity:** Medium

**Location:** `internal/discipline/select.go` — `Select()` lines 201–205
(PPS sources are removed from `cands` before `intersect`), 214–226
(qualification only checks that *some* numbering survivor has `|θ| < 0.4 s`,
not that it agrees with the PPS), 239–245 (a prefer PPS becomes the system
source outright, and `sel.Offset = sys.Offset` at line 271). Design:
`DESIGN.md` §5.2 qualification and §6.3 "PPS override".

**Problem:** The only sanity check on the PPS offset is that a numbering
source is within ±0.4 s of the local clock. Nothing compares the PPS offset
to that source's offset. A PPS on the wrong edge (`edge = "assert"` on a
receiver whose second mark is the falling edge, or an inverted signal that
`dev.uart.N.pps_mode` should have had `0x10` for) reports a stable offset
equal to the pulse width — typically 20–200 ms — with µs-level jitter, so it
locks, qualifies, becomes the system source, and the daemon disciplines the
clock to be 20–200 ms wrong while advertising stratum 1 refid `PPS` with a
root dispersion of a few µs. The NTP numbering source then reports
`θ ≈ −pulse width`, still inside the 0.4 s guard band, so the PPS stays
qualified indefinitely and the NTP source (which is *correct*) is the one
that looks wrong. Downstream, the colo prefers this host and inherits the
error with a plausible-looking root distance. ntpd's PPS driver has the same
weakness in principle, but ntpd's `flag3`/`pps_stratum` behaviour and the
`tos mindist` interaction at least make the NTP peer a falseticker visibly;
here the NTP source is reported as a *survivor* with a large offset, which
`carillonctl sources` will show but nothing alarms on.

**Evidence:** Code path as cited; `TestSelectPPSQualification` only tests the
guard band. Not executed on hardware (**Needs investigation** on the live PPS
test: deliberately configure the opposite edge and confirm the daemon locks
to the wrong second boundary).

**Fix specification:**

1. Qualification must require agreement, not just proximity to zero: a PPS
   source is qualified only if at least one *surviving* numbering source `n`
   satisfies `|θ_n − θ_pps| ≤ λ_n + guard` where `λ_n` is that source's root
   distance and `guard` is a few ms (the numbering source's own jitter budget;
   `max(4·ψ_n, 1 ms)` is reasonable). Keep the existing `|θ_n| < 0.4 s` test
   as well (it is what makes the whole-second numbering valid).
2. When a locked, stable PPS fails the agreement test, mark it
   `StatusFalseticker` (not `StatusUnqualified`) and emit `EventFalseticker`
   so the engine logs at WARN once; `carillonctl refclock` should show a
   distinct "disagrees with <source> by X ms" field so the operator is
   pointed at `edge`/`pps_mode 0x10`/`offset`.
3. Must not change: the ±0.4 s guard band value, the PPS bypass of the
   *combine* step once qualified (that is the point of the prefer PPS), or the
   handling of a PPS with no numbering source at all (`EventPPSUnqualified`).

**Verification:** Add to `select_test.go`: NTP source at θ = 0.000, PPS at
θ = 0.120 with 1 µs jitter → PPS must be `StatusFalseticker`, system source
must be the NTP source, `PreferLost` true. NTP at θ = 0.100 ± 2 ms and PPS at
θ = 0.101 → qualified. On hardware: swap `edge` and confirm the daemon logs
the disagreement and does not go to stratum 1.

---

## RF5X-008 — Rate limiting runs before MAC verification, so a spoofed flood from the colo's address silently breaks the authenticated home → colo association

**Severity:** Medium

**Location:** `internal/server/responder.go` — `Handle()` lines 187–196
(token bucket consulted and RATE KoD sent) before lines 198–213 (MAC
verification and `require_key`). `internal/server/ratelimit.go` —
`allow()` lines 59–89 keys the bucket by source address only. Design:
`DESIGN.md` §7.2 order (this is what the design says) and §2 (the colo
"trusts only its own upstream").

**Problem:** The token bucket is keyed by source address and consumed before
the MAC is checked, so any host that can send UDP with a forged source
address of the colo (no reflection needed; the attacker never needs a reply)
drains the colo's bucket at the home server with `rate_limit_pps` packets
per second. The colo's genuine, authenticated polls then get RATE kisses (or
nothing), its poll interval is pushed to `poll_max`, home is marked
unreachable, `prefer_lost` fires, and the colo synchronises to the public
falseticker-detector servers instead of the trusted upstream — precisely the
failure the CMAC key exists to prevent, achievable with 8 packets a second
from anywhere on the internet path (the home listener has to be reachable
from the colo, so it is reachable from the attacker). The design's 64 bytes
of CMAC work per packet is the cost being avoided, but an attacker can
already force that cost on any host by sending packets with a *known key id
and a bad MAC*, which the code verifies before dropping (`badAuth`), so the
ordering buys nothing against a hostile sender.

**Evidence:** Code as cited. `TestRateLimitAndKoDThrottle` and
`TestAuthentication` never combine the two paths.

**Fix specification:**

1. For a source address that matches a `require_key` prefix, verify the MAC
   *before* consulting the limiter, and give verified requests their own
   bucket keyed by `(address, keyID)` with a separate, higher rate (or exempt
   them: a peer that holds the shared key is by definition trusted). An
   unverified request from such an address goes to the ordinary per-address
   bucket and is then dropped as `badAuth` as today, so the spoofed flood
   costs the attacker's packets one CMAC each and cannot touch the
   authenticated bucket.
2. Requests from addresses that do *not* match `require_key` keep the current
   order.
3. Cap the extra CMAC work: only addresses inside `require_key` prefixes
   (a handful of /32s) get the verify-first path, so an internet flood is not
   a CMAC DoS.
4. Must not change: reply-length invariant; `bad_auth`/`rate_limited`
   counter meanings (a verified-then-limited request is still
   `rate_limited`); the KoD throttle.

**Verification:** Add a handler test: `RequireKey` for `192.0.2.7/32`, send 100
unsigned requests from that address in one second (all must be `badAuth`,
none `rateLimited`), then one correctly signed request and assert it is
served. On the LAN pair: replay a captured unsigned mode-3 packet with the
colo's source address at 20 pps against home for a minute and confirm
`carillonctl sources` on the colo keeps reach 377 for home.

---

## RF5X-009 — `recv_buffer` above `kern.ipc.maxsockbuf` aborts startup on FreeBSD; the design says the kernel clamps it

**Severity:** Medium

**Location:** `internal/server/listener.go` — `listenOne()` lines 109–113
(`conn.SetReadBuffer(recvBuffer)`; any error is fatal). `deploy/
carillon.toml.example` lines 173–181 (public block sets `recv_buffer =
4194304`). Design: `DESIGN.md` §7.1 "the kernel clamps the request to
`net.core.rmem_max` or `kern.ipc.maxsockbuf`, so the size actually granted is
read back and logged".

**Problem:** Linux silently clamps `SO_RCVBUF` to `rmem_max`, so the "read
back and log" design holds there. FreeBSD does not clamp: `sosetopt` →
`sbreserve_locked` returns 0 when the request exceeds `sb_max_adj`
(`kern.ipc.maxsockbuf`, default 2 MB, adjusted to ≈1.86 MB) and
`setsockopt` fails with `ENOBUFS`. `SetReadBuffer` therefore errors, `Listen`
fails, and the daemon exits at startup with `receive buffer of 4194304
bytes: ... no buffer space available`. The example's recommended public
configuration, copied verbatim to a FreeBSD pool host with default sysctls,
will not start. `twocom` is FreeBSD and is the public host; the acceptance
record notes `recv_buffer` is still unset there, so this has not yet been
hit. **Needs investigation** on `twocom`: confirm `sysctl kern.ipc.maxsockbuf`
and that `carillon -check`/startup fails with the example block.

**Evidence:** `sys/kern/uipc_socket.c` `sosetopt` case `SO_RCVBUF`
(`if (sbreserve(...) == 0) { error = ENOBUFS; goto bad; }`);
`sys/kern/uipc_sockbuf.c` `sbreserve_locked` (`if (cc > sb_max_adj) return
(0);`). `config.MaxRecvBuffer` is 256 MB, so validation cannot catch it.

**Fix specification:**

1. On `SetReadBuffer` failure with `ENOBUFS` (FreeBSD) or `EINVAL`, retry
   with successively halved sizes down to `config.MinRecvBuffer`, then read
   back the effective size and log it at WARN naming the sysctl to raise
   (`kern.ipc.maxsockbuf` / `net.core.rmem_max`). Only fail startup if even
   the minimum is refused.
2. `-check` cannot know the sysctl without a socket; have it log the same
   advice as a warning when `recv_buffer` is set and `runtime.GOOS ==
   "freebsd"`, or read `kern.ipc.maxsockbuf` via `unix.SysctlUint64` in
   `checkRefclockPlatform`'s sibling and compare.
3. Update `DESIGN.md` §7.1 and the example's `recv_buffer` comment to say
   FreeBSD refuses rather than clamps, and that `kern.ipc.maxsockbuf` must be
   raised first.
4. Must not change: the effective-size logging, the Linux behaviour.

**Verification:** Unit test with a fake `syscall.RawConn` is awkward; instead
add a listener test that requests `256 << 20` on loopback and asserts
`Listen` succeeds on every platform and `ReceiveBuffers()[0] > 0`. On
`twocom`: set `recv_buffer = 4194304` with default `maxsockbuf`, confirm the
daemon starts and logs the granted size.
