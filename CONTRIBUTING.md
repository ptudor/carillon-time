# Contributing

Start with a small, reproducible operational problem. A good issue includes the
release or commit, OS and architecture, a minimal configuration, expected behavior,
and redacted logs. For protocol interoperability, include the peer software and
relevant packet fields. Use [private reporting](SECURITY.md) for vulnerabilities.

Install the Go version in [.go-version](.go-version). Clone the repository and run
`make build` and `make test`. Read [DESIGN.md](DESIGN.md) before changing protocol
or clock-discipline behavior, and update it alongside the implementation.

Never run the daemon or hardware tests against a development machine’s clock.
Ordinary tests use fake clocks. Native `make abicheck` compares struct layouts
against system headers without opening devices or adjusting time. Tests tagged
`hwtest` are a separate, explicitly enabled operator task.

Keep changes focused. Explain the trigger, resulting behavior, compatibility
impact, and tests in the pull request. Add a regression test for a behavior fix;
packaging changes should also pass `make release-check`, `make snapshot`, and
`sh scripts/test-packages.sh` on a Linux amd64 machine with Docker. Shell scripts
are POSIX sh and are checked with ShellCheck in CI.

Production binaries use `CGO_ENABLED=0`. The race detector and native ABI checks
need a C compiler. GitHub CI runs on Linux amd64, Linux arm64, and macOS arm64;
FreeBSD release targets are cross-built, with runtime acceptance performed on
FreeBSD separately.

The module path is `github.com/ptudor/carillon-time`. Build from the repository
root; release and local builds stamp metadata into this module’s `internal/buildinfo`.

Contributions are accepted under the project's [MIT license](LICENSE). Retain
third-party copyright and license notices. After a dependency or Go toolchain
update, run `make snapshot`, then `python3 scripts/update-notices.py` and review
the changes to `THIRD_PARTY_NOTICES.md`. Rebuild the snapshot to include the updated
notices. CI compares those notices against every release binary's module metadata.
If your Go distribution stores its license outside `GOROOT`, pass
`--go-license /path/to/go/LICENSE` to the script.
