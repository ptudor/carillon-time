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

---

## RF5X-010 — The SETTLING counter is not reset by a second step inside the startup window

**Severity:** Low

**Location:** `internal/discipline/system.go` — `reselect()` lines 312–318
(`u.Stepped` branch calls `setState(StateSettling)`), `setState()` lines
232–241 (returns early when the state is unchanged, so `settled` keeps its
value).

**Problem:** After a first step the state is SETTLING with `settled = 0`. A
good update makes `settled = 1`. A second step (allowed while `Updates <
StepLimit`) calls `setState(StateSettling)`, which is a no-op because the
state is already SETTLING, so `settled` stays 1 and SYNCED is declared after
two post-step updates instead of the three §6.5 specifies.

**Evidence (Verified):** scratch test in package `discipline`:

```
update offset=+2.000 stepped=true  state=settling settled=0
update offset=+0.001 stepped=false state=settling settled=1
update offset=+2.000 stepped=true  state=settling settled=1   <- not reset
update offset=+0.001 stepped=false state=settling settled=2
update offset=+0.001 stepped=false state=synced   settled=3
```

**Fix specification:** In the `u.Stepped` branch set `s.settled = 0`
explicitly before `setState`. If RF5X-006 replaces the counter, make sure the
replacement's "since last step" reference is reset on every step, not only
on the first. Must not change: `SettleUpdates` default.

**Verification:** Promote the scratch test; assert `State == StateSettling`
after the fourth update and SYNCED only after the fifth.

---

## RF5X-011 — `Loop.Tick` assumes exactly one second per tick, and `Update` drops the slew transient for one second

**Severity:** Low

**Location:** `internal/discipline/loop.go` — `Tick()` lines 258–272
(`Pending -= actual` assumes the transient was applied for exactly 1 s;
`now` is ignored); `Update()` line 225 emits `SetFrequency(l.Freq)` (base
only) so the transient issued by the previous `Tick` is removed until the
next `Tick`. `internal/engine/engine.go` line 261 (`time.NewTicker(e.tick)`).

**Problem:** The engine ticker can be late (GC, VM pause, a slow drift-file
write, a `handle()` that blocks in `Observe`); the kernel keeps running at
`base + adj` for the whole delay while `Pending` is only debited for one
second, so the phase is over-corrected by `adj × (delay − 1 s)` — up to
500 µs per second of lateness during a saturated slew. Separately, each
`Update` writes the base frequency, pausing the slew for up to one second per
update; harmless at poll 6, but with a PPS at poll 4 and a step-less
1 s ticker it removes ~6 % of the slew capacity.

**Fix specification:** Pass the real elapsed time into the slew accounting:
`Tick(now)` computes `dt = now − lastTick` (clamped to `[0, 2 s]`; anything
larger is logged as a stall and treated as 1 s), debits `Pending -= actual ×
dt` and `slewed += actual × dt`. Have `Update` emit `Freq + adj` (the
current transient) or emit nothing and let the next `Tick` apply the new base.
Must not change: the ±`MaxSlewPPM` and ±500 ppm clamps; the exponential
approach (`TestLoopTickSlew` expects 1/e after τ ticks — keep that with
`dt = 1`).

**Verification:** Add a loop test that calls `Tick` with `now` advancing by
3 s and asserts `Pending` is debited by `3 × adj`; run the simulation with a
random tick jitter of ±200 ms and assert the RMS is unchanged.

---

## RF5X-012 — Filter dispersion ignores unfilled stages, so a single sample is fully trusted

**Severity:** Low

**Location:** `internal/discipline/filter.go` — `Add()` lines 119–129: the
dispersion sum runs over `len(order)` = `f.n` populated stages only. RFC 5905
§10 (and ntpd `clock_filter`) initialise all eight stages at `MAXDISP` and
sum `Σ ε_i · 2^-(i+1)` over all eight, so a filter with one sample has
dispersion ≈ ε/2 + 16 · (1/4 + … + 1/256) ≈ 7.9 s until it fills.
`TestFilterSingleSample` asserts the current 0.0005.

**Problem:** A source's first sample gets a root distance of a few ms
instead of several seconds, so (a) it is an immediate candidate with full
weight in the intersection and combine, (b) the very first loop update — and
therefore the step decision — can rest on one packet from one server, and (c)
with several sources the earliest replier dominates. This is what makes
RF5X-002 bite at startup. The design's "first correction within ~10 s"
target does not require this: with `iburst` the filter has four samples in
6 s, at which point the RFC dispersion is already ~1 s and falling.

**Fix specification:** Treat absent stages as `MaxDispersion` in the
dispersion sum (loop over `FilterStages`, using `MaxDispersion` for
`i >= n`), exactly as RFC 5905. If the faster start is wanted, gate it
elsewhere (e.g. require `Filter.Len() >= 2` before a *step*, see RF5X-002
item 5) rather than by misreporting dispersion. Update
`TestFilterSingleSample`/`TestFilterDispersionAges` expectations. Must not
change: the jitter computation, the staleness rule, `Output.Samples`.

