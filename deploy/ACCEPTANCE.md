# carillon deployment acceptance

This file records real-host acceptance separately from the deterministic test
suite. Repeat the checklist after init-script, privilege, socket, clock, or
wire-protocol changes and add a dated result without including key material.

## Initial two-host deployment — 2026-08-23

Revision: `7de8ca2` (`Complete NTP server milestone`)

Topology:

- `gummi`, Fedora 43 amd64, takes three public NTP sources and serves the LAN.
- `twocom`, FreeBSD 15 amd64, prefers `gummi` over two public survivors.
- The `twocom` → `gummi` association requires AES-128-CMAC key 1. The shared
  key is stored only in the hosts' mode-`0600` keys files.
- Both hosts listen on IPv4 and IPv6 UDP/123 with default-deny ACLs restricted
  to the in-house prefixes.

Results:

- Cross-compiled static Linux and FreeBSD binaries reported version
  `7de8ca2`; each host passed `carillon -check` before service replacement.
- `gummi` runs as the unprivileged `carillon` systemd user with only
  `CAP_SYS_TIME` and `CAP_NET_BIND_SERVICE`. `ntpd`, `chronyd`, and
  `systemd-timesyncd` are disabled.
- `twocom` runs as the unprivileged `carillon` rc.d user under `mac_ntpd(4)`.
  `ntpd_enable=NO`, `carillon_enable=YES`, and the MAC policy uid is persistent
  in the boot-loader configuration.
- Both hosts reached `synced` without a clock step. `twocom` selected the
  authenticated LAN source at roughly 0.2 ms round-trip while retaining both
  public sources as survivors.
- An authenticated query from `twocom` to `gummi` and an unauthenticated,
  ACL-authorized query from `gummi` to `twocom` both succeeded with kernel RX
  timestamps. The latter measured 0.295 ms delay and -0.107 ms offset.
- IPv4 and IPv6 listeners are owned only by `carillon` on both hosts. Server
  counters record allowed, denied, and unsynchronized requests as expected.
- FreeBSD rc.d stop/start writes structured output to syslog. Linux systemd
  stop/start wrote the drift file, restarted from the saved frequency, and
  reacquired synchronization without a step.

These hosts intentionally remain on carillon for long-term observation. This
is an initial functional acceptance, not the later PPS/GPS or 24-hour accuracy
acceptance described in `DESIGN.md` §13.

## PPS kernel-path probe — 2026-08-23

Revision: `88de18c` (`Integrate PPS reference clock`)

- `gummi` has `/dev/pps0` backed by `/dev/ttyS0`; `pps_ldisc` is loaded.
- `twocom` has `/dev/cuau0` and `/dev/cuau1`, with DCD capture enabled by
  `dev.uart.0.pps_mode=2` and `dev.uart.1.pps_mode=2`.
- The gated `internal/pps` hardware test opened each interface, validated its
  capabilities, configured `assert` timestamps, and entered `PPS_FETCH`.
- All three fetches ended with the expected three-second timeout because no
  input had an active pulse (`gummi` remained at sequence 0). No PPS
  `[[refclock]]` was enabled, and the NTP-only production configs were left
  unchanged.

This verifies both real kernel ABI paths through the no-pulse failure case.
M3 stratum-1 acceptance remains pending until a serial input is wired to a
live 1 Hz source.

## M3 regression deployment — 2026-08-23

Revision: `a5e3f65` (`Complete PPS refclock milestone`)

- Both hosts passed the new strict config check with three upstreams, zero
  enabled refclocks, and two listeners. The previous daemon/control binaries
  remain installed as `.prev` rollback copies.
- `gummi` synchronized eight seconds after restart; `twocom` synchronized
  after 3 minutes 35 seconds. Both restarted from their drift files and made
  zero clock steps.
- `carillonctl refclock` returns an empty table on each host, confirming the
  dormant M3 path does not affect an NTP-only configuration.
- The authenticated `twocom` → `gummi` query and the ACL-authorized `gummi` →
  `twocom` query succeeded with kernel receive timestamps after the upgrade.
- Both hosts own IPv4 and IPv6 UDP/123 exclusively. The former Linux time
  services remain disabled; FreeBSD retains `ntpd_enable=NO` and
  `carillon_enable=YES`.

The longer FreeBSD settling interval came from waiting for a third fresh
lowest-delay filter estimate. It remained inside the documented 300-second
upgrade check and is now part of the long-term soak baseline.

## Failure-only re-resolution and opened `[serve]` ACL — 2026-08-24 (UTC)

Revisions: daemon `eb89ae4` (`Re-resolve upstream hostnames only on failure`),
`carillonctl` `462a157` (`Make carillonctl waitsync follow a service restart`).

Why: after 100 minutes on `a5e3f65` the soak showed the 1024 s periodic
re-resolution changing every pool source's server about every 17 minutes
while its clock filter kept the previous servers' samples. On `gummi` the
three sources disagreed by 6 ms, two of them showed 22–24 ms jitter, the
system source changed seven times, and root dispersion sat at 33 ms; `twocom`
followed at 39 ms. `twocom` also counted 184 denied requests with `Denied`
at zero on `gummi`.

Changes:

- Both hosts run daemon `eb89ae4`; `carillonctl` `462a157` was installed
  afterwards without a daemon restart (the daemon did not change between the
  two revisions). Previous binaries remain as `.prev` copies.
- `twocom` `[serve] allow` is now `172.19.0.0/16` and `2000::/3` (it was the
  LAN and its own `/64`); the previous file is `carillon.toml.prev`. Per-client
  rate limiting (8 pps, burst 16, KoD) is unchanged. IPv4 stays LAN-only;
  `gummi` is unchanged.

Results:

- Both hosts passed `-check` and restarted from their drift files with zero
  clock steps. `twocom` reached `synced` in 6 s; `gummi` took 5 m 25 s to
  satisfy |θ| < 4ψ for three updates with pool servers 2 ms apart, past the
  300 s `waitsync` used as the upgrade check. The settling time depends on
  which pool servers the first lookup returns.
