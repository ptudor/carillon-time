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

## Repeatable checklist

On each host:

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
