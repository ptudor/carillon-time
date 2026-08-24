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

## Repeatable checklist

On each host:

```sh
carillon -check -config /path/to/carillon.toml
carillonctl waitsync 300
carillonctl tracking
carillonctl sources
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