- Twelve minutes after restart neither host had re-resolved any name: each
  source kept its first address, and `gummi`'s root dispersion had fallen
  from 33 ms to 13 ms.
- `Denied` on `twocom` stayed at 0. A 240 s capture on `lagg0` showed the
  admitted traffic: `2607:ff50:0:20::ffff` (`junia`, whose reverse DNS still
  reads `kraftwerk.packetexport.com`, at SBA Edge) polling
  `2603:8000:ae00:d304::123` every 35 s from source port 123 — 1.75 requests
  a minute, the whole of the earlier denial rate. The LAN clients in the
  same window were `172.19.1.231` (every 60 s), `.93`, `.247`, `.252`,
  `.253`, and `.254`, all already allowed. No other Internet client appeared.
- Operator errors worth not repeating: the two restarts were issued 30 s
  apart, so `twocom` polled `gummi` while it was still settling, discarded
  the stratum-16 replies (`preferred source is not usable`), synchronized to
  its public survivors, and then had to slew +7 ms when `gummi` returned;
  its frequency estimate moved from +5.5 ppm to +41 ppm within twelve
  minutes. `README.md` now says to restart the upstream first. On `gummi`,
  `carillonctl waitsync 300` issued right after `systemctl restart` failed
  because the socket did not exist yet; `waitsync` now waits for it.

Watch during the soak:

- `twocom`'s frequency should return to the +24 to +27 ppm its drift file
  held before the transient. Millisecond offsets at poll 4–6 on the LAN
  association become tens of ppm of frequency motion because the loop time
  constant is 4·P; if the estimate keeps wandering once the transient has
  passed, raise `poll_max` on the `gummi` association or revisit the gain.
- `gummi`'s `fedora-0` still reports 42 ms jitter from its first samples;
  it should decay as the filter turns over.

Rollback: reinstall the `.prev` binaries, restore `carillon.toml.prev` on
`twocom`, restart.

## Public-server hardening and observability — 2026-08-25 (UTC)

Deployed `1410eae` to both hosts, replacing `eb89ae4` (19 commits behind).
Binaries installed by rename-over-in-place, since neither Linux nor FreeBSD
will let a running executable be written; `.prev` copies kept on both.

`[monitor]` and `[stats]` were enabled at the same time. Neither host had
them, so none of the new per-family metrics or the daily `server.tsv` had
anywhere to go — the hardening was deployed but unobservable. Both now bind
the monitoring listener on `127.0.0.1:9123` with the default loopback ACL,
and write statistics to `/var/lib/carillon/stats` (`gummi`) and
`/var/db/carillon/stats` (`twocom`).

Confirmed on both:

- `carillon -check` passes; `gummi` reports no public-ACL warning (its allow
  list is `172.19.0.0/16` plus a delegated `/64`), `twocom` reports all three
  (`2000::/3`, `rate_limit_pps 8`, `recv_buffer` unset).
- Effective `SO_RCVBUF` logged at startup: 212992 on Linux, 42080 on FreeBSD
  — both the kernel default, matching what the example config documents.
- `/healthz` 200 once synced, 503 while settling. `/metrics` carries the
  per-family series. `server.YYYY-MM-DD.tsv` gets two rows a minute.
- Topology restored: `gummi` synced at stratum 3 off the Fedora pool,
  `twocom` at stratum 4 with `gummi` as system source, +129.8 us, no steps.

Live counter proof, `gummi` to `twocom` over the LAN: six deliberately bad
datagrams (modes 6, 7 and 4; versions 5 and 0; a 10-byte runt) produced
exactly `non-client mode 3` (`server=1 control=1 private=1`), `bad version 2`
and `malformed 1`, with `Served` untouched. Before this release all six were
silent drops. `/metrics` agreed with `carillonctl serverstats` field for
field.

Note: `gummi` sat in `settling` for over eight minutes after the first
restart with only two loop updates. That is the RFC 5905 clock filter holding
an early low-delay sample, not a fault — the same host averaged one update
per 18 minutes over its previous 20-hour run. The second restart cleared it
in under two minutes.

Outstanding, needs an operator decision: `twocom` serves `2000::/3` with
`rate_limit_pps = 8` and no `recv_buffer`, which is 64x more permissive than
chrony's default and the kernel's own 41 KB of headroom. See the public
`[serve]` block in `carillon.toml.example`.

Rollback: reinstall the `.prev` binaries, restore `carillon.toml.prev` on
both hosts, restart.

## Third host, client-only — navlisten2026, 2026-08-25 (UTC)

Moved `navlisten2026` (172.19.1.62, Debian 13 trixie, x86_64) from ntpsec to
carillon `1410eae`. First host with no `[serve]` section at all, so it
exercises the client-only path: `-check` reports "0 NTP listeners".

Baseline, ntpsec immediately before the cutover: offset 3.491 ms, sys_jitter
1.83 ms, frequency +9.077 ppm, stratum 3, rootdelay 87.891 ms, rootdisp
28.928 ms, ten internet peers at 21-83 ms.

The handover carried the frequency across cleanly. With no drift file yet the
engine falls back to the kernel, which ntpsec had left at +9.077 ppm:

    msg="no drift file yet" path=/var/lib/carillon/drift
    msg="initial frequency" ppm=9.077285766601562 known=true from=kernel

Synchronized 4 s after start, no step. At one minute: offset +1.019 ms,
frequency +9.082 ppm, root dispersion 22.390 ms. Not yet a fair comparison
against ntpsec — that needs hours, and is what the Zabbix template is for.

Sources chosen to give an opinion from outside the building as well as inside:
`gummi` and `twocom` over the LAN (371 us and 320 us delay, 70.8 us and 0.2 us
jitter), `junia` via `clock.packetexport.com`, and `2.debian.pool.ntp.org`.
None marked `prefer`: gummi and twocom are chained, so preferring one would
weight a single failure twice. Selection immediately put `junia` in charge —
stratum 2 with tight root dispersion beats a stratum-3 LAN server on root
distance despite 70 ms of path — and classified `debian-2` an outlier at
80 ms jitter.