**Verification:** New filter test asserting one-sample dispersion ≈ 7.9 s
and four-sample dispersion ≈ 1 s; `TestSimConvergesFromUnknownFrequency`
must still converge (it will, the sim source uses `iburst`-like cadence).

---

## RF5X-013 — NMEA reader would spin if `read(2)` returns 0 bytes without an error

**Severity:** Low (**Needs investigation**)

**Location:** `internal/serial/read_unix.go` — `ReadTimeout()` lines 34–41
returns `n == 0, nil` on EOF; `internal/refclock/nmea.go` — `Run()` lines
171–182 treats that as a successful read, `consume` does nothing, and the
loop immediately calls `ReadTimeout` again, where `poll(2)` returns
readable-at-EOF at once.

**Problem:** On Linux a `CLOCAL` tty never returns EOF, and a USB detach
surfaces as `POLLHUP`/`POLLERR` (handled). On FreeBSD a detached `ucom`
should return `ENXIO` from `ttydev_read` (handled). If either kernel ever
returns a plain 0-byte read (some USB-serial drivers on Linux do return 0
once after a hangup before `EIO`), the loop is a hot 100 % CPU spin with no
log line, which on the GPS host is also the PPS host.

**Fix specification:** In `ReadTimeout`, treat `n == 0 && err == nil` as
`io.EOF` wrapped in a `serial:` error so `NMEA.Run` takes the `reopen` path.
Add a test using a pipe whose write end is closed.

**Verification:** Unit test with `os.Pipe()`; on hardware, unplug a USB GPS
and confirm `GPS serial device unavailable` followed by reopen, with no CPU
spike.

---

## RF5X-014 — `SIGHUP` is not handled, so `kill -HUP` terminates the daemon without a clean shutdown

**Severity:** Low

**Location:** `cmd/carillon/main.go` line 350 (`signal.NotifyContext(...,
SIGINT, SIGTERM)`); no other `signal` use anywhere. Design: `DESIGN.md` §9
"`SIGHUP` is not a reload (restart is cheap…)", §16 D11.

**Problem:** Go's default action for an unhandled `SIGHUP` is to exit the
process immediately. An operator who sends `HUP` expecting a reload (or a
no-op, as the design implies) gets an abrupt exit: no drift file write, the
slew transient left in the kernel (RF5X-004), sockets closed by the OS. Under
`daemon(8)` a terminal hangup on an interactively started instance has the
same effect.

**Fix specification:** Either `signal.Ignore(syscall.SIGHUP)` with a log
line the first time it arrives, or add it to `NotifyContext` so it is a
graceful stop. Document the choice in the rc.d/systemd comments
(`reload` → restart). Must not change: `SIGTERM`/`SIGINT` handling.

**Verification:** Start the daemon in a test harness, send `SIGHUP`, assert
it either keeps running or exits 0 with the drift file written.

---

## RF5X-015 — `omitempty` on `time.Time` fields does nothing, so the JSON carries year-1 timestamps the design says should be absent

**Severity:** Low

**Location:** `internal/control/protocol.go` lines 66 (`LastPulse`), 73
(`LastSentence`), 199 (`LeapExpiry`), 237 (`LastRx`); the same structs are
embedded verbatim in `/api/v1/status` by `internal/monitor/model.go`.
Compare lines 115–116, where `LastRequest`/`LastServed` were correctly made
pointers with a comment explaining why.

**Problem:** `encoding/json` never omits a zero struct, so a source that has
never received a reply serialises `"last_rx": "0001-01-01T00:00:00Z"`, a
daemon without a leap file emits `"leapfile_expires": "0001-01-01T00:00:00Z"`,
and `RefTime`/`Now` likewise. `DESIGN.md` §10.2/§10.3 say absent-rather-than-
zero is the convention and the schema is `carillon.status.v1`, so clients
written against the doc will parse year 1 as a real timestamp (the iPhone
client mentioned in §10.3 would show "last reply: 2025 years ago").
`carillonctl` already guards with `IsZero()`, which is why nobody noticed.

**Fix specification:** Change those four fields (and `Tracking.RefTime`) to
`*time.Time` using the existing `optionalTime` helper, or add
`MarshalJSON` on a small `optionalTime` type. This is an additive-compatible
change under the v1 rule only if clients tolerate the field disappearing;
since the documented contract is "absent", do it now before external
clients exist. Must not change: field names.

**Verification:** Extend `TestServerRoundTrip`/`TestSnapshotOf` to decode
into `map[string]any` and assert the keys are absent for a fresh engine.

---

## RF5X-016 — Clustering stops at a fixed 3 survivors, not at `min_survivors` as `DESIGN.md` §6.3 and the example config say

**Severity:** Low (documentation/behaviour drift)

