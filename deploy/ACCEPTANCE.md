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
carillon query server.example.net
carillon query -keys /path/to/keys -key 1 server.example.net
```

Then verify that exactly one process owns UDP/123, the former time service is
disabled, the drift file survives a clean restart, and the platform service
log contains a clean stop/start with no clock step.