Two fixes this shook out:

- The shipped unit listed `ntp.service`, `ntpd.service`, `chrony.service`,
  `chronyd.service` and `systemd-timesyncd.service` under `Conflicts=` but
  not `ntpsec.service`, which is what Debian actually calls it. Both hosts
  here run ntpsec, so the guard would have missed the case it was written
  for.
- The unit is deployed here without `SupplementaryGroups=dialout` or either
  `DeviceAllow=` line. Those exist only for a serial refclock; a client-only
  or server-only host should have no device access at all, which the unit now
  says in a comment.

`CAP_NET_BIND_SERVICE` is deliberately kept even though a client binds no
privileged port: it is what lets the startup probe bind UDP/123 to detect a
second time daemon. Two daemons disciplining one clock is worth more than the
capability is worth saving.

Post-cutover: ntpsec disabled and inactive, nothing holding UDP/123, drift
file written, `/healthz` 200, statistics writing to /var/lib/carillon/stats.
`claude` added to the `carillon` and `systemd-journal` groups to match gummi,
so the control socket and unit journal are readable without sudo.

Rollback: `systemctl disable --now carillon; systemctl enable --now ntpsec`.

## Health-probe semantics and log throttle — 2026-08-28 (UTC)

Deployed `2700bc7` to `twocom` and `navlisten2026`, replacing `1410eae`.
`gummi` has been down since before the 2026-08-27 reboots and was skipped.

Why: `twocom` had been answering `/healthz` with 503 for a day while synced
and serving 13670 requests with zero drops. `gummi` is its `prefer` source,
`gummi` is down, and the handler mapped everything that was not `healthy` to
503 — so a lost preferred source failed the probe. `navlisten2026` stayed 200
through the same outage only because it marks no source `prefer`.

Changes:

- `/healthz` returns 200 for degraded and 503 only for unhealthy. The body
  still carries `status` and `reasons`, which is where a client that wants to
  alert on degraded should read them.
- The per-exchange failure WARN is throttled to the first and every tenth,
  the throttle `badAuthSeen` and `bogusSeen` already used.

Results:

- Both hosts passed `-check` and restarted from their drift files with zero
  steps. Frequency carried across untouched: `twocom` +20.943 to +20.925 ppm,
  `navlisten2026` +9.625 to +9.623 ppm.
- `twocom` `/healthz` now returns 200 with `{"status":"degraded",
  "reasons":["preferred_source_lost"]}`; `navlisten2026` returns 200 healthy.
  `settling` still returns 503 on both, which is the intended distinction.
- Log volume on `twocom`: 60 failed exchanges to the dead `gummi` produced 7
  lines (`count=1,10,20…60`), and the whole 20-minute run produced 24 carillon
  lines. The previous run had 4488 `exchange failed` lines out of 4545 total,
  98.7% of everything the daemon had said.
- `twocom` owns UDP/123 on both families exclusively; `navlisten2026` binds
  nothing but its loopback monitor, as a client-only host should.

The first attempt at the throttle was wrong and the deployment is what showed
it. It suppressed a repeat only when the error text matched the previous one,
but every exchange opens a fresh socket, so each message names a different
ephemeral source port and no two failures ever compared equal — `count=1`
through `count=12`, one line per attempt, the same flood. `cd83602` carried
that version for five minutes before `2700bc7` replaced it. The unit test had
passed because it fed the function a fixed string, which is not a failure this
code can produce; the throttle now takes no argument at all, so there is
nothing per-attempt left to key on.

Two things worth not repeating:

- `twocom` sat in `settling` for 15 minutes with two loop updates before its
  third qualifying sample arrived, and served 108 requests unsynchronized in
  that window. This is the RFC 5905 clock filter holding an early low-delay
  sample, already noted on 2026-08-25, but the cost to clients is clearer now:
  a restart of a serving host is not a 10-second outage.
- `navlisten2026` was restarted 17 seconds before `twocom` finished settling
  and discarded a stratum-16 reply from it (`discarding reply reason="stratum
  16"`). The same sequencing mistake as 2026-08-24. Waiting for the upstream
  to reach `synced`, not merely to have been restarted, is the actual rule.

Rollback: `.prev` on `twocom` is `cd83602` (not `1410eae`, which two installs
in five minutes displaced); `.prev` on `navlisten2026` is `1410eae`.

## Fable 5.1 review fixes — 2026-09-05 (UTC)

Deployed `7289d4e` to `gummi`, `twocom` and `navlisten2026`, replacing
`1410eae` on `gummi` and `2700bc7` on the other two. `gummi` is back up since
the 2026-08-28 entry, so all three hosts are current for the first time since
2026-08-25.

This carries the fixes for a 36-finding deep review (`review/2026/09/
REVIEW_FABLE5_XHIGH.md`); 34 fixed, 2 skipped with reasons in
`FIXES_FABLE5_XHIGH.md`. The two that matter operationally here are RF5X-006
(SETTLING counted loop updates) and RF5X-004 (the slew transient was left in
the kernel at exit).

Order was upstream-first with the rule from 2026-08-28 observed properly this
time — wait for the upstream to reach `synced`, not merely to have been
restarted: `gummi`, then `twocom`, then `navlisten2026`.

Pre-flight: the new binary was staged in `/tmp` on each host and run
`-check` against that host's *live* configuration before anything was
replaced. This release makes validation stricter (RF5X-021 rejects GPS-only
keys on a `type = "pps"` refclock) and adds three keys with defaults
(`[discipline] settle_updates`, `[serve] rate_limit_v6_prefix`,
`[stats] keep_days`), so an existing configuration could in principle have
stopped validating. All three passed unchanged; `twocom` reported its three
standing public-server warnings and nothing else.

Results:

- **Settling is no longer a multi-minute outage.** `carillonctl waitsync`
  returned in **3.2 s** on `gummi`, **2.1 s** on `twocom` and **2.3 s** on
  `navlisten2026`. The 2026-08-28 entry recorded `twocom` sitting in
  `settling` for 15 minutes and serving 108 requests unsynchronized, and
  `gummi` taking over eight minutes on another day. `twocom` served 0
  unsynchronized this time.
- Zero steps on all three; frequency carried across from the drift files
  untouched: `gummi` −15.418 to −15.336 ppm, `twocom` +6.127 to +6.482 ppm,
  `navlisten2026` +9.437 to +9.414 ppm.
- Topology unchanged: `gummi` stratum 3 off the Fedora pool, `twocom`
  stratum 4 with `gummi` as system source, `navlisten2026` stratum 3 with
  `junia` as system source and both LAN hosts as survivors.
- `/healthz` returns 200 `healthy` on all three.
- Statistics now land in `<dir>/YYYY/MM/DD/<kind>.tsv` (RF5X-029). The tree
  was created on first write on each host. **The pre-existing flat
  `<kind>.YYYY-MM-DD.tsv` files are left in place** — nothing reads or
  removes them, so they can be archived or deleted at leisure.
  `[stats] keep_days` is unset (0, keep everything) everywhere.
- SELinux on `gummi`: the replaced binaries were `restorecon`'d and kept
  `system_u:object_r:bin_t:s0`; no denials.
- Measured clock precision (RF5X-020) is now realistic on every host, where
  the old minimum-delta method reported 2^-30 everywhere regardless of the
  hardware: `twocom` 2^-21, `navlisten2026` 2^-22, `gummi` 2^-24. `carillon
  query` against `twocom` shows the new value on the wire. Everything floored
  at the precision — the filter's jitter floor, the loop's popcorn threshold,
  the negative-delay tolerance — moves with it.
- Authenticated and unauthenticated `carillon query` from `gummi` to `twocom`
  both answer correctly. `twocom` has no `require_key` prefixes, so RF5X-008's
  verify-before-limit path is not exercised by this topology; the optional-MAC
  path is.

RF5X-004 confirmed on a real kernel, which the fake-clock tests cannot do.
Before the stop, `navlisten2026`'s base estimate was +9.405 ppm with −7.7 ms
of phase pending, and `ntptime` showed the kernel holding **+1.739 ppm** —
the base plus the slew transient. On stop the daemon logged

```
msg="kernel frequency left at the base estimate" ppm=9.404899529698861
  abandoned_slew_ppm=-7.554560832743737 abandoned_phase=-0.0077283198055273505
```

and left +9.4049 in the kernel, matching the drift file. The old binary would
have left +1.85 ppm there — 7.6 ppm, 0.65 s/day, adrift from what the drift
file claimed — until something else wrote the frequency word.

Not exercised, because no host has one: the live PPS/GPS paths, and so
RF5X-001 (the spike gate) and RF5X-007 (the PPS agreement check) remain
covered only by their tests.

### Worth not repeating: `synced` no longer means `settled`

`twocom`'s frequency moved from +6.13 ppm to +32.28 ppm over the four minutes
after cutover, driving the offset down from 2.9 ms to 484 µs with no steps,
then turned around and began unwinding once the offset crossed zero. The loop
was doing its job; the disturbance was ours:

```
13:16:46  offset +2.907 ms  freq  6.48 ppm   (restarted, from the drift file)
13:17:06  offset +2.776 ms  freq  9.79 ppm
13:17:37  offset +2.512 ms  freq 14.52 ppm
13:19:45  offset +2.207 ms  freq 31.80 ppm   <- mu = 128 s against tau = 64 s
13:20:50  offset +0.484 ms  freq 32.28 ppm
13:25:07  offset -1.418 ms  freq 30.89 ppm   <- overshoot unwinding, as designed
```

`gummi` was restarted 40 s before `twocom` and reached `synced` in 3.2 s, but
it still had ~10 ms of phase to slew out and went on slewing for another ten
minutes. `twocom` polls it at `poll_min = 4`, so it saw its own upstream's
deliberate rate offset as a frequency error and integrated it. The 13:19:45
step is large because the clock filter had not produced a new lowest-delay
sample for 128 s while tau was 64 s: `mu > tau` makes one integration step
bigger than the time constant.

None of this is new — `mu` and tau have always been defined that way, and the
filter gate is the same one RF5X-006 is about. What *is* new is that the old
rule from 2026-08-28, "wait for the upstream to reach `synced`, not merely to
have been restarted", has quietly stopped being enough. It used to imply a
multi-minute wait that incidentally outlasted the upstream's slew. Now
`synced` arrives in seconds, which is the point of RF5X-006, and the wait it
used to buy is gone.

**The rule for the next deployment of a chain:** wait for the upstream's
`Pending slew` to fall to a few hundred microseconds — `carillonctl tracking`
reports it — not merely for its state to read `synced`. On this LAN that is
about ten minutes after the upstream restarts, or one `holdover_max`-free
poll interval after its offset settles, whichever is longer.

### Why the frequency moved, and what was actually wrong

`gummi` did the same thing on its own account — −15.42 ppm from its drift file
to +1.72 ppm — while its offset came down from 10.2 ms and overshot to
−2.8 ms. Both hosts recovered.

**A first explanation, written here on the day, was wrong**, and is recorded
because it was wrong in a way worth not repeating. It said the cause was
`mu`, the interval the frequency integration uses, running to one or two
times `tau`: a loop update happens only when the clock filter yields a new
lowest-delay sample, which is uncorrelated with the poll interval that sets
`tau`, so the step `theta·mu/(4·tau²)` gets large. The arithmetic was right —
`twocom`'s 13:19:45 update really did convert 2.5 ms into 17 ppm in one step —
but the conclusion did not follow. A closed-loop reproduction
(`TestLoopConvergesWhateverTheUpdateSpacing`) sweeps `mu` from 0.06·tau to
2·tau against a 15.4 ppm drift and a 10 ms offset:

