# carillon — NTP-compatible time daemon with serial-port PPS

`carillon` is a single-binary NTPv4 (RFC 5905) client + server whose reason for
existing is **disciplining the system clock from a PPS signal on a serial port**
(RFC 2783 PPS API) on FreeBSD and Linux. The
second job is the two-host topology: the **home** box (GPS + PPS, stratum 1)
is the trusted upstream for the **colo** box (stratum 2), which serves time to
its clients.

**Status (2026-08-23):** milestones M0–M4 of `DESIGN.md` §15 are implemented
in code, plus a public-server hardening pass (2026-08-24) covering martian
filtering, a counter for every dropped datagram, per-address-family statistics
and the `[serve]` sizing knobs — see `DESIGN.md` §7 and §10.4.
The authenticated two-host topology is deployed on `gummi` (Fedora 43) and
`twocom` (FreeBSD 15): both clock backends, init systems, drift persistence,
IPv4/IPv6 listeners, ACLs, CMAC, and client/server paths have passed an initial
real-host acceptance run (`deploy/ACCEPTANCE.md`). They are now the long-term
in-house test hosts. Linux and FreeBSD PPS API/capability/fetch paths have
also run on them, but none of their configured serial inputs currently has a
live pulse, so GPS/PPS stratum-1 acceptance remains pending. M4 adds GPS/NMEA,
leapfile authority, daily statistics, and read-only JSON/health/Prometheus
monitoring. OpenWrt is explicitly deferred to a separate, smaller C project;
this Go daemon targets Linux and FreeBSD. A live PPS/GPS test and the
long-duration accuracy comparison remain ahead.

`DESIGN.md` is the specification. Read it before writing code, and update it
whenever protocol or discipline behaviour changes — the design doc is the
source of truth, the code follows it.

The name is `carillon` — a set of tuned bells struck on the hour. (It replaced
a working name built on "tick", which reads too much like "bug".) The daemon
binary is `carillon`, the status client `carillonctl`. The name appears in
exactly these places: the Go module path, `cmd/carillon`, `cmd/carillonctl`,
the config path, metric/env prefixes, and `deploy/` — keep it that way so any
future rename stays a mechanical search-and-replace.

## Toolchain and dependencies

- **Go**, pure Go, `CGO_ENABLED=0` always. Everything the daemon needs from the
  kernel (ioctls, `ntp_adjtime`, socket timestamps) is done with
  `golang.org/x/sys/unix` and hand-declared structs — no C toolchain, so
  cross-compiling for FreeBSD/Linux from this Mac is one command.
- Local toolchain: `/opt/local/bin/go` (MacPorts). `go.mod` sets `go 1.25.0`
  as the floor — `golang.org/x/sys` v0.47 requires it; bump deliberately, not
  as a side effect of a `go mod tidy`. Run `go` with `GOMODCACHE`/`GOCACHE`
  pointed at the session scratchpad so nothing is written outside `~/Git`.
- Allowed third-party modules — do not add others without saying why:
  - `golang.org/x/sys` — syscalls, ioctls, socket options
  - `github.com/pelletier/go-toml/v2` — config (house standard; strict decode
    with `DisallowUnknownFields`)
  - `github.com/prometheus/client_golang` — optional `/metrics` endpoint
- Standard library for everything else: `log/slog` for logging, `crypto/aes`
  (AES-CMAC per RFC 4493 is implemented in-house in `internal/ntp/auth` — the
  stdlib has no CMAC; test against the RFC 4493 vectors), `net`, `context`.

## Layout