**Location:** `internal/discipline/select.go` — `cluster()` line 375
(`for len(surv) > ClusterMin`, `ClusterMin = 3` at line 19); `Select()` line
232 uses `minSurvivors` only to decide whether a system source is declared.
`DESIGN.md` §6.3 ("… and more than `min_survivors` (default 1; set 3 on a
host with many upstreams) remain") and `deploy/carillon.toml.example` line
233 ("Smallest number of sources the cluster algorithm keeps").

**Problem:** The code follows RFC 5905 (`NMIN = 3`), which is the right
choice, but the design and the example describe a different knob: with the
documented `min_survivors = 3` and only two upstreams the daemon never
synchronises, and with the default 1 the reader expects clustering to be
able to reduce to one survivor, which it never does. The design text also
says the discard rule compares against "that survivor's own jitter" while the
code (and RFC) use the minimum peer jitter.

**Fix specification:** Fix the documents: §6.3 should say clustering keeps at
least 3 (RFC `NMIN`) and that `min_survivors` is the number of survivors
required before a system source is chosen; the example's comment likewise.
No code change. **Verification:** doc review.

---

## RF5X-017 — A `RATE` kiss's poll is not honoured as a new minimum

**Severity:** Low

**Location:** `internal/source/ntp.go` — `handleKiss()` lines 337–345 sets
`n.poll = max(poll+1, pkt.Poll)` clamped to `[PollMin, PollMax]`; `hit()` line
402 then calls `adaptPoll`, which may lower the poll again when `|θ| ≥ 4ψ`.
Design: `DESIGN.md` §5.4 "honour the packet's poll field as the new minimum".

**Problem:** After a RATE kiss the next noisy update can step the poll back
below what the server demanded, producing another RATE, and so on — the
client oscillates at the server's limit instead of backing off. When the
server's demand exceeds `PollMax` it is silently clamped, so the client keeps
violating it. RFC 8633 §5.4 expects clients to honour the KoD poll.

**Fix specification:** Keep a per-source `kodMinPoll` (reset only on
re-resolution); make `adaptPoll`'s lower bound `max(cfg.PollMin, kodMinPoll)`;
if the demanded poll exceeds `PollMax`, raise the effective maximum to it
and log once. Must not change: DENY/RSTR handling.

**Verification:** Extend `TestKissRATE`: after the kiss, feed a reply with a
large offset and assert the poll does not drop below the kiss value.

---

## RF5X-018 — Client sockets are unconnected, so a dead server costs a 2 s timeout per poll instead of an immediate `ECONNREFUSED`

**Severity:** Low

**Location:** `internal/source/ntp.go` — `exchange()` line 559
(`net.ListenUDP(network, nil)`) and 584 (`WriteToUDPAddrPort`). Design:
`DESIGN.md` §5.4 "a fresh UDP socket per request … *connected* to the server
address".

**Problem:** ICMP port-unreachable errors are only delivered to connected UDP
sockets. With an unconnected socket every poll to a host that is up but not
running NTP (the exact case in the acceptance log: `gummi` down for a day)
waits the full timeout, the source goroutine spends 2 s of every poll
blocked, `iburst` takes 8 s to fail instead of milliseconds, and the log
throttle had to be invented to cope with the volume. `sameEndpoint` filtering
is still needed for the connected case only when the server replies from a
different address, which the design says to drop anyway.

**Fix specification:** Use `net.DialUDP(network, nil, remote)` and
`conn.Write`; keep the `from` check via `ReadMsgUDPAddrPort` (a connected
socket still reports the peer). Map `ECONNREFUSED` to a distinct
`errUnreachable` that counts as a miss without waiting. Must not change: the
nonce/origin check, kernel timestamping (`sockts.Enable` works on a connected
socket), the ephemeral-port randomisation.

**Verification:** `TestQueryTimeout` against a closed loopback port must now
return quickly with the new error; add a test asserting `< 100 ms`.

---

## RF5X-019 — Crash between `CreateTemp` and `Rename` leaves `.drift-*` files that accumulate forever

**Severity:** Low

**Location:** `internal/engine/engine.go` — `writeDrift()` lines 178–209.

**Problem:** A SIGKILL or power loss during the hourly write leaves a
`.drift-NNNN` file in `/var/db/carillon`; nothing ever removes them. Harmless
individually, but the directory is also the stats directory's parent on both
hosts and a year of unclean shutdowns leaves clutter that `-check` will not
explain.

**Fix specification:** On startup (in `initialFrequency` or `New`), glob
`filepath.Join(dir, ".drift-*")` and remove matches older than a minute, at
DEBUG. Must not change: the atomic write itself.

**Verification:** Unit test that plants a stale temp file and asserts it is
gone after `New`.

---

## RF5X-020 — Precision is measured as the minimum non-zero delta, which reports 2^-30 on any modern host and makes every downstream floor unrealistic

**Severity:** Low

**Location:** `internal/clock/precision.go` — `measurePrecision()` lines
18–29. Consumers: `discipline.NewFilter(precision)` jitter floor,
`LoopConfig.Precision` jitter floor, `exchange()` negative-delay tolerance
(`ntp.go` line 652), the packet `Precision` field.