```
mu =    16 s (0.06 tau): peak |freq-true|  6.92 ppm, final offset -0.000 ms
mu =   256 s (1.00 tau): peak |freq-true|  4.01 ppm, final offset +0.000 ms
mu =   512 s (2.00 tau): peak |freq-true|  2.63 ppm, final offset +0.000 ms
```

Every spacing converges to the correct frequency, and the peak excursion
*falls* as `mu` grows, because the phase slew removes most of the offset
between sparse updates and the next update integrates a smaller theta. Sparse
updates are not the problem.

The phase slew was verified against the live host too, from `Pending slew`
sampled every 45 s on `twocom` during the transient: the decay implies
`tau = 126.9 s` against a theoretical `4·2^5 = 128 s`. The loop is doing
exactly what §6.4 says.

**What actually happened** is simpler and is not a defect. `gummi` restarted
with ~10 ms of phase and spent fifteen minutes slewing it out, which means its
clock was deliberately running about 17 ppm fast for that whole period.
`twocom` is locked to `gummi`. A PLL locked to a source that changes rate must
follow it — that is what a PLL is for — so `twocom` moved its own frequency by
a comparable amount, and unwound once `gummi` stopped. `gummi`'s own swing has
a second contributor: its system source changed from `fedora-0` (67 ms delay)
to `fedora-2` (19 ms), and path asymmetry between two pool servers that far
apart moves the measured offset by milliseconds.

### The real defect: the drift file persisted the transient

None of the above is worth changing. What *was* worth changing is that
`maybeWriteDrift` wrote `sys.Frequency()` — the instantaneous value — hourly
and at shutdown, with no test of whether it meant anything. So a restart or an
hourly tick landing inside a transient persisted a frequency the host does not
need, and the next start began from it and produced the same excursion again.
The first advice written here, "do not restart a host mid-transient", is not
operational guidance; it is a defect wearing a hat.

The engine now keeps the last `[engine] drift_stable_window` (default 15 min)
of base-frequency readings and writes the drift file only while that history
spans the window and its spread is within `drift_stable_spread` (default
1 ppm). Otherwise it keeps what is on disk — by construction the last estimate
that did hold still — and logs why, at INFO on the shutdown path where an
operator will want to see it. A stale drift file costs one convergence the
loop performs anyway; a persisted transient costs the same convergence *and*
starts it from the wrong place.

Measured against the real numbers: `twocom` went 6.13 → 32.28 ppm inside four
minutes, a spread of 26 ppm against a 1 ppm gate, so nothing would have been
written. In steady state it sat at +6.127 ppm for days, a spread well under
0.1 ppm, so the hourly write proceeds normally. The regression test drives the
engine into a 61 ppm excursion and asserts the file keeps its earlier value.

### Upgrading *to* the gate: the outgoing binary still poisons the file

Deploying the drift-file gate has one step that is easy to miss and that this
deployment got wrong the first time. The write that matters happens at
**shutdown, in the binary being replaced** — which does not have the gate. So
the sequence "install, restart" persists the transient one last time and the
new binary starts from it:

```
msg="kernel frequency left at the base estimate" ppm=1.6378832867356365 ...
msg="initial frequency" ppm=1.637883 known=true from="drift file"
```

`gummi` came up on +1.638 ppm when its true value was −15.42.

For this upgrade only, restore the file between stop and start:

```sh
sudo systemctl stop carillon          # the old binary writes its transient here
printf -- '-15.418252\n' | sudo tee /var/lib/carillon/drift
sudo chown carillon:carillon /var/lib/carillon/drift
sudo systemctl start carillon
```

Use the value the host held while it was settled, which is what the file
contained before the first restart of the day. Afterwards the gate handles it:
the second stop logged

```
msg="leaving the drift file alone" reason="less than 15m0s of frequency history"
  current_ppm=1.5070742347860342
```

and `gummi` restarted on −15.418252, reaching −15.449 ppm immediately.

### Remediation, 13:47–13:52 UTC

Redeployed `5561238` (the drift-file gate) to all three, restoring each drift
file between stop and start because the outgoing binary poisons it. `twocom`'s
read **32.106037** after its old binary stopped, against the 6.126572 it had
held for days; `gummi`'s read 1.637883 against −15.418252.

| host | frequency before | after |
|---|---|---|
| `gummi` | +1.64 ppm (transient) | **−15.74 ppm** |
| `twocom` | +32.11 ppm (transient) | **+1.29 ppm**, converging to ~+6 |
| `navlisten2026` | +9.39 ppm | +11.81 ppm |

All three synced, no steps, `twocom` serving with zero unsynchronized replies.
Restoring `gummi` also cleared its loop-update starvation: it had gone 14
minutes with no update while carrying a 17 ppm error.

The second stop of `gummi` — the first with the gate present — logged what it
is for:

```
msg="leaving the drift file alone" reason="less than 15m0s of frequency history"
  current_ppm=1.5070742347860342
```

### A defect the deployment found in the RF5X-006 fix

`navlisten2026` came up in `settling` with `Updates = 1` and stayed there past
a `waitsync 60`, reaching `synced` only at 2m18s. RF5X-006 moved the settling
counter off loop updates and onto measurements for the system source, but left
the *check* inside the branch of `reselect` that only runs when the clock
filter yields a new lowest-delay sample — so a filter withholding updates
still pinned the daemon in SETTLING, which is the outage RF5X-006 existed to
remove. Under the drought documented in `DESIGN.md` §6.3 that is up to 34
minutes of LI=3 on a serving host.

Fixed in `751c23c`: the settling transition is evaluated on every reselect.
`TestSystemLeavesSettlingWithoutAFreshLoopUpdate` feeds five samples that
produce no loop update and requires SYNCED; it fails on `5561238`.

**The hosts are running `5561238` and still carry this defect.** It costs
nothing while running — settling only happens after a restart or a step — so
it does not warrant a fourth restart today. Deploy `751c23c` at the next
natural opportunity, and it will make that restart's settling robust.

## Settling fix, and the gate proving itself — 2026-09-05 14:46–14:51 (UTC)