```
cmd/carillon/            daemon entry point (flags: -config, -check, -version; subcommand: query)
cmd/carillonctl/          status client for the control socket
internal/config/      TOML schema, validation, per-OS defaults
internal/ntp/         wire format, timestamps/eras, MAC (auth/), KoD codes — pure, fuzzable
internal/source/      Source interface + NTP client source (poller, clock filter, Query)
internal/sockts/      kernel receive timestamps on UDP sockets, per OS
internal/buildinfo/   version and build time stamped by the Makefile
internal/refclock/    PPS and GPS (NMEA plus optional PPS) refclocks
internal/leap/        NIST/IERS leap-seconds.list parser and authority
internal/pps/         RFC 2783 bindings: pps_linux.go, pps_freebsd.go, pps_other.go
internal/serial/      termios open/configure; Linux N_PPS line-discipline attach
internal/clock/       actuator: Clock interface, sysclock_linux.go, sysclock_freebsd.go,
                      sysclock_other.go (stub), fake.go (deterministic, for tests)
internal/discipline/  filter → select → combine → loop; pure functions, no wall clock
internal/engine/      the single owning goroutine: wires sources, discipline, actuator, status
internal/server/      UDP listener(s), responder, ACL, rate limiter, KoD
internal/control/     unix-socket JSON protocol used by carillonctl
internal/monitor/     read-only JSON/health HTTP server and Prometheus collector
internal/stats/       bounded asynchronous daily UTC TSV writer
deploy/               freebsd/ (rc.d), systemd/, apache/, carillon.toml.example
DESIGN.md             the spec
```

Platform code lives in `_linux.go` / `_freebsd.go` files with `//go:build`
tags, plus an `_other.go` fallback that compiles everywhere (including this
Mac) and returns `ErrUnsupportedPlatform` at runtime. `go build ./...` and
`go test ./...` must pass on darwin; only the clock and PPS backends are stubs.

## Commands

```
make build                 # host build of carillon + carillonctl into ./bin
make test                  # go vet ./... && go test -race ./...
make freebsd               # GOOS=freebsd GOARCH=amd64
make linux                 # GOOS=linux GOARCH=amd64 (+ arm64 target)
go test -fuzz=FuzzDecode ./internal/ntp/   # wire-format fuzzing
```

Prefer `go test -race ./...` over per-package runs before declaring anything
done. `staticcheck` is welcome if it is installed; do not add it as a module
dependency.

## Hard rules

1. **Never touch the clock of the machine you are running on.** No `sudo`, no
   running `carillon` against the real clock, no `ntp_adjtime`/`settimeofday`
   from a test. Every test uses `clock.Fake`. Tests that need a real PPS device
   or real clock privileges are build-tagged `hwtest` and gated on
   `CARILLON_HW_TESTS=1`; the user runs those on the target host, never Claude.
2. **No cgo.** Kernel struct layouts (`timex`, `pps_info`, `pps_params`,
   `pps_fetch_args`, Linux `pps_fdata`) are declared by hand per OS/arch. Each
   gets a `//go:build cgo && hwtest` size/offset test that compares against the
   C headers — it runs only on the target host, but it must exist.
3. **NTP modes 6 and 7 (control / private) are never implemented.** Status
   comes from the unix control socket. A reply is never larger than the request
   that produced it; the server is not an amplifier.
4. **The server is off unless `[serve] allow` lists a prefix.** No implicit
   "serve everyone" default. (`[[server]]` entries are the upstreams we poll;
   `[serve]` is the listener — TOML cannot name both `server`.)
5. **Timestamps come from the kernel where the kernel offers them**: PPS edges
   via the PPS ioctls, packet receive times via `SO_TIMESTAMPNS` (Linux) /
   `SO_TIMESTAMP`+`SO_TS_CLOCK=SO_TS_REALTIME` (FreeBSD). Wall-clock reads in
   the time path use `unix.ClockGettime(CLOCK_REALTIME)`; intervals use the
   monotonic clock. `time.Now()` is fine for logging and scheduling only.
6. **`internal/discipline` is pure and deterministic.** It takes measurements
   and elapsed times as arguments and returns actions (`SetFrequency`, `Slew`,
   `Step`). No goroutines, no channels, no clock reads, no logging inside it.
   That is what makes the simulation tests possible; keep it that way.
7. **One owner per piece of mutable state.** The engine goroutine owns
   discipline state and is the only caller of the clock actuator. Sources send
   `Measurement`s over a channel. The server reads an immutable snapshot
   through `atomic.Pointer[Status]`. No mutex-protected shared bags.
