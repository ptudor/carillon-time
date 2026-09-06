# Changelog

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