Deployed `9cb190e` (the RF5X-006 settling fix, `751c23c`) to all three.

It doubled as remediation, because between the previous deployment and this
one the starvation defect did real damage. `gummi` went **2533 s — 42
minutes — between consecutive loop updates**, longer than the Allan intercept
because the stale sample must both age past it *and* lose to a fresher one on
raw delay. It drifted 18 ms blind, 27 ms by the time it was restarted, and its
downstreams read the eventual correction as a rate change and integrated it:

| host | true value | reached |
|---|---|---|
| `navlisten2026` | ~+10 ppm | **−95.4 ppm** (8 s/day) |
| `twocom` | ~+6 ppm | **+52.1 ppm** |

**The drift-file gate is what made this recoverable.** Both hosts' files still
held their settled values, so each restarted correct rather than persisting
the excursion. The refusals, verbatim:

```
twocom:        msg="leaving the drift file alone" reason="frequency moved 51.442 ppm
                 in the last 15m0s, more than the 1.000 ppm the estimate must hold to"
                 current_ppm=52.05749506890824
navlisten2026: msg="leaving the drift file alone" reason="frequency moved 105.511 ppm
                 in the last 15m0s ..." current_ppm=-95.42969409216681
```

`twocom` restarted on 6.126572 and came up at **+6.127 ppm**;
`navlisten2026` on 10.066208 and came up at **+10.201 ppm**; `gummi` on
−15.246936 at **−15.220 ppm**. Without the gate all three would have started
from the excursion. This is the mechanism the 13:30 entry's "do not restart
mid-transient" was standing in for, now handled by the daemon.

Each came up carrying the phase it had accumulated — `navlisten2026` +85 ms,
`gummi` +27 ms, `twocom` −8.8 ms — and slewed it out; no steps anywhere.
`navlisten2026` reached SYNCED in 2 updates, where before the settling fix it
had taken 2m18s and a `waitsync 60` failure.

**Outstanding and unfixed:** the starvation itself (`DESIGN.md` §6.3). The
ranking is what withholds the sample — `Filter.Add` ranks by raw `delay` until
the Allan intercept, so 38 ms of accumulated dispersion never costs a stale
sample its place. Ranking by root distance would demote it in about 11 minutes
instead of 34-plus, but that is a deliberate deviation from RFC 5905 §10 and
from ntpd, and wants a simulation pass. Until it is fixed, expect this chain
to repeat.

## Astra6 review fixes — 2026-09-06 00:48–01:05 (UTC)

Revision: `3b08543` (55 of the 59 `REVIEW_ASTRA6_XHIGH.md` findings fixed).
Deployed to all three in-house hosts, upstream first per the runbook:
`gummi` → `twocom` → `navlisten2026`, each reaching SYNCED before the next.

Method on each host: stage the binary in `/tmp`, run `-check` against the live
config with the *new* binary, keep the outgoing binary as
`carillon.9cb190e`, install beside the target and `mv` over it (atomic, and
avoids `ETXTBSY` on a running image), restart, `waitsync 300`.

Results:

- All three report `3b08543`, reached SYNCED with **no clock step**, and
  restarted from their drift files. `gummi` in 6 s, `twocom` in 33 s,
  `navlisten2026` in 8 s. No WARN or ERROR in any service log.
- `gummi` (stratum 2→3, public pool): offset +1.9 ms, frequency −6.33 ppm
  against −6.353 ppm before the restart — the drift file carried it across
  unchanged. Root dispersion 130 ms at t+6 s decaying to 66 ms by t+100 s as
  the filters primed, which is the RA6X-024 priming uncertainty behaving as
  designed.
- `twocom` (prefers `gummi`, authenticated): materially better than before.
  Offset **+287 µs** against −1.13 ms, jitter **574 µs** against 2.50 ms, and
  16 loop updates in the first 13 minutes against a pre-restart reference time
  of 30 s. Still selects `gummi` at ~250 µs round-trip.

### The field evidence for RA6X-001 and RA6X-013

The hosts' own `loop.tsv` files record both defects happening, hours before
the deploy:

- `navlisten2026` held a stable **+9.4 ppm** from 13:17 to 13:49, then the old
  code's delayed feedback took it through +11.8, −28, −30, +10, **−95.4**,
  +62, and finally **+170.80**. The −95.4 ppm figure is the one already quoted
  in `DESIGN.md` §6.3.
- It then sat at **170.805461 ppm for eighteen minutes** (23:21–23:39) with no
  loop update. That flat run is exactly what the old drift gate measured as
  "stable", and it persisted it: `/var/lib/carillon/drift` held `170.805461`
  while the running daemon had since walked to −26.8 ppm — a 197 ppm
  disagreement between the file and the process that wrote it.
- `twocom` shows the same shape: it loaded a known-good `6.126572`, then went
  6.48 → 9.79 → 14.52 → **31.80** within three minutes, and its drift file was
  left at `20.187612`, a mid-excursion value.

On restart the new code loaded `navlisten2026`'s poisoned file, as it should —
the file is the stated authority. That put the host 197 ppm fast. The drift
file was replaced with `9.400000`, the value the host demonstrably held while
stable, and the poisoned copy kept as `drift.poisoned-170ppm`. **Before that,
on shutdown, the new gate refused to persist the bogus +172 ppm it was
carrying** — insufficient evidence — leaving the file untouched. That is
RA6X-013 working on the first host that could exercise it.

`twocom`'s stale `20.187612` was deliberately left alone: it is converging on
its own (18.3 ppm and falling, phase held at 287 µs), and the fixed gate will
replace it once the estimate holds within 1 ppm for the 15-minute window with
real loop updates behind it. That is the designed self-heal, and watching it
is worth more than another restart of a serving host.

### Closes the outstanding item from 2026-09-05

The previous entry left the starvation "outstanding and unfixed", noting that
root-distance ranking "wants a simulation pass". RA6X-002 made that change with
the simulation pass (`review/2026/09/takeover-repro/`), together with the
RA6X-001 observation-time propagation without which it does not help.