**Problem:** Two back-to-back `clock_gettime` reads on a TSC-backed clock can
differ by 1 ns, so the loop finds `minDelta = 1 ns` → `-30` regardless of the
actual ~20–50 ns read cost or the µs-level real resolution of a VM clock.
RFC 5905 §7.3/ntpd measure the *time to read the clock* (the typical
increment), not the smallest observed difference. A `-30` precision makes the
filter jitter floor 1 ns, the loop's popcorn threshold `3·max(jitter, 1 ns)`,
and the negative-delay tolerance 2 ns; it also advertises a resolution to
clients that no host has. Both deployed hosts log `precision_log2` at startup
so the current values can be checked.

**Fix specification:** Measure like ntpd: loop until the clock changes, take
the delta between successive *changes*, repeat `precisionSamples` times and
use the median (or mean) rather than the minimum; keep the `[-30, -6]` clamp.
Must not change: `ntp.PrecisionFromSeconds` and the clamp range.

**Verification:** Existing `TestMeasurePrecision` cases (stepping clocks)
still hold with the median; on hosts expect around `-25`…`-23` instead of
`-30`.

---

## RF5X-021 — Configuration validation gaps: GPS-only keys accepted on `type = "pps"`, and FreeBSD `-check` rejects a separate PPS tty that the daemon would use

**Severity:** Low

**Location:** `internal/config/config.go` — `Validate()` lines 545–557
(`pps` type validates only `edge`/`offset`); `internal/config/
refclock_check_freebsd.go` lines 18–25 (`gps` with `HasPPS()` requires
`dcd`/`cts`); `cmd/carillon/main.go` lines 218–221 (any absolute `pps` path
is opened as its own device on every platform).

**Problem:** (a) `[[refclock]] type = "pps"` with `baud = 9600` or
`nmea_offset = 0.15` is accepted and the keys silently ignored, contradicting
the strict-config promise (§9 "unknown keys are errors"). (b) On FreeBSD a
`gps` block with `pps = "/dev/cuau1"` (a second callout tty carrying PPS
only, which `main.go` supports and which is the natural wiring for a USB
GPS plus a UART PPS) fails `-check` with "must be dcd or cts", while on Linux
the same block passes. The design (§5.3) says only that the *same* tty is used
on FreeBSD; it does not forbid a separate one.

**Fix specification:** (a) In `Validate`, fail when `Type == "pps"` and any of
`Baud`, `PPS`, `PPSEdge`, `PPSOffset`, `NMEAOffset`, `Sentences` is set.
(b) In the FreeBSD check, accept an absolute `pps` path, run the `pps_mode`
sysctl check against *that* device's unit instead of `Device`, and keep the
pin check for `dcd`/`cts`. Must not change: existing valid configs.

**Verification:** Add validate cases for (a); for (b) a FreeBSD-tagged test
with a fake sysctl is impractical — cover with a table test on the path
selection and document the manual check.

---

## RF5X-022 — Two divergent `splitHostPort` implementations

**Severity:** Low (maintainability, with one behaviour gap)

**Location:** `internal/config/config.go` lines 418–470 and
`internal/source/poll.go` lines 64–93.

**Problem:** `config` validates addresses with one parser and `source`
re-parses the same string with another. They disagree on `"[2001:db8::1]x"`
(config rejects, source rejects via `net.SplitHostPort`) and would drift on
future changes; `NewNTP` can therefore fail at *startup* for an address
`-check` accepted, exiting with `exitUsage` after the clock and devices are
already open. Today the two agree on every case in both test tables only by
construction.

**Fix specification:** Export one parser (e.g. `config.ParseServerAddress`
returning `(host, port)`) and have `source.NewNTP` take host and port rather
than the raw string, so a config that passed `-check` cannot fail in
`NewNTP`. Must not change: accepted syntax.

**Verification:** Delete one test table, keep the union of cases on the
survivor.

---

## RF5X-023 — `versions` histogram and `last_request` are recorded before rate limiting and authentication

**Severity:** Low (metric semantics)

**Location:** `internal/server/responder.go` — `Handle()` lines 184–185,
before the limiter at 187 and the MAC check at 198. `DESIGN.md` §10.4
describes `carillon_server_client_version_total` as "accepted requests by
client protocol version" and `last_request` as "last valid client request".

**Problem:** A request that is then rate-limited or fails `require_key` is
still counted in the version histogram and refreshes `last_request`, so a
spoofed flood shows up as accepted-version traffic and `last_request`
advances while nothing is being served. Not a partition breaker (those are
the `result` counters) but it makes the histogram misleading exactly during
the abuse it is meant to characterise.

**Fix specification:** Move both increments after the auth checks, next to
`served`; or rename the metric help text to "requests that passed the ACL".
Must not change: metric names.

**Verification:** Extend `TestVersionHistogram` with a rate-limited request
and assert the histogram is unchanged.

