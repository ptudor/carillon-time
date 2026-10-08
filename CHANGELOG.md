# Changelog

## 1.2.0 — 2026-10-07

- Update `github.com/prometheus/common` to v0.72.0 and refresh compiled
  dependency notices.
- Update GoReleaser to v2.18.2, `govulncheck` to v1.8.0, and the pinned
  artifact-upload Action to v7.0.2. The release toolchain remains Go 1.27.1.
- Keep dependency alerts and scheduled security checks while disabling
  automatic dependency-update pull requests.

## 1.1.0 — 2026-09-16

- Add durable leap-second data. `[leap] acquire` selects `manual` (the existing
  `daemon.leapfile`), a seed that downloads the IERS or NIST `leap-seconds.list`
  from a fixed HTTPS URL, or `peers`, which learns the original file from an
  upstream marked `leap_trust = true` over an AES-128-CMAC authenticated NTP
  extension and can redistribute it to the keys in `serve.leap_keys`. Tables
  live in a private atomic cache beside the drift file, pass digest, replay,
  history, and rollback checks, and activate without a restart. The wire
  protocol is experimental.
- **Upgrade:** a refclock or serving host now requires a current leap table
  before it reports synchronization, and its configuration fails validation
  without a manual file or an acquisition mode. Network-only clients need no new
  settings: they keep using fresh upstream leap indicators and keep holdover
  when leap knowledge is unknown.
- The seed is a polite client of a shared public server: it identifies itself,
  makes conditional requests, obeys `Retry-After`, never asks more often than
  every 15 minutes even across restarts, and ignores proxy environment
  variables. Acquisition recovers from transient network failures and RATE.
- `carillonctl tracking` reports the leap source, readiness and its reason, the
  table's digest, provider and expiry, and acquisition state.
- A Linux GPS can carry NMEA and its pulse on one tty. `pps = "dcd"` attaches the
  N_PPS line discipline itself, as on FreeBSD, with no `ldattach` service or
  second port; the manual unit grants the tty `rw`, which the attach needs.
  `pps = "cts"` remains FreeBSD-only.
- Allow `AF_NETLINK` in the packaged and manual systemd units. Without it the
  daemon kept time but silently skipped the guards that refuse a source that is
  itself or synchronized to it and that recognize directed broadcasts.
- Fix replies from a FreeBSD listener bound to a specific IPv4 address, where
  `sendmsg` failed with `EINVAL`.
- Sign release RPMs, DEBs, and the `checksums.txt` manifest with the project's
  OpenPGP release key, `8C4F 58EF B945 4902 267D 9B3E 426C 4AA2 1A40 0728`,
  published in `packaging/release-signing-key.asc`. `rpm` and DNF verify the
  packages once the key is imported. APT does not check a local DEB's signature,
  so DEBs and archives are authenticated through the signed manifest.
- The release workflow refuses to build unless its signing secret matches the
  published key, and verifies every signature, including a DNF installation with
  signature checking enforced, before attestation and publication. CI and
  `make snapshot` sign with a throwaway key, so every build exercises that path.
- Update `golang.org/x/sys` to v0.48.0.

## 1.0.0 — 2026-09-06

- First public release of Carillon: a pure-Go NTP time daemon for Linux and
  FreeBSD, with GPS, serial PPS, and inspectable clock discipline.
- Publish static Linux and FreeBSD binaries for amd64 and arm64, unsigned Linux
  RPMs and DEBs, SHA-256 manifests, and GitHub build attestations.
- Use the canonical Go module path `github.com/ptudor/carillon-time`.
- Ship a package-specific systemd unit with network-only defaults, a dedicated
  account, preserved configuration, and explicit operator activation/restarts.
- Update TOML, Prometheus, and protobuf dependencies, pinned GitHub Actions,
  and the Go release toolchain to 1.27.1.
- License the project under MIT and include dependency notices in archives and
  package copyright files, including on minimal Debian installations.
- Add native race tests, safe ABI checks, cross-builds, Debian/Fedora package
  lifecycle tests, scheduled vulnerability scans, and full-history secret scans.
- Document platform coverage and hardware acceptance limits; version 1.0.0 does
  not claim a measured accuracy guarantee across hardware configurations.