### Regression found: loop-update cadence on navlisten2026

`navlisten2026` now takes a loop update every 2–3 minutes where it previously
took one every ~19 s. The clock still tracks correctly — pending slew drains,
and all four sources agree the offset is converging — but frequency correction
is slower there.

The cause is not a fault in the new code so much as the removal of something
that was masking an older one. RA6X-003 made loop-update consumption
per-source; before that, a single system-wide watermark was reset to zero on
every system-source change, so a host whose system source flaps got a forced
loop update on each flap — re-integrating an observation the loop had already
used, which is the RA6X-001 defect. With that gone, the loop runs only on
genuinely new samples from the current system source, and on this host that is
`debian-2`: the only stratum-2 survivor, a WAN server at poll 6 whose clock
filter withholds. Its two excellent LAN sources, `gummi` at 300 µs and
`twocom` at 345 µs, are stratum 3 and 4 and so never drive the loop, because
RFC 5905 selection orders by stratum before distance.

This wants a design decision rather than a patch, and it is not on the Astra6
list: whether a much nearer, much lower-distance survivor should be able to
drive the loop when the stratum-preferred source is withholding. `gummi` and
`twocom`, whose source mixes do not have this shape, are unaffected — 7 and 16
updates respectively over the same period.

## Leap authority, manual acquisition — 2026-09-08 (UTC)

Deployed `v1.0.0-4-g3413e23` to all three hosts, replacing `3b08543`.

`3413e23` makes a leap table mandatory for any host with `[serve]` or a
refclock, so the existing configs no longer validate. Running the new binary's
`-check` against the live configs before installing anything showed exactly
that, on the two serving hosts:

    carillon: config /etc/carillon/carillon.toml: leap: required table needs
    manual, nist or peers acquisition

`navlisten2026` passed unchanged, being client-only. Had the binaries gone in
first, `ExecStartPre` and the rc.d check would have refused to start and left
both servers down.

Acquisition: `manual`, from the `leap-seconds.list` each host already had —
`/usr/share/zoneinfo/leap-seconds.list` from tzdata on the two Linux hosts,
`/var/db/ntpd.leap-seconds.list` on FreeBSD. All three are the same 5069 bytes
and the same SHA-256 `506e737d…`, carry both `#$` and `#@`, and expire
2026-12-28. Only `leapfile` was added under `[daemon]`; `LeapMode()` returns
`manual` on its own once that is set, so no `[leap]` section was needed. Each
candidate config was validated with `-check` on a temp copy before replacing
the live file, and the previous file kept as `carillon.toml.prev`.

Order was `gummi` → `twocom` → `navlisten2026`, waiting for `synced` and not
merely for the restart to return, which is the rule the 2026-08-28 entry got
wrong twice.

Results:

- All three report `Leap source file`, `Leap ready true`, the same digest, and
  `manual: activated`. All synced with zero steps and `/healthz` 200.
- Topology intact: `twocom` stratum 4 on `gummi`, `navlisten2026` on `junia`.
- Zero dropped datagrams in every category on both servers.

Fixed while deploying: the shipped systemd unit set

    RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX

with no `AF_NETLINK`, so `net.InterfaceAddrs()` failed on both Linux hosts with
`netlinkrib: address family not supported by protocol`. That silently disabled
self-synchronization and timing-loop detection (RA6X-038) and directed-broadcast
recognition — the daemon says so at WARN and keeps time, so the guards were off
on every Linux install without anything failing. `AF_NETLINK` added to the unit
here and on both hosts; the warning is gone and both resynced clean. FreeBSD was
never affected.

Two things to know:

- **The table expires 2026-12-28.** Missing or expired required data withholds
  synchronized service, so on that date `gummi` and `twocom` stop serving
  unless the file is refreshed. The Linux hosts get a new one whenever tzdata
  updates; `twocom`'s copy is ntpd's, and ntpd is disabled there, so it will
  not refresh on its own. Moving a seed to `acquire = "nist"` and the others to
  `peers` is the design's answer and removes the cliff; `peers` needs a
  `leap_trust` CMAC key per hop, which `navlisten2026` does not yet have to
  either server.
- `journalctl -p warning` does not filter this daemon's warnings. Carillon logs
  slog levels in the message text and systemd stamps the whole stderr stream at
  one priority, so `-p warning` returns nothing while warnings are present.
  Grep for `level=WARN` instead. Earlier entries here that reported "no
  warnings" on a Linux host were reading that filter, not the log.

Rollback: `.prev` copies of both binaries, `carillon.toml.prev`, and
`carillon.service.prev` are on each host; the previous release is `3b08543`.

## M5 review fixes — 2026-09-09 13:04–13:21 (UTC)

Deployed `v1.0.0-11-ga8f56b5` to all three hosts, replacing
`v1.0.0-4-g3413e23`. This carries the eight fixes from the M5 leap-authority
review (`review/2026/09/REVIEW_M5_FABLE51.md`, recorded per finding in
`FIXES_M5_FABLE51.md`): a plain client keeps its synchronization and holdover
when LI is unknown, SETTLING publishes LI=3 and cannot arm a kernel leap, the
NIST transport ignores proxy environment variables, unanswered CLPS probes get
an ordinary timeout backoff instead of the 24-hour capability floor, a
transfer recovers its normal spacing after RATE, and the leap cache is
checkpointed every 15 minutes with a final save on orderly shutdown. It also
brings the repository copy of the systemd unit (`c290aeb`, the `AF_NETLINK`
change made on the hosts on 2026-09-08) and the deployed units back into
agreement, below.

Pre-flight, before anything was replaced:

- `make test` (vet plus the race detector) passed on the build commit.
- The new binary was staged in `/tmp` on each host and run `-check` against
  that host's live configuration. All three passed unchanged; `twocom` reported
  only its three standing public-server warnings. No configuration needed
  changing: all three hosts have been on `manual` acquisition from a
  `leapfile` since 2026-09-08, so the M5 upgrade gate was already satisfied.
- Both candidate unit files passed `systemd-analyze verify` on their hosts.