8. **Config is TOML only**, read from `-config`, unknown keys are errors,
   secrets (MAC keys) live in a separate `0600` keys file. No environment
   variables, no `.env`, ever (see global rules).
9. **Stepping the clock is loud and rare.** Steps are logged at WARN with
   before/after, counted in metrics, and only happen under the `[step]` policy
   in `DESIGN.md`. Never add a code path that silently steps.
10. Errors are wrapped with `%w` and carry the device/host/source name.
    Goroutines are started with a context and have a documented shutdown path.
    On exit the daemon leaves the kernel frequency alone (it is the best
    estimate we have) and writes the drift file.

## Platform notes you will need (details and citations in DESIGN.md)

- **FreeBSD serial PPS:** open the callout device `/dev/cuau0` (or `/dev/cuaU0`
  for USB), never `/dev/ttyu0` — the dial-in device waits for DCD, which is
  the PPS line. Capture is enabled with the `dev.uart.<N>.pps_mode` sysctl
  (`0x01` CTS, `0x02` DCD; OR `0x10` to invert, `0x20` for narrow pulses).
  USB serial uses `hw.usb.ucom.pps_mode` (0/1/2 only). The PPS ioctls are then
  issued on the tty fd itself. `ntp_adjtime` has no `x/sys` wrapper on FreeBSD:
  use `unix.Syscall(unix.SYS_NTP_ADJTIME, ...)` with our own `timex`.
- **Linux serial PPS:** DCD only. The daemon sets line discipline `N_PPS` (18)
  on the tty with `TIOCSETD` and resolves the resulting `/dev/ppsN` via
  `/sys/class/pps/ppsN/path`; it must keep the tty fd open for the lifetime.
  Needs the `pps_ldisc` module. A pre-existing `/dev/ppsN` (from `ldattach` or
  `pps-gpio`) can be configured directly instead.
- **Both:** set `CLOCAL` immediately after opening a tty that carries PPS on
  DCD, otherwise the carrier toggling at 1 Hz hangs up the port.
- **Privileges:** FreeBSD runs `carillon` as its own user with the `mac_ntpd(4)`
  policy (`security.mac.ntpd.uid=<carillon uid>`) granting `PRIV_ADJTIME`,
  `PRIV_CLOCK_SETTIME`, `PRIV_NTP_ADJTIME`, `PRIV_NETINET_RESERVEDPORT`, plus
  group `dialer` for the tty. Linux uses a systemd unit with
  `AmbientCapabilities=CAP_SYS_TIME CAP_NET_BIND_SERVICE` and group `dialout`.
  The binary itself does no privilege dropping.

## Deployment conventions

- Config: `/usr/local/etc/carillon/carillon.toml` (FreeBSD) or
  `/etc/carillon/carillon.toml` (Linux); keys file alongside as `keys` (mode `0600`).
- State: `/var/db/carillon/` (FreeBSD) or `/var/lib/carillon/` (Linux) — drift file.
- Control socket: `/var/run/carillon/carillon.sock`; init creates the
  daemon-owned `/var/run/carillon` directory.
- rc.d invokes `/usr/sbin/daemon` with `-f -S -T carillon`, a pidfile, and
  `-u carillon`; `-S` preserves structured stderr in syslog.
- `carillon` must refuse to start if UDP 123 is already bound, with a message that
  names the likely culprit (ntpd, chrony, systemd-timesyncd).

## Git

The repository is on `main` with `origin` configured. Commit directly to
`main` and push useful checkpoints regularly.

## Vocabulary

- **offset** θ: true time minus local clock; positive means the local clock is
  behind. **delay** δ: round-trip. **dispersion** ε: error bound that grows at
  φ = 15 ppm. **root distance** λ: δ/2 + ε (+ jitter), the selection interval.
- **truechimer / falseticker**: source inside / outside the intersection of
  the survivors' correctness intervals (RFC 5905 §11.2.1).
- **numbering source**: any source good to ±0.4 s; required before a PPS
  source can be used, because a PPS edge only says "a second starts here",
  not which second.
- **holdover**: PPS lost, frequency held, dispersion growing.