---

## RF5X-024 — IPv6 rate limiting is per /128, so one /64 holder has unlimited fresh buckets

**Severity:** Low

**Location:** `internal/server/ratelimit.go` — `allow()` keys on the full
address (line 60 `addr.Unmap()`).

**Problem:** Every residential IPv6 customer controls at least a /64; each
new source address gets a fresh `rate_burst` of tokens and an LRU slot. A
single host can therefore draw `rate_burst` replies per address indefinitely
and churn the `max_clients` table, evicting real clients. ntpd's MRU list
and chrony's client log have the same weakness, but chrony documents it and
this daemon is being pointed at the pool with `2000::/3`.

**Fix specification:** Key IPv6 buckets on the /64 prefix (`addr.Prefix(64)`)
and IPv4 on the /32; expose the v6 prefix length as `[serve]
rate_limit_v6_prefix` (default 64). The clients gauge then counts /64s for
v6, which should be said in the metric help. Must not change: the v4
behaviour, the KoD throttle.

**Verification:** Handler test: 20 addresses in one /64 with `rate_burst = 8`
must share one bucket.

---

## RF5X-025 — The loop's first-update jitter equals the whole initial offset, inflating reported jitter for ~30 updates

**Severity:** Low

**Location:** `internal/discipline/loop.go` — `Update()` lines 187–192
(`lastOffset` is 0 on the first update, so `d = |offset|` and `Jitter = d`).

**Problem:** A 100 ms initial offset makes `Jitter` start at 100 ms and decay
by 7/8 in variance per update: still 26 ms after 20 updates. It feeds
`carillon_jitter_seconds`, `carillonctl tracking`, the kernel `esterror`, and
the popcorn gate (which only arms when SYNCED, so the practical effect is
cosmetic and on `esterror`). ntpd seeds `clock_jitter` from precision and
averages the first sample in, giving ~35 ms in the same case, and steps do not
update it at all.

**Fix specification:** On the first update set `Jitter = max(precision,
d/√8)` (i.e. run the same exponential average from the precision floor), and
skip the jitter update on the update that follows a step (`lastOffset` is
meaningless then). Must not change: the averaging constant.

**Verification:** Loop test asserting first-update jitter ≈ 35 ms for a
100 ms offset.

---

## RF5X-026 — A PPS sequence counter that goes backwards produces a 4-billion-slot gap

**Severity:** Low

**Location:** `internal/refclock/pps.go` — `accept()` lines 270–285 and
`accountSequence()` lines 367–389 (`delta := s.Sequence - p.previousSeq`
unsigned; `misses := int(delta - 1)`; `p.slots += additional`;
`r.Gaps += uint64(misses)`).

**Problem:** `reopen()` clears `havePrevious`, so an ordinary device reopen is
safe, but a kernel-side counter reset without a reopen (Linux `pps_ldisc`
re-attached by `ldattach` restarting, a `/dev/ppsN` recreated under the
same name, a FreeBSD `PPS_IOC_DESTROY`/`CREATE` by another process on the
same tty) yields `delta ≈ 2^32`, `misses ≈ 4.29e9`, `Gaps` jumps by that
much (a monotonic Prometheus counter that will never look right again),
`slots` overflows the emit cadence, and the interval check rejects the
sample as a glitch anyway.

**Fix specification:** If `delta > 3600` (an hour of missed pulses is a
device restart, not a gap), treat it like a reopen: reset `havePrevious`,
window and slots, count one `Glitches`, log once. Must not change: normal gap
accounting.

**Verification:** Unit test feeding sequence 1000 then 5 and asserting
`Gaps` unchanged and `Glitches` incremented.

---

## RF5X-027 — systemd unit grants write access to all of `/run`

**Severity:** Low

**Location:** `deploy/systemd/carillon.service` line 48
(`ReadWritePaths=/var/lib/carillon /run`) together with line 37
(`RuntimeDirectory=carillon`).

**Problem:** `RuntimeDirectory=` already makes `/run/carillon` writable under
`ProtectSystem=strict`; listing `/run` opens every other daemon's runtime
directory to a compromised carillon. Nothing in the code writes outside
`/run/carillon` (control socket) and `/var/lib/carillon` (drift, stats).

**Fix specification:** `ReadWritePaths=/var/lib/carillon` only. Verify on
`gummi` that the control socket is still created (it is, via
`RuntimeDirectory`). Must not change: `RuntimeDirectoryMode`.

**Verification:** `systemctl restart carillon && carillonctl version`.

---

## RF5X-028 — Documentation uses a routable placeholder hostname

**Severity:** Low

**Location:** `deploy/README.md` lines 275–276; `deploy/ACCEPTANCE.md` lines
323–324 (`carillon query … server.example.net`, including the authenticated
form with `-keys`).