### Units brought back to the repository's version

The two Linux units had drifted from `deploy/systemd/carillon.service`: they
were hand-edited for `AF_NETLINK` on 2026-09-08 and still carried
`ReadWritePaths=/var/lib/carillon /run`, which the repository narrowed to
`/var/lib/carillon` some time ago on the grounds that `RuntimeDirectory=`
already makes `/run/carillon` writable under `ProtectSystem=strict` and that
listing `/run` opens every other daemon's runtime directory. `gummi`'s also
lacked `ntpsec.service` in `Conflicts=`.

`gummi` got the repository unit verbatim. `navlisten2026` got it minus
`SupplementaryGroups=dialout` and the two `DeviceAllow=` lines, as the unit's
own comment prescribes for a client-only host and as that host has been
deployed since 2026-08-25. `systemctl show` confirms
`ReadWritePaths=/var/lib/carillon` on both, and both restarts created the
control socket under `/run/carillon` without incident, which is the thing the
narrowing could have broken. The FreeBSD rc.d script was already identical to
`deploy/freebsd/carillon`.

### Method and order

On each host: `cp -p` the outgoing binaries to `.prev`, install the new ones
beside the target and `mv` over it, `restorecon` on `gummi`, install the unit
and `daemon-reload` on the Linux hosts, restart, `waitsync 300`. `.prev` now
holds `3413e23` everywhere; the `.9cb190e` copies remain; `3b08543` is no
longer on any host.

Upstream first, with the slew rule from 2026-09-05 applied literally:

- `gummi` restarted 13:04:48. It came up with −2.83 ms of phase and took 13
  minutes to slew it out: −2.7 ms at 13:05, −1.6 ms after its second loop
  update at 13:09, −211 µs at 13:18.
- `twocom` restarted 13:18:50, once `gummi` was under 300 µs. Its own pending
  slew then held between −75 and −85 µs for four consecutive 30 s samples.
- `navlisten2026` restarted 13:21:17.

### Results

| host | `waitsync` | offset at restart | frequency before → after | drift file | system source |
|---|---|---|---|---|---|
| `gummi` | 7 s | −2.83 ms | −5.597 → −5.554 ppm (−5.582 at +17 min) | −5.547275, untouched | `fedora-1` |
| `twocom` | 3 s | −116 µs | +19.000 → +20.674 ppm (+20.191 at +3 min) | 20.687926, untouched | `gummi` |
| `navlisten2026` | 3 s | −878 µs | +9.375 → +9.393 ppm | 9.401391, untouched | `debian-2` |

- Zero clock steps on all three. Zero `level=WARN` or `level=ERROR` lines in
  any service log since its restart, apart from `twocom`'s three standing
  configuration warnings at startup.
- Leap: all three report `Leap ready true`, `manual: unchanged`, and the same
  digest `506e737d…`; each rewrote `state.json` on start.
- The outgoing `3413e23` binaries each declined to persist the drift file at
  stop, for three different reasons the gate gives — `gummi` "only 1 loop
  updates in the last 15m0s", `twocom` "frequency moved 1.708 ppm in the last
  15m0s", `navlisten2026` "less than 15m0s of frequency history" — and each new
  binary started from the value on disk. The files are unchanged.
- Server paths, from the new builds: `twocom`'s authenticated query to `gummi`
  (key 1) measured −84 µs offset at 210 µs delay; `gummi`'s unauthenticated,
  ACL-authorized query to `twocom` measured +120 µs at 258 µs. Both carried
  kernel receive timestamps.
- `gummi` and `twocom` each own IPv4 and IPv6 UDP/123 exclusively and served
  zero requests while unsynchronized (50 and 18 served in the first minutes).
  `navlisten2026` binds nothing but its loopback monitor. `/healthz` is 200
  `healthy` on all three.
- Topology intact: `twocom` stratum 4 on `gummi`; `navlisten2026` stratum 3
  with both LAN hosts as survivors. It chose `debian-2` as system source on
  its first three samples with `junia` an outlier, which is early-sample
  selection and is expected to settle.

### Observed, not acted on

`twocom` has logged `ignored offset spike` at WARN 20–40 times a day since
2026-09-06, against one a day before 2026-09-05; `gummi` logs 1–2 a day and
`navlisten2026` 3–7. Today's rejected offsets on `twocom` ranged from 2 µs to
461 µs against a reported jitter of 27–41 µs. Several of them — 2 µs, 4 µs,
38 µs — are well inside the jitter, so either the spike gate (RF5X-001) is
keying on something other than the offset it logs, or it is rejecting good
samples on the host with the tightest path. Worth checking against
`twocom`'s `loop.tsv` and `sources.tsv`; it is not a deployment issue and the
binaries were not changed for it.

Rollback: `.prev` copies of both binaries (`3413e23`) and
`carillon.service.prev` on each host; configurations unchanged.

## Repeatable checklist

Deploy a chain upstream-first, and between hosts wait for the upstream's
`Pending slew` to fall to a few hundred microseconds — not merely for its
state to read `synced`, which since 2026-09-05 arrives in seconds and no
longer implies the upstream has finished moving its clock.

On each host, before replacing anything, stage the new binary and run it
against the live configuration — validation gets stricter from time to time:

```sh
scp -F ~/Git/infra/ssh-claude/config carillon <host>:/tmp/
ssh -F ~/Git/infra/ssh-claude/config <host> 'sudo /tmp/carillon -check -config <config>'
```

Then, on each host:

```sh
carillon -check -config /path/to/carillon.toml
carillonctl waitsync 300
carillonctl tracking
carillonctl sources
carillonctl refclock
carillonctl serverstats
```

From an allowed peer, exercise the applicable unauthenticated and
authenticated paths:

```sh
carillon query server.invalid
carillon query -keys /path/to/keys -key 1 server.invalid
```

Then verify that exactly one process owns UDP/123, the former time service is
disabled, the drift file survives a clean restart, and the platform service
log contains a clean stop/start with no clock step.
