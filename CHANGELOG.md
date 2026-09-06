# Changelog

## Unreleased

- Adopt the MIT license and include dependency license/attribution notices in all
  release archives and Linux packages.

- Prepare Carillon’s public README, Linux package guide, and documented acceptance
  limitations.
- Rename the Go module and imports to `github.com/ptudor/carillon-time`.
- Add GitHub CI, static Linux/FreeBSD release archives, unsigned RPMs and DEBs,
  SHA-256 manifests, and build provenance attestations.
- Add a package-specific systemd unit with network-only defaults, a dedicated
  account, preserved configuration, and explicit operator activation/restarts.
- Add scheduled vulnerability scans, history secret scans, and Dependabot updates.

The first published tag will establish the public release baseline. Review
[release setup](docs/releases.md) before publication.