**Problem:** `example.net` resolves. The house rule for anything an operator
may paste with credentials is RFC 2606 `.invalid`; the authenticated example
sends a real CMAC (not the key) to whatever answers, which is harmless but
the rule is the rule and `DESIGN.md` already uses `home.tunnel.invalid`.

**Fix specification:** Replace with `server.invalid`. **Verification:** grep.

---

## RF5X-029 — Statistics files accumulate in one flat directory with no retention

**Severity:** Low

**Location:** `internal/stats/writer.go` — `file()` lines 250–280 creates
`<kind>.<day>.tsv` in `Dir` and nothing deletes anything.

**Problem:** Four files a day (`loop`, `sources`, `pps`, `server`) is ~1,500
files a year in `/var/db/carillon/stats`; `sources.tsv` writes one row per
source per loop update and `pps.tsv` one per pulse (86,400/day). Over a
multi-year soak this is tens of GB with no age-out and no per-day directory
to `rm -rf`, which is the failure mode the house `YYYY/MM/DD` convention
exists to avoid.

**Fix specification:** Write into `Dir/YYYY/MM/DD/<kind>.tsv` (create the
directories at rotation) and add `[stats] keep_days` (default 0 = keep
forever) that removes day directories older than the limit at rotation time.
Update `deploy/README.md` and the example. Must not change: TSV columns or
the header line.

**Verification:** `TestRecorderRotatesAtUTCMidnight` extended to check the
directory layout and that a planted old day is removed when `keep_days = 1`.

---

## RF5X-030 — `readLine` compares `err.Error() == "EOF"`

**Severity:** Low

**Location:** `internal/control/client.go` line 130.

**Problem:** String-comparing an error works for `io.EOF` today but breaks
for any wrapped EOF (`net` wraps read errors in `*net.OpError` on some
paths), turning a complete unterminated response into "reading response:
EOF". Use `errors.Is(err, io.EOF)`.

**Fix specification:** One-line change; add `io` import.
**Verification:** existing control tests.

---

## RF5X-031 — A non-prefer PPS becomes the system source by sorting on stratum 0, contrary to §6.3

**Severity:** Low

**Location:** `internal/discipline/select.go` — `synch()` line 145 (stratum
dominates), `Select()` lines 227 and 255–257 (`survivors[0]` when no prefer).
Design: `DESIGN.md` §6.3 says the PPS override applies to a PPS that is
"also `prefer`"; otherwise "the survivor with the smallest λ".

**Problem:** A qualified PPS has stratum 0 and a tiny distance, so it always
sorts first and becomes the system source even with `prefer = false`; the
combined offset is then a distance-weighted mean dominated by the PPS
anyway. The visible consequences are that the loop's time constant follows
the PPS poll (4–7) rather than the numbering source's, that the advertised
refid is `PPS` at stratum 1 for a source the operator did not mark
`prefer`, and that `noselect`-less "monitor" PPS configurations are
impossible. This matches ntpd's behaviour for a refclock, so it may be
intended; the design should say so.

**Fix specification:** Either document that a qualified PPS is always the
system source unless `noselect`, or exclude non-prefer PPS sources from the
system-source choice (they still contribute to combine). No behaviour
change is required; pick one and write it down.

**Verification:** doc review, or a `select_test.go` case if the second
option is chosen.

---

## RF5X-032 — A source whose goroutine exits is deleted from status instead of shown unreachable

**Severity:** Low

**Location:** `internal/engine/engine.go` — `sourceExited()` lines 291–308
(`delete(e.sources, name)` and `sys.RemoveSource`). Design: `DESIGN.md` §14
"Serial device vanishes → source goroutine exits with error; engine marks
unreachable; reopen retried with backoff".

**Problem:** In practice no production source returns from `Run` (all three
reopen internally), so the path is only reachable from tests. If it ever
fires, the source disappears from `carillonctl sources`, `/api/v1/status`,
and every per-source metric series (Prometheus sees the series vanish
rather than reach dropping to 0), and there is no restart. Either the design
or the code should change; the cheaper is the design, plus making the
engine restart a source that returns an error with backoff, since that is
what §14 promises.

**Fix specification:** Keep the source registered with reach 0 and a
`LastError`, restart `Run` after a backoff (1 s doubling to 1 min), and log
each attempt. Must not change: the `Source` interface.

**Verification:** `TestEngineSourceExitRemovesIt` becomes
"…MarksItUnreachableAndRestarts".

---

## RF5X-033 — Shutdown has no deadline; a stuck auxiliary goroutine hangs exit

**Severity:** Low

**Location:** `cmd/carillon/main.go` lines 395–398 (`eng.Run`, `stopStats`,
`stop`, `wg.Wait()` with no timeout). Design: `DESIGN.md` §12 "`main` waits
with a 5 s deadline".

**Problem:** `control.Server.handle` writes replies without a deadline for a
`waitsync` with `timeout = 0` (`conn.SetDeadline(time.Time{})` at
`server.go` line 137); a client that connected, sent `waitsync`, and stopped
reading holds `s.wg` open and `main` never exits, so `service carillon
stop` hangs until `daemon(8)`/systemd's `TimeoutStopSec` kills it — after
which the drift file has already been written (that happens before
`wg.Wait`), but RF5X-004's base-frequency restore, once added, would be
skipped.

**Fix specification:** Wrap the final `wg.Wait()` in a 5 s deadline
(`select` on a done channel and `time.After`), log which component did not
stop, and exit anyway. Give `reply()` a write deadline of a few seconds.
Must not change: the drift write ordering.

**Verification:** Control test that opens a `waitsync 0` connection and never
reads; daemon shutdown must complete within 6 s.

---

## RF5X-034 — Refid `HOLD` is advertised after a panic refusal, not only after holdover expiry

**Severity:** Low (cosmetic, but it misleads diagnosis)

**Location:** `internal/discipline/system.go` — `Status()` lines 383–391
(`StateUnsynced` with `haveUpdate` → `KissHOLD`); `reselect()` line 307 sets
`StateUnsynced` on `PanicRefused`. Design: `DESIGN.md` §7.4 (`HOLD` after
holdover expiry).

**Problem:** A host whose offset exceeds `panic` answers with refid `HOLD`,
which the design and ntpd/chrony users read as "was synced, lost sources",
sending the operator to look at network reachability instead of at a
wildly wrong clock.

**Fix specification:** Track the reason (`unsyncedReason`) and advertise
`INIT` before any update, `HOLD` after holdover expiry, and a new `PANC`
(or reuse ntpd's `STEP`? no — use a new code) after a panic refusal;
document it in §7.4. **Verification:** `TestSimPanicRefused` asserts the
refid.

---

## RF5X-035 — A request carrying a MAC with an unknown key id is answered unauthenticated instead of with a crypto-NAK

**Severity:** Low (interoperability)

**Location:** `internal/server/responder.go` — `Handle()` lines 198–207:
an unknown `mac.KeyID` leaves `replyKey == nil` and the request is served
with a 48-byte reply. `DESIGN.md` §7.2 step 8 says "if the key id is known,
verify", which the code follows.

**Problem:** ntpd and chrony answer a request they cannot authenticate with
a crypto-NAK (48-byte header plus a 4-byte zero key id, which `ntp.Decode`
already recognises on the client side). An ntpd/chrony client configured
with a key the server does not have currently gets a plain reply, which it
drops as "unauthenticated" with no explanation, whereas a NAK tells it (and
`ntpq -p`) that the *key* is the problem. The NAK is 52 bytes against a
68-byte request, so the length invariant holds.

**Fix specification:** When `mac != nil && !mac.IsCryptoNAK()` and the key id
is unknown (or the MAC fails), reply with the header plus a zero key id,
count `bad_auth`, and keep the reply unauthenticated. Must not change: the
reply-length invariant (add a fuzz assertion that a NAK is only sent when the
request was ≥ 52 bytes).

**Verification:** Handler test: unknown key id → 52-byte reply whose trailer
decodes as `IsCryptoNAK()`.

---

## RF5X-036 — "preferred source is not usable" is logged at ERROR on every start

**Severity:** Low (log noise)

**Location:** `internal/discipline/select.go` line 233 (`PreferLost =
preferConfigured` whenever there is no survivor); `internal/discipline/
system.go` lines 267–274; `internal/engine/engine.go` line 398 (ERROR).

**Problem:** Before the prefer source's first reply there are no survivors,
so `PreferLost` flips true and the engine logs an ERROR at every daemon
start, then INFO "preferred source is back in charge" a few seconds later.
An operator alerting on ERROR lines gets a false page per restart.

**Fix specification:** Do not report `PreferLost` until the prefer source has
been reachable at least once (or until any source has become a survivor);
log the transition at WARN, not ERROR, when it happens within the first
poll interval. Must not change: the `prefer_lost` status field semantics
once running.

**Verification:** `TestSimPreferLost` asserts no `EventPreferLost` before the
first survivor.

---

# Summary

| ID | Severity | Area | Title | Status |
|---|---|---|---|---|
| RF5X-001 | Critical | refclock/pps | Spike gate rejects every pulse once the loop slews; window never refreshes; PPS lost until restart | Verified |
| RF5X-002 | High | engine, source, discipline | Measurements queued before a step/leap are applied after it; double step | Verified |
| RF5X-003 | High | server | Directed-broadcast destinations answered; FreeBSD replies from a broadcast source | Verified (code); wire proof pending |
| RF5X-004 | High | engine | Shutdown leaves the slew transient (up to ±500 ppm) in the kernel | Verified |
| RF5X-005 | Medium | engine, discipline | Leap transition → HOLDOVER → SETTLING; server LI=3 for three loop updates | Verified |
| RF5X-006 | Medium | discipline | SETTLING counts filter updates; restarted server unsynced for minutes | Operational evidence |
| RF5X-007 | Medium | discipline | Qualified prefer PPS bypasses intersection; wrong edge undetected | Needs investigation (hardware) |
| RF5X-008 | Medium | server | Rate limit before MAC check starves the authenticated association | Code |
| RF5X-009 | Medium | server, deploy | `recv_buffer` > `kern.ipc.maxsockbuf` aborts startup on FreeBSD | Needs investigation (twocom) |
| RF5X-010 | Low | discipline | `settled` not reset by a second step | Verified |
| RF5X-011 | Low | discipline | `Tick` assumes 1 s; `Update` drops the transient | Code |
| RF5X-012 | Low | discipline | Filter dispersion ignores empty stages (RFC deviation) | Code |
| RF5X-013 | Low | serial, refclock | Zero-byte read would spin the NMEA loop | Needs investigation |
| RF5X-014 | Low | cmd | `SIGHUP` unhandled → abrupt exit | Code |
| RF5X-015 | Low | control, monitor | `omitempty` on `time.Time` emits year-1 timestamps | Code |
| RF5X-016 | Low | docs | Cluster floor is 3, not `min_survivors` | Doc |
| RF5X-017 | Low | source | RATE kiss poll not a new minimum | Code |
| RF5X-018 | Low | source | Unconnected client socket; no ICMP unreachable | Code |
| RF5X-019 | Low | engine | `.drift-*` temp files after a crash | Code |
| RF5X-020 | Low | clock | Precision measured as min delta → 2^-30 | Code |
| RF5X-021 | Low | config | GPS keys accepted on `pps`; FreeBSD check rejects a separate PPS tty | Code |
| RF5X-022 | Low | config, source | Two `splitHostPort` implementations | Code |
| RF5X-023 | Low | server | Version histogram counted before rate limit/auth | Code |
| RF5X-024 | Low | server | IPv6 rate limiting per /128 | Code |
| RF5X-025 | Low | discipline | First-update jitter equals the whole offset | Code |
| RF5X-026 | Low | refclock | Backwards PPS sequence → 4e9-slot gap | Code |
| RF5X-027 | Low | deploy | systemd `ReadWritePaths=/run` | Config |
| RF5X-028 | Low | docs | `server.example.net` placeholder | Doc |
| RF5X-029 | Low | stats | Flat directory, no retention | Code |
| RF5X-030 | Low | control | `err.Error() == "EOF"` | Code |
| RF5X-031 | Low | discipline, docs | Non-prefer PPS always system source | Doc/code |
| RF5X-032 | Low | engine | Exited source deleted rather than marked unreachable | Code |
| RF5X-033 | Low | cmd, control | No shutdown deadline | Code |
| RF5X-034 | Low | discipline | `HOLD` refid after panic refusal | Code |
| RF5X-035 | Low | server | No crypto-NAK for unknown key id | Code |
| RF5X-036 | Low | discipline, engine | Prefer-lost ERROR on every start | Code |

Counts: Critical 1, High 3, Medium 5, Low 27.

# Suggested fix order

The order accounts for shared code paths so that a later fix does not have to
re-touch an earlier one.

1. **RF5X-001** (PPS spike gate). Self-contained in `refclock/pps.go`; blocks
   the pending live PPS acceptance. Do it first and re-run the hardware
   probe.
2. **RF5X-004** (base frequency on exit). Five lines in `engine.Run`; no
   dependencies; protects every restart done for the fixes below.
3. **RF5X-002** (measurement generation). Touches `Measurement`, all three
   sources, the engine and `System.Update`; do it before any other change to
   `System` so RF5X-005/006/010 are built on the new contract. Include
   RF5X-012 (filter dispersion) in the same change set, since together they
   define what a "first sample" is worth.
4. **RF5X-005, RF5X-010, RF5X-006** (state machine), in that order, in
   `system.go`; they share `reselect()`/`settled`. Update `DESIGN.md` §6.5
   once for all three. RF5X-034 and RF5X-036 are small edits in the same
   file and can ride along.
5. **RF5X-003** (directed broadcast) and **RF5X-009** (FreeBSD `SO_RCVBUF`):
   both in `server` platform code, independent of the discipline work; can be
   done in parallel with step 4. RF5X-023, RF5X-024, RF5X-035 are further
   `responder.go`/`ratelimit.go` edits best batched with RF5X-008.
6. **RF5X-008** (auth before limiter for `require_key`), then the batched
   server Lows above.
7. **RF5X-007** (PPS agreement check) after step 4 has settled the state
   machine, since it adds a new falseticker path through `reselect`.
8. **RF5X-011, RF5X-025** (loop) — independent of the above but easiest to
   validate once the simulation gains the PPS case from step 1.
9. Remaining Lows in any order: RF5X-013, 014, 015, 017, 018, 019, 020, 021,
   022, 026, 027, 028, 029, 030, 031, 032, 033, 016.

After steps 1–4, repeat `deploy/ACCEPTANCE.md`'s checklist on both hosts and
add the live PPS run; after step 5, re-run the six-datagram counter proof
plus one broadcast-destination datagram.
