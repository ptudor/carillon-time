# carillon design

An NTPv4-compatible time daemon, in Go, for FreeBSD and Linux (OpenWrt later),
built around one precise input — a PPS pulse on a serial-port modem-control
line — and one deployment: a GPS-disciplined home machine that is the trusted
upstream for a colocated server which serves time to the rest of the world.

This document is the specification. When the code and this document disagree,
one of them is wrong and the document is fixed first.

Contents

1. Goals and non-goals
2. Deployment topology
3. Requirements and accuracy targets
4. Architecture
5. Time sources
6. Clock discipline
7. NTP server
8. Wire format and timestamps
9. Configuration
10. Observability and control
11. Privileges, sandboxing, and init integration
12. Process model and concurrency
13. Testing strategy
14. Failure modes and safety
15. Milestones
16. Decision log
17. References

---

## 1. Goals and non-goals

### Goals

- Discipline the system clock from a **PPS signal on a serial port** using the
  RFC 2783 PPS API on FreeBSD (`uart(4)`/`ucom(4)` capture) and Linux
  (`pps_ldisc` or any `/dev/ppsN`), to the accuracy the hardware allows —
  single-digit microseconds on a real UART.
- Number the PPS seconds from either an **NMEA** stream on the same port
  (GPS receiver) or from ordinary **NTP servers** (bare PPS from a GPSDO,
  rubidium, or a receiver with no data line).
- Act as an **NTP client** to upstream servers with the standard RFC 5905
  filter / select / cluster / combine pipeline, so a host with no refclock is
  a well-behaved stratum-2+ client.
- Act as an **NTP server** (modes 3 → 4 only), correctly reporting stratum,
  reference id, root delay, root dispersion, and leap state; safe on the
  public internet (no amplification, rate limited, ACL'd).
- Authenticate the home → colo association with **AES-128-CMAC** message
  authentication (RFC 8573) so the colo trusts only its own upstream.
- One static binary per OS/arch, cross-compiled from macOS. Same code, same
  behaviour, on FreeBSD, Linux, and OpenWrt.

### Non-goals (v1)

- NTP modes 1/2 (symmetric), 5 (broadcast), 6/7 (control/private). 6 and 7
  are never implemented (§16, D5).
- NTS (RFC 8915), interleaved mode, hardware (NIC) timestamping, PTP. All
  are compatible with this design and listed as later work (§15).
- Kernel PPS discipline (`hardpps`, `PPS_SYNC`, `STA_PPSFREQ`). The kernel
  is used only as an actuator (§16, D2).
- A generic refclock driver framework. There are two refclock types, `pps`
  and `gps`; that is the whole zoo.
- Running on macOS. The Mac is a build/test host; the clock and PPS backends
  are compile-time stubs there.

---

## 2. Deployment topology

```
  GPS antenna
      │
  GPS receiver ──NMEA (TX)──┐
      │                     │      home host (FreeBSD or Linux)
      └──PPS──► DCD/CTS ────┴──► /dev/cuau0 ──► carillon  stratum 1, refid "GPS"/"PPS"
                                                   │  serves LAN (unauthenticated)
                                                   │  serves colo (AES-CMAC key 1)
                                          NAT ─────┤  via WireGuard or a UDP/123 forward
                                                   ▼
                                        colo host  carillon  stratum 2
                                          upstream: home (prefer, key 1)
                                                    + 2–3 public servers as falseticker detectors
                                          serves: the internet, rate limited
                                                   │
                                                   ▼
                                               clients (chrony, ntpd, Windows, ...)
```

- **Home** is the only stratum-1. It serves the LAN without authentication and
  the colo with a shared CMAC key. It may also list a few public servers with
  `noselect = true` purely so `carillonctl sources` shows how far the world thinks
  it is from GPS.
- **Colo** polls home (client/server, mode 3/4). Home is `prefer = true`; as
  long as home survives the intersection algorithm its offset is used as-is,
  and the public servers only exist to detect the case where home is wrong.
  If home is voted a falseticker or becomes unreachable the colo falls back to
  the public survivors and logs it at ERROR. Expected colo accuracy is bounded
  by WAN path asymmetry: low milliseconds, not microseconds. The PPS precision
  does not survive the WAN; what the colo gains is a *trusted* upstream.
- **Reaching home from the colo.** Recommended, in order: (a) an existing
  WireGuard tunnel — poll home's tunnel address, one extra encapsulation adds
  negligible, symmetric delay; (b) a UDP/123 port-forward on the home router
  restricted to the colo's source address. Symmetric-active "push" mode was
  considered and rejected (§16, D3).
- **OpenWrt later.** If the home router itself runs `carillon` with a GPS on its
  serial header or a GPIO PPS, it is directly reachable and the NAT question
  disappears. The daemon runs as root under procd there.

---

## 3. Requirements and accuracy targets

| Scenario | Target (steady state) | Limiting factor |
|---|---|---|
| PPS on a real UART (16550 / SoC UART) | ≤ 10 µs RMS offset to the edge; typically 1–5 µs | interrupt latency, oscillator wander |
| PPS via USB serial (`ucom`, `pl2303`, `ftdi_sio`) | ~100 µs – 1 ms | USB frame polling (1 ms / 125 µs) |
| NMEA only (no PPS) | ~10–50 ms after offset calibration | sentence timing jitter |
| NTP client, LAN | ≤ 100 µs | server quality, switch queuing |
| NTP client, WAN (colo ← home) | 1–5 ms | path asymmetry |
| Server response accuracy | RX kernel-timestamped; TX user-space, tens of µs | none worth fixing before interleaved mode |

Other requirements:

- **Platforms:** FreeBSD 14+ amd64/arm64; Linux 5.10+ amd64/arm64; OpenWrt
  (Linux, `mipsle`/`arm`/`arm64`) later. Build-only on darwin.
- **Startup:** with `iburst` on a reachable server, first correction within
  ~10 s; PPS lock (see §6.5) within one averaging window after a numbering
  source is available.
- **Never a hazard to the host.** No stepping after startup unless the
  configured policy allows it; frequency bounded ±500 ppm by the kernel
  anyway; refuse absurd corrections (§6.6).
- **Compatibility:** interoperates as client and server with ntpd, chrony,
  systemd-timesyncd, Windows Time, and each other; responds to NTP versions
  1–4 with the request's version number.
- **Footprint:** OpenWrt binary ≤ ~7 MB stripped; no cgo; no runtime files
  other than config, keys, drift, control socket.

---

## 4. Architecture

```
            ┌────────────────────────────────────────────────────────────┐
            │                        engine goroutine                    │
  sources   │   Measurement ──► per-source filter ──► select/cluster ──►  │
  ────────► │                                        combine ──► loop ───┼──► clock actuator
  (chan)    │                                                   │        │    (SetFrequency / Step)
            │                          Status snapshot ◄────────┘        │
            └───────────────┬────────────────────────────┬───────────────┘
                            │ atomic.Pointer[Status]     │ control requests
                            ▼                            ▼
                     server goroutines             control / monitoring
                     (one per listen socket)       (carillonctl, JSON, Prometheus)

  source goroutines:  ntp poller ×N   |   pps fetch loop   |   nmea reader
```

Packages (see `CLAUDE.md` for the tree):

- `internal/ntp` — packet encode/decode, 64-bit timestamp ↔ `time.Time` with
  era handling, MAC append/verify, KoD codes. Pure; fuzz-tested.
- `internal/source` — the `Source` interface and the NTP client source.
- `internal/refclock` — `pps` and `gps` refclocks, NMEA parser.
- `internal/pps` — RFC 2783 bindings per OS.
- `internal/serial` — termios setup; Linux `N_PPS` attach.
- `internal/clock` — `Clock` actuator interface; real backends per OS; `Fake`.
- `internal/discipline` — filter, selection, clustering, combining, loop.
  Pure functions over values; the simulation tests live here.
- `internal/engine` — owns state; single goroutine; wires everything.
- `internal/server` — listeners, responder, ACL, rate limiter.
- `internal/control` — local unix-socket status and `carillonctl` protocol.
- `internal/monitor` — read-only HTTP status, health and Prometheus endpoints.

Data types shared across packages:

```go
// Measurement is what a source delivers after its own clock filter has run.
// Offsets are "true minus local": positive means the local clock is behind.
type Measurement struct {
    Source string
    Now    float64   // monotonic seconds when produced
    Reach  uint8     // the source's reach register after this poll
    Poll   int8      // the source's current poll exponent
    Valid  bool      // false = a poll without a new estimate (only Reach/Poll count)

    At         float64  // monotonic time of the sample the filter chose
    Offset     float64  // θ, seconds
    Delay      float64  // δ, seconds (0 for a refclock)
    Dispersion float64  // ε, seconds
    Jitter     float64  // ψ, seconds

    Leap        ntp.Leap
    Stratum     uint8    // 0 for a refclock
    RefID       ntp.RefID // the source's own reference id
    SourceRefID ntp.RefID // what we advertise when this source drives us
    RootDelay   float64
    RootDisp    float64
    Precision   int8
    RefTime     time.Time
}
```

---

## 5. Time sources

### 5.1 Source interface

```go
type Source interface {
    Name() string
    Run(ctx context.Context, out chan<- discipline.Measurement) error // returns when ctx is done or on a fatal error
    Reset()                                                            // discard samples taken before a clock step
    Info() Info                                                        // lock-free snapshot for carillonctl
}
```

Every source keeps an 8-bit **reach register**, shifted left on each expected
sample slot with the low bit set when a sample arrived, and reports it in
every `Measurement`. A source with reach 0 is unreachable and excluded from
selection. NTP sources shift once per poll; refclocks once per second. The
RFC 5905 clock filter (`discipline.Filter`) also runs inside the source, so
a `Measurement` carries a filtered estimate; `Valid = false` reports a poll
that produced no new estimate. After a clock step the engine calls `Reset()`
on every source, because samples taken before the step are wrong by the step.

Per-source config common to all types: `prefer` and `noselect`. There is no
per-source weight; combining is by root distance (§6.3).

### 5.2 PPS refclock (`type = "pps"`)

The reason the project exists.

**Signal path.** A PPS source drives a modem-control input of a serial port:
DCD (DE-9 pin 1) or CTS (pin 8). The UART raises an interrupt on the line
change; the kernel's PPS layer timestamps it with `CLOCK_REALTIME` in the
interrupt handler and stores it with a sequence number. Nothing in user space
is in that path, which is why a Go daemon is as accurate as a C one here.

**FreeBSD.** `uart(4)` implements PPS capture natively. Configure with the
`dev.uart.<N>.pps_mode` sysctl (or `hw.uart.pps_mode` as a loader tunable for
all units): `0x00` disabled, `0x01` CTS, `0x02` DCD; OR in `0x10` to invert
(TTL-level wiring, or a receiver with active-low pulses) and `0x20` to
attempt narrow-pulse capture (the driver then synthesises ASSERT and CLEAR
with the same timestamp). `ucom(4)` (USB serial) uses `hw.usb.ucom.pps_mode`
with plain values 0/1/2. The PPS ioctls (`PPS_IOC_SETPARAMS`, `PPS_IOC_GETCAP`,
`PPS_IOC_FETCH` from `<sys/timepps.h>`) are issued on the tty fd itself.
Open the **callout** device (`/dev/cuau0`, `/dev/cuaU0` for USB) —
`/dev/ttyu0` blocks in `open(2)` until carrier is asserted, and carrier is
the PPS line. `PPS_IOC_FETCH` blocks until the next event when the timeout
is non-zero; `tv_sec = -1` waits forever.

The daemon reads `dev.uart.<N>.pps_mode` (via `unix.Sysctl`) when the device
is `/dev/cuau<N>` and logs an ERROR at startup if it is 0 — the most likely
"why is there no PPS" mistake.

**Linux.** PPS on a serial port means the `pps_ldisc` line discipline on the
tty, which captures **DCD only**: ASSERT on DCD going active, CLEAR on it
going inactive. Configuration is either

- `device = "/dev/ttyS0"` — the daemon opens the tty, sets `CLOCAL`, does
  `ioctl(fd, TIOCSETD, N_PPS)` (18), then scans `/sys/class/pps/pps*/path`
  for the entry equal to the tty path and opens that `/dev/ppsN`. The tty fd
  is held open for the lifetime of the source (closing it detaches the
  discipline). Requires the `pps_ldisc` module; the error message says so.
- `device = "/dev/pps0"` — a PPS device somebody else created (`ldattach PPS
  /dev/ttyS0`, `pps-gpio` on a Raspberry Pi/OpenWrt, `pps-ktimer` for tests).

`PPS_FETCH` (`<linux/pps.h>`) blocks until the next event; a timeout with
`flags = PPS_TIME_INVALID` waits forever, zero returns immediately.

**Struct layouts** for both OSes are declared by hand in `internal/pps`
(`pps_params`, `pps_info`, FreeBSD `pps_fetch_args`, Linux `pps_kparams`,
`pps_kinfo`, `pps_fdata`, `pps_ktime`) per `GOARCH`, with `hwtest`-tagged cgo
size/offset tests.

**Both:** set `CLOCAL` in the very first `tcsetattr` after `open`, because a
DCD drop on a non-`CLOCAL` tty is a hangup. On Linux, if the first open loses
that race (reads return `EIO`), close and reopen once.

**Configuration.**

```toml
[[refclock]]
name      = "pps0"
type      = "pps"
device    = "/dev/cuau0"    # FreeBSD tty, Linux tty, or Linux /dev/ppsN
edge      = "assert"        # or "clear"; choose the edge that is the second boundary
offset    = 0.0             # seconds added to every sample (cable/driver delay)
prefer    = true
lock_jitter = 0.0002        # seconds; window MAD below this = "locked" (UART: set ~20e-6)
poll_min  = 4               # log2 seconds; averaging window 16 s ...
poll_max  = 7               # ... up to 128 s
```

**Sample rule.** Each fetch yields `(seq, ts)` for the configured edge.
The PPS offset is the distance from `ts` to the nearest whole second:

```
frac = ts.nsec / 1e9
θ    = -frac         if frac < 0.5
θ    = 1 - frac      otherwise
```

This is only valid when the local clock is already within ±0.5 s of true
time. The engine therefore marks a PPS source **qualified** only while at
least one selected non-PPS source with `Numbering = true` reports
`|θ| < 0.4 s` (0.1 s guard band). Unqualified PPS samples are still collected
and reported by `carillonctl` but never fed to the loop. With no numbering source
at all, the daemon logs once at ERROR: *PPS present but nothing to number its
seconds — add an NTP server or use a gps refclock*.

**Validation** (each applies before the sample enters the window):

- `seq` must be `prev + 1`; a gap shifts the reach register by the number of
  missed pulses and resets nothing else.
- `ts - prev_ts` must be within 1 s ± 100 ms (glitch / double pulse).
- `|θ - median(window)| ≤ max(5·MAD(window), 1 µs)` once the window has ≥ 4
  samples (spike rejection).

**Filter.** A ring of the last `2^poll` accepted samples. Every `2^poll`
seconds the source emits one `Measurement` with `Offset = median`,
`Dispersion = MAD × 1.4826` (robust σ) plus configured `offset`; `Delay = 0`;
`Precision` from the clock's measured resolution; `RefID = "PPS "`. `poll`
adapts like an NTP source: up when the window σ is below `lock_jitter` and
the loop's residual offset is under 4σ, down when σ grows.

### 5.3 GPS refclock (`type = "gps"`)

One serial device that provides both an NMEA data stream and a PPS edge.
Internally it is a PPS source (as above, same fd) plus an NMEA source, and
produces two logical sources `<name>/pps` and `<name>/nmea` so they can be
selected and displayed independently.

```toml
[[refclock]]
name        = "gps"
type        = "gps"
device      = "/dev/cuau0"
baud        = 9600
pps         = "dcd"         # "dcd", "cts", "none", or a Linux "/dev/ppsN" path
pps_edge    = "assert"
pps_offset  = 0.0
nmea_offset = 0.150         # seconds: measured lag of the sentence behind its PPS edge
sentences   = ["RMC", "ZDA"]  # accepted talkers: GP, GN, GL, GA, BD
prefer      = true
```

**Serial setup:** raw mode, 8N1, `CLOCAL`, `CREAD`, no flow control, no echo,
`VMIN=1, VTIME=0`; opened `O_RDWR|O_NOCTTY|O_NONBLOCK`, `O_NONBLOCK` cleared
after configuration. Reads use a buffered line scanner; the timestamp of a
sentence is the `CLOCK_REALTIME` read taken when the read that delivered its
`$` returned (at 9600 baud a 70-byte RMC takes ~73 ms to arrive; the start
character is the anchor).

**NMEA parsing:** checksum mandatory; `RMC` (status `A`, date `ddmmyy`, time
`hhmmss.ss`), `ZDA` (full year, preferred when present), `GGA` only for fix
quality reporting. Talker prefixes `GP`, `GN`, `GL`, `GA`, `BD`. A sentence
whose time has a non-zero fractional second is ignored (it is not aligned to
a PPS edge). Two-digit RMC years are `20yy`; any decoded date earlier than the
binary's build date is rejected and logged as *suspected GPS week rollover*
(1024-week rollovers: 1999-08, 2019-04, 2038-11). The build date is embedded
via `debug.ReadBuildInfo` (`vcs.time`) with an `-ldflags -X` override for
non-VCS builds.

**NMEA measurement:** `θ = sentence_time − arrival_time + nmea_offset`,
`Dispersion` = robust σ of the last 16 sentences (tens of ms), `Numbering =
true`, `RefID = "GPS "`, reach shifted once per second. When PPS is also
present and locked, the source records the median of `(arrival − pps_edge)`
and exposes it in `carillonctl refclock` as the suggested `nmea_offset`; it does
not apply it automatically in v1 (§15).

When PPS is locked, the NMEA logical source is not demoted to `noselect`.
It stays a normal source with a large dispersion; the intersection and
clustering steps keep it as a survivor (it is *correct*, just noisy) and it is
exactly what numbers the PPS seconds. Combining is by root distance, so its
contribution to the combined offset would be negligible anyway, and the PPS
`prefer` rule (§6.3) bypasses combining entirely.

### 5.4 NTP client source

Standard RFC 5905 client, one goroutine per configured server.

```toml
[[server]]
name     = "home"
address  = "home.tunnel.invalid:123"  # host or IP, optional :port; IPv4/IPv6
key      = 1                          # key id from the keys file; omit = unauthenticated
prefer   = true
iburst   = true
poll_min = 4                          # log2 seconds
poll_max = 6
noselect = false
```

**On the wire:**

- A fresh UDP socket per request, bound to an ephemeral port (RFC 9109 port
  randomisation), connected to the server address, closed after the reply or
  timeout.
- A hostname is resolved once, when the source starts, and re-resolved only
  after 8 consecutive timeouts (the server may have moved). There is no
  periodic re-resolution. A pool name such as `2.fedora.pool.ntp.org` answers
  with a different server on every lookup, and the first soak (2026-08-23)
  showed the earlier 1024 s re-resolution silently changing every source's
  server about every 17 minutes while its clock filter blended samples from
  unrelated servers: 20 ms jitter, a system source that changed every few
  minutes, and 30–40 ms root dispersion. When a re-resolution yields a
  different address the source's clock filter is reset, since its samples
  describe the previous server; the same address keeps the filter, and stale
  samples age out through dispersion.
- Client packets: LI=0, VN=4, mode 3, poll = current poll exponent, precision
  from the clock, root delay/dispersion = 0, refid 0, receive/origin = 0,
  **transmit = 64 random bits** used purely as a nonce; the real T1 is kept
  locally. The reply's origin timestamp must equal the nonce or the reply is
  dropped (off-path spoof / stale reply protection).
- Receive timestamp T4 from the kernel (§8.3). T2/T3 from the packet.
  `θ = ((T2−T1) + (T3−T4)) / 2`, `δ = (T4−T1) − (T3−T2)`.
- Reply sanity: mode 4, stratum 1..15, LI ≠ 3, `δ ≥ 0` (else drop as
  "bogus"), root distance sane (`< 1 s` unless configured otherwise),
  reference time not in the future by more than 1 s.
- **Kiss-o'-Death** (stratum 0): `RATE` → double the poll interval and honour
  the packet's poll field as the new minimum; `DENY`/`RSTR` → stop polling that
  server and log ERROR. Any KoD packet that carries a valid MAC is honoured;
  unauthenticated KoDs are honoured only for `RATE`.
- `iburst`: a burst of 4 requests 2 s apart on start-up and whenever the
  server transitions from unreachable to reachable.
- **Authentication:** if `key` is set, append a MAC (§8.4) to requests and
  require a valid MAC on replies — unauthenticated replies are dropped and
  counted.
- Timeout 2 s per request; a timeout is a missed reach slot.

**Clock filter** (RFC 5905 §10, kept as specified): the last 8 samples
`(θ, δ, ε, t)`; the sample with minimum δ is chosen; `jitter` = RMS of the
other samples' offsets relative to it; `ε` ages at φ = 15 ppm; a sample older
than the chosen one is never used again (the "popcorn"/staleness rule). Each
new filter output that differs from the previous one produces a
`Measurement`.

**Poll adaptation:** after each update, if `|θ| < 4·jitter` the poll exponent
increments (until `poll_max`), else it decrements by one (down to
`poll_min`). Exactly ntpd's rule, and it keeps public-pool load polite.

---

## 6. Clock discipline

### 6.1 Model

Local clock `T_loc`, true time `T_true`, offset `θ = T_true − T_loc`,
fractional frequency error `f` in ppm. The kernel exposes one frequency word
(scaled ppm; positive speeds the clock up) and one way to step. The daemon
does **all** filtering, selection, and loop control in user space and uses the
kernel only as the actuator (§16, D2). Slewing is done by temporarily offsetting
the frequency word (§6.4), so behaviour is identical on every OS.

### 6.2 Pipeline

For every `Measurement` the engine runs:

1. update the source's state (filter output, reach, dispersion aging)
2. **selection** across all reachable, non-`noselect` sources (§6.3)
3. **combine** survivors into `(θ_sys, ψ_sys, λ_sys)`
4. **loop update** (§6.4) → actions for the actuator
5. publish a new `Status` snapshot (server variables, `carillonctl` data)

Steps 2–4 are pure functions in `internal/discipline`; the engine is the only
thing that calls them and the only thing that touches the actuator.

### 6.3 Selection, clustering, combining

Implemented as in RFC 5905 §11.2, no deviations:

- **Root distance** `λ = (rootdelay + δ)/2 + rootdisp + ε + ψ`, where `ψ` is
  the source's filter jitter.
- **Intersection (Marzullo/Mills):** each candidate contributes the interval
  `[θ−λ, θ+λ]`; find the largest set of intervals with a common point,
  tolerating `f` falsetickers where `f < n/2`. Candidates outside are
  falsetickers and are logged at WARN the first time they become one.
- **Cluster:** repeatedly discard the survivor with the largest *selection
  jitter* (RMS distance to the other survivors) while it exceeds that
  survivor's own jitter and more than `min_survivors` (default 1; set 3 on a
  host with many upstreams) remain.
- **Combine:** weighted mean with weights `1/λ`; `ψ_sys` is the weighted RMS
  of survivor offsets around the mean.
- **prefer:** if a `prefer` source is among the survivors, `θ_sys` is *that
  source's* offset and the other survivors only contribute to `ψ_sys`. If it
  is a falseticker or unreachable, combining proceeds without it and the
  status shows `prefer_lost = true` (metric + ERROR log, once per transition).
- **PPS override:** a qualified (§5.2), locked PPS source that is also
  `prefer` is the system source outright; nothing else contributes to
  `θ_sys`. This is the ntpd "PPS peer" behaviour and is what makes the home
  host stratum 1 with a `GPS`/`PPS` refid.

The **system source** (the survivor with the smallest `λ`, or the prefer/PPS
source) supplies stratum (+1), refid, root delay (+δ), root dispersion, leap
bits, and reference time for the server variables (§7.3).

### 6.4 Loop

A critically damped type-II phase-locked loop. The *structure* is ntpd's
(RFC 5905 §11.3): phase is slewed exponentially, frequency is integrated
from the offset, one time constant tied to the poll interval, a popcorn
spike gate and a clock-jitter estimate. The *gains* are not ntpd's — see D8.

- `freq` — base frequency correction (ppm), applied to the kernel
- `pending` — residual phase still to be slewed (seconds)
- `P = 2^poll` — the system source's poll/averaging interval
- `τ = 4·P` — loop time constant (`TimeConstant`, default 4)
- `μ` — seconds since the previous loop update

Per update with `θ = θ_sys`:

```
if the step policy fires (§6.6): Step(θ); pending = 0; Reset() every source; return
if synced and |θ − θ_prev| > 3·ψ_clk and μ < 2P: ignore it (popcorn spike); return
ψ_clk   = exp. average (weight 1/8) of |θ − θ_prev|, floored at the clock precision
pending = θ                                       # replace, don't accumulate
if |θ| ≤ max_slew·τ:                              # linear region only — see anti-windup
    freq += θ · min(μ, 2048) / (4τ²) · 1e6        # 2048 s = Allan intercept
freq    = clamp(freq, ±500)
```

Every second (engine ticker):

```
adj      = clamp(pending / τ, ±max_slew)          # max_slew = max_slew_ppm · 1e-6
total    = clamp(freq + adj·1e6, ±500)            # the kernel's own limit
pending -= (total − freq)·1e-6                    # only what the clamp let through
SetFrequency(total)                               # holds for one second
```

**Why these gains.** With phase gain 1/τ and integral gain K, the closed
loop is `s² + s/τ + K = 0`. `K = 1/(4τ²)` puts a double pole at `−1/(2τ)`:
critically damped, no overshoot, settled in a few × 2τ — 512 s at poll 6,
128 s at poll 4. ntpd uses τ = 16·P and K = 1/(16τ²), which leaves a slow
real pole near `−1/(15τ)`: hours at poll 6, days at poll 10 (where ntpd
relies on its FLL instead). That buys noise immunity a PPS-disciplined host
does not need and a client already gets from the clock filter.

**Anti-windup.** A slew saturated at 500 ppm cannot follow the offset, and
integrating the offset meanwhile would wind the frequency straight to the
clamp. So the frequency is not integrated while `|θ|` exceeds what one τ can
slew (`max_slew·τ`, 0.128 s at poll 6); the phase runs at the limit and the
integral resumes in the linear region.

**Initial frequency:** the drift file (§10.4), else the kernel's current
frequency word if non-zero (a previous daemon left it), else unknown. When
unknown, the first `FreqMeasure` = 900 s are a direct measurement — ntpd's
FREQ state, but without withholding phase corrections: at the end,
`freq += (θ_now − θ_first + slewed)/elapsed`, where `slewed` is the phase the
ticks removed meanwhile. The PLL takes over afterwards.

**Measured in simulation** (`internal/discipline/sim_test.go`, 200 µs delay
noise, poll 6): 50 ppm error and 100 ms offset from nothing → 49.99 ppm
recovered, 27 µs RMS in hour 6, no step; with a drift file, under 1 ms in
about ten minutes; a 1 s jump after startup is slewed at exactly the 500 ppm
bound with no frequency disturbance.

**Estimator seam:** `Loop.Update/Tick` is the whole interface; a
regression-based estimator (chrony's approach) can replace it without
touching the engine.

### 6.5 States

```
  UNSYNCED ──first update──► SETTLING ──|θ|<4ψ for 3 updates──► SYNCED
      ▲                          │                                 │
      └──── all sources lost ────┴───── (holdover timeout) ◄───────┘
                                                        HOLDOVER: PPS/all sources unreachable,
                                                        freq held, root dispersion growing at φ
```

- **UNSYNCED:** server replies LI=3, stratum 16 (§7.4); kernel `STA_UNSYNC`
  set; no frequency changes have been applied yet.
- **SETTLING:** corrections are applied; the server still answers as
  unsynchronized until three loop updates have gone by without a step. (The
  root dispersion, which includes the pending slew, tells clients the truth
  from then on; waiting for the offset to shrink first would keep a slowly
  converging client unsynced for an hour for no gain.)
- **SYNCED:** `STA_UNSYNC` cleared, maxerror/esterror maintained; server
  serves. Clearing `STA_UNSYNC` is also what allows the kernel to write the
  clock back to the RTC periodically (Linux's 11-minute mode).
- **HOLDOVER:** the system source became unreachable. The frequency is held
  and the root dispersion reported by the server grows at φ = 15 ppm from the
  last update, which is the honest number. After `holdover_max` (default
  3600 s) without any survivor the daemon returns to UNSYNCED and sets
  `STA_UNSYNC`. If other survivors exist (e.g. NTP servers after PPS loss),
  they become the system source immediately and there is no holdover, only a
  stratum change.

PPS-specific: **locked** means the PPS window σ < `lock_jitter` and the source
is qualified; lock is lost after 8 missed pulses (reach 0) or when σ exceeds
4× `lock_jitter`.

### 6.6 Stepping policy

```toml
[step]
threshold        = 0.5    # seconds; |θ| beyond this is stepped instead of slewed ...
limit            = 3      # ... but only within the first N loop updates after start;
                          # 0 = never step, -1 = always allowed (not recommended on a server)
panic            = 1000   # seconds; refuse to correct more than this ...
panic_at_startup = false  # ... unless set, in which case it is allowed for the very first correction
                          # (OpenWrt default: true — no RTC, the clock starts in 1970/build time)
```

Semantics match chrony's `makestep 0.5 3` plus ntpd's panic gate. A refused
panic correction is fatal: the daemon logs the offset and exits 1 rather than
serve or slew nonsense; the init script's restart backoff makes this visible
rather than harmful. Every step is logged at WARN with before/after times and
the source that justified it, and increments `carillon_steps_total`.

Backward steps are as allowed as forward ones under this policy (after the
startup window neither happens by default). Programs that cannot tolerate a
backward step at boot (databases) should be ordered after `carillon` has reached
SYNCED; `carillonctl waitsync [timeout]` exists for init scripts.

### 6.7 Actuator interface and per-OS implementation

```go
type Clock interface {
    Now() (unix.Timespec, error)           // CLOCK_REALTIME
    Frequency() (ppm float64, err error)   // current kernel frequency word
    SetFrequency(ppm float64) error
    Step(delta float64) error              // add delta seconds atomically where the OS can
    SetStatus(synced bool, leap Leap, maxerr, esterr float64) error
    Precision() int8                        // log2 seconds, measured once at start
}
```

- **Linux:** `unix.Adjtimex` / `unix.ClockAdjtime(CLOCK_REALTIME)` with
  `ADJ_FREQUENCY` (`freq = ppm × 65536`), `ADJ_STATUS` (clear `STA_PLL`,
  manage `STA_UNSYNC`, `STA_INS`, `STA_DEL`), `ADJ_MAXERROR`, `ADJ_ESTERROR`,
  `ADJ_NANO`; steps via `ADJ_SETOFFSET|ADJ_NANO` (atomic, no read-modify-write
  race).
- **FreeBSD:** `ntp_adjtime(2)` through `unix.Syscall(unix.SYS_NTP_ADJTIME)`
  with a hand-declared `timex` (all `long` fields are 64-bit on amd64/arm64),
  same `MOD_FREQUENCY`/`MOD_STATUS`/`MOD_MAXERROR`/`MOD_ESTERROR` semantics;
  steps via `clock_gettime` + `clock_settime` back-to-back (raw
  `SYS_CLOCK_SETTIME`; `x/sys` has no wrapper) — the read-to-write gap is a
  few hundred nanoseconds and is accepted. `adjtime(2)` is deliberately not
  used for slewing (its 5000 ppm rate is uncontrolled from our side).
- **Kernel PLL is disabled** (`STA_PLL` clear) on both, so the kernel never
  fights the daemon. `STA_PPSFREQ`/`STA_PPSTIME` are never set.
- **Precision** is measured at start as log2 of the minimum delta between
  successive `CLOCK_REALTIME` reads, clamped to `[-30, -6]`.
- **darwin / other:** every method returns `ErrUnsupportedPlatform`;
  `carillon query` and all tests still work.

### 6.8 Leap seconds

Leap indication is taken from, in priority order: a configured
`leapfile` (NIST/IERS `leap-seconds.list`, the format ntpd and chrony read;
its expiry is a WARN 30 days out and an ERROR when past), then the majority
LI of the survivors. NMEA carries no leap warning, so a stratum-1 host with no
upstream servers **must** configure `leapfile` to announce leaps; this is
called out by `carillon -check`.

`leap_mode = "kernel"` (v1 only): on the last day of June or December with a
pending leap, set `STA_INS`/`STA_DEL` at 00:00 UTC; the kernel performs the
insertion at midnight; the flag is cleared and the PPS/NMEA filters are reset
afterwards because the PPS seconds numbering shifts by one. A `"slew"` mode
(spread the second over a window, chrony-style) is future work (§15).

---

## 7. NTP server

### 7.1 Listening

```toml
[serve]                                             # not [server]: that is the upstream array
listen         = ["0.0.0.0:123", "[::]:123"]
allow          = ["10.0.0.0/8", "2001:db8::/32"]   # required; empty = server disabled
deny           = []
require_key    = { "203.0.113.7/32" = 1 }          # prefix → key id: MAC mandatory from these
rate_limit_pps = 8                                  # per client address, token bucket
rate_burst     = 16
kod            = true                               # send RATE KoD when limiting
```

One goroutine per listen socket, `recvmsg` loop with a control-message buffer
for the receive timestamp and the destination address (`IP_PKTINFO` /
`IP_RECVDSTADDR`, `IPV6_RECVPKTINFO`); replies are sent from the address the
request arrived on (`IP_PKTINFO` / FreeBSD `IP_SENDSRCADDR`, `IPV6_PKTINFO`).
The server is not a client: it never sends anything except a reply to a
request it received.

### 7.2 Request handling

For each datagram:

1. Length ≥ 48. Modes other than 3 (client) are dropped silently — including
   1, 2, 5, 6, 7. Version 1–4 accepted; the reply carries the request's
   version.
2. ACL: `deny` first, then `allow`; no match → drop, count `denied`.
3. Rate limit per source address (token bucket, `rate_limit_pps`/
   `rate_burst`, entries expire after 60 s idle, table capped at 65 536
   addresses with LRU eviction). Over limit → if `kod`, reply with stratum 0,
   refid `RATE`, poll = our suggested minimum, LI=3, at most once per 4 s per
   client; else drop.
4. Authentication: if the request carries a MAC and the key id is known,
   verify; a bad MAC → drop, count `badauth`. If the source matches
   `require_key` and the request has no valid MAC with *that* key → drop.
   A valid MAC on the request produces a MAC on the reply with the same key.
5. Build the reply (§7.3). Its length is exactly the request's: 48 bytes, or
   48 + 20 when the request carried a MAC that verified. Extension fields in
   the request are never echoed, so a reply is never longer than what was
   received.
6. Transmit timestamp is read immediately before `sendmsg`.

Extension fields in requests are ignored (skipped to find a trailing MAC per
RFC 7822 framing) and never echoed.

### 7.3 Server variables

Maintained in the `Status` snapshot by the engine after every loop update:

| Field | Value |
|---|---|
| LI | 0 normal; 1/2 pending leap; 3 when not SYNCED |
| Stratum | 1 with a refclock system source; upstream stratum + 1 otherwise; 16 when not SYNCED (§7.4) |
| Precision | measured (§6.7) |
| Root delay | system source's root delay + its δ (0 for refclocks) |
| Root dispersion | source root dispersion + source ε + ψ_sys + φ·(now − last update) + |pending| |
| Reference ID | `GPS `/`PPS ` at stratum 1; IPv4 address of the system source; first 4 bytes of MD5(IPv6 address) per RFC 5905 |
| Reference timestamp | time of the last loop update |
| Poll | copied from the request |

### 7.4 Unsynchronised behaviour

When not SYNCED the server still answers (clients then see a rejected source
rather than a timeout, which is easier to diagnose): LI=3, packet stratum
**16** (not 0 — stratum 0 means Kiss-o'-Death), refid `INIT` before the first
update, `STEP` just after a step, `HOLD` after holdover expiry, following
ntpd's conventions so chrony/ntpd users recognise them. Root dispersion is
reported as 16 s.

---

## 8. Wire format and timestamps

### 8.1 Packet

RFC 5905 §7.3, 48-byte header, big-endian. `internal/ntp` provides
`Decode([]byte) (Packet, MAC, error)` and `(Packet) Encode(buf []byte) []byte`,
with all 64-bit timestamps as `ntp.Time` (uint64) and helpers to and from
`time.Time` / `unix.Timespec`.

### 8.2 Eras

NTP timestamps are seconds since 1900-01-01 modulo 2^32; era 0 ends
2036-02-07T06:28:16Z. Conversion to absolute time picks the era that places
the value closest to the local clock (RFC 5905 §6, and the fact that any
source more than 68 years off is discarded anyway). Unix ↔ NTP seconds offset
is 2 208 988 800. Round-trip tests cover 2036 and 2038 boundaries.

Short-format 32-bit fields (root delay/dispersion): 16.16 fixed point,
saturating on encode.

### 8.3 Receive timestamps

| OS | Socket option | Control message |
|---|---|---|
| Linux | `SO_TIMESTAMPNS` (`SO_TIMESTAMPING` with `SOF_TIMESTAMPING_RX_SOFTWARE\|SOFTWARE` later, for TX) | `SCM_TIMESTAMPNS` → `timespec` |
| FreeBSD | `SO_TIMESTAMP` + `SO_TS_CLOCK = SO_TS_REALTIME` | `SCM_REALTIME` → `timespec` |

If the control message is missing on a packet (should not happen) the
user-space read time is used and a counter is incremented. Transmit
timestamps are user space in v1 on both client and server.

### 8.4 Authentication (MAC)

RFC 5905 §7.3 MAC trailer with **AES-128-CMAC** (RFC 8573): 4-byte key id +
16-byte tag over the 48-byte header (plus any extension fields). Keys are
16 bytes. The MD5 MAC of RFC 5905 is **not** implemented — ntpd ≥ 4.2.8p11 and
chrony ≥ 4.0 both support `AES128CMAC`, and this daemon's only authenticated
peer is another instance of itself.

Keys file (`/usr/local/etc/carillon/keys`, mode `0600`, ntpd `ntp.keys` syntax so
one file can be shared with chrony/ntpd hosts):

```
# id  type        key (hex, 32 hex digits)
1     AES128CMAC  <generate with: openssl rand -hex 16>
```

`carillon -check` refuses a keys file that is group/world readable.

---

## 9. Configuration

TOML, strict (unknown keys are errors), loaded once at start; `SIGHUP` is not a
reload (restart is cheap and the drift file preserves the frequency). Defaults
are per OS. Naming: `[[server]]` entries are upstreams *we poll*; the listener
*we serve from* is `[serve]` (TOML cannot have both a table and an array of
tables called `server`). The keys file path is `[daemon] keys` because both
the client and the server side use it.

Full example, **home** (stratum 1):

```toml
# /usr/local/etc/carillon/carillon.toml
[daemon]
drift_file = "/var/db/carillon/drift"
control    = "/var/run/carillon/carillon.sock"
leapfile   = "/var/db/carillon/leap-seconds.list"
keys       = "/usr/local/etc/carillon/keys"
log_level  = "info"

[[refclock]]
name        = "gps"
type        = "gps"
device      = "/dev/cuau0"
baud        = 9600
pps         = "dcd"
pps_edge    = "assert"
nmea_offset = 0.150
prefer      = true
lock_jitter = 0.00002

[[server]]                     # sanity only; never selected
name     = "pool-a"
address  = "0.freebsd.pool.ntp.org"
noselect = true

[serve]
listen      = ["0.0.0.0:123", "[::]:123"]
allow       = ["192.168.1.0/24", "203.0.113.7/32"]   # LAN + colo
require_key = { "203.0.113.7/32" = 1 }

[step]
threshold = 0.5
limit     = 3

[monitor]
listen = "127.0.0.1:9123"
id     = "home"
name   = "Home GPS"
roles  = ["reference", "internal-server"]
```

**Colo** (stratum 2):

```toml
[daemon]
drift_file = "/var/db/carillon/drift"
control    = "/var/run/carillon/carillon.sock"
keys       = "/usr/local/etc/carillon/keys"

[[server]]
name     = "home"
address  = "10.9.0.1:123"        # WireGuard address of home
key      = 1
prefer   = true
iburst   = true
poll_min = 4
poll_max = 6

[[server]]
name    = "pool-a"
address = "0.pool.ntp.org"
[[server]]
name    = "pool-b"
address = "1.pool.ntp.org"
[[server]]
name    = "pool-c"
address = "2.pool.ntp.org"

[serve]
listen         = ["0.0.0.0:123", "[::]:123"]
allow          = ["0.0.0.0/0", "::/0"]
rate_limit_pps = 4
rate_burst     = 8

[discipline]
min_survivors = 1
holdover_max  = 3600
max_slew_ppm  = 500

[step]
threshold = 0.5
limit     = 3
```

Bare-PPS client (`pps` refclock numbered by the colo/home NTP server) is the
first config above with the `gps` block replaced by a `pps` block and a
`[[server]]` without `noselect`.

`carillon -check` validates the file, resolves nothing over the network, checks
device existence and permissions, keys-file mode, `pps_mode` on FreeBSD, and
warns about the stratum-1-without-leapfile case.

---

## 10. Observability and control

### 10.1 Logging

`log/slog`, text handler to stderr (rc.d's `daemon(8)`, systemd, and procd
all capture it); `log_level` in config. Events at WARN or above are the ones
an operator needs: steps, falsetickers, prefer lost/regained, PPS lock
lost/regained, KoD received, keys-file problems, leap flag set. Nothing per
packet above DEBUG.

### 10.2 Control socket and `carillonctl`

Unix socket, newline-delimited JSON request/response, `0660`. Commands:

- `tracking` — state, system source, θ, freq, ψ, root delay/dispersion,
  stratum, refid, leap, last update, pending slew, steps.
- `sources` — per source: reach (octal), poll, θ, δ, ψ, λ, selection status
  (`falseticker`/`survivor`/`system`/`noselect`/`unreachable`), prefer.
- `refclock` — PPS window σ, locked, qualified, sequence gaps, pulse
  interval σ, measured NMEA lag, fix status, satellites.
- `serverstats` — served/denied/ratelimited/badauth/unsynced counters.
- `waitsync [seconds]` — block until SYNCED or timeout; exit status for init.

`carillonctl` prints these as aligned tables; `-json` passes the raw reply through.

### 10.3 HTTP monitoring

The optional `[monitor]` listener is a read-only network observability surface.
It never changes daemon state and is not a remote form of the control socket.
It is disabled when `listen` is empty. Its request ACL defaults to loopback,
even when an operator explicitly binds a non-loopback address:

```toml
[monitor]
listen = "127.0.0.1:9123"
allow  = ["127.0.0.0/8", "::1/128"]
id     = "twocom"
name   = "Twocom"
roles  = ["colo", "ntp-pool"]
```

`listen` is one numeric TCP address and non-zero port. `allow` is checked
against the immediate TCP peer only; forwarded-address headers are ignored.
For a LAN-direct iPhone client, bind a private address and explicitly allow
the LAN prefix. An Internet-facing host keeps the default loopback listener
and has Apache proxy only the desired path over authenticated HTTPS.

Endpoints, all GET/HEAD only:

- `/api/v1/status` — one versioned JSON snapshot containing instance metadata,
  health, tracking, sources, refclocks and NTP listener statistics. It returns
  HTTP 200 whenever the snapshot can be encoded, including when carillon is
  unsynchronised, so a client can display the cause.
- `/healthz` — a small JSON probe. Healthy is HTTP 200; degraded or unhealthy
  is HTTP 503. `synced` is healthy; `holdover` or loss of the preferred source
  is degraded; other discipline states are unhealthy. A snapshot more than
  five seconds old is unhealthy.
- `/metrics` — Prometheus exposition of the same snapshot and counters.

The JSON schema identifier is `carillon.status.v1`. `snapshot_at` is the time
at which the engine published the immutable snapshot; `served_at` is when the
HTTP response was built. Existing control-protocol field names are retained
inside `tracking`, `sources`, `refclocks` and `server`, and durations in those
objects are seconds unless a field name says otherwise. Additive fields are
permitted within v1; incompatible changes require `/api/v2` and a new schema
identifier. Responses use `Cache-Control: no-store` and standard defensive
content headers.

Instance `id`, display `name`, and `roles` are opaque operator metadata for
grouping hosts in clients. They do not affect selection, serving, or daemon
health policy. Role-specific expectations — for example, that a GPS reference
is stratum 1 and PPS-locked, or that a pool host recently served a request —
belong in the monitoring client.

The status and health endpoint proving reachable does not prove that UDP/123
is externally reachable. A client that needs that assertion must make an NTP
query from the observation point as a separate check.

### 10.4 Metrics

The `/metrics` endpoint exports:
`carillon_state`, `carillon_offset_seconds`, `carillon_frequency_ppm`,
`carillon_jitter_seconds`, `carillon_root_dispersion_seconds`, `carillon_stratum`,
`carillon_steps_total`, `carillon_source_offset_seconds{source}`,
`carillon_source_delay_seconds{source}`, `carillon_source_jitter_seconds{source}`,
`carillon_source_root_distance_seconds{source}`, `carillon_source_reach{source}`,
`carillon_source_selected{source}`, `carillon_source_events_total{source,result}`,
`carillon_pps_samples_total{source,result=ok|timeout|spike|gap|glitch}`,
`carillon_pps_jitter_seconds{source}`, `carillon_pps_locked{source}`,
`carillon_server_requests_total{result}`, `carillon_server_last_request_timestamp_seconds`,
`carillon_server_last_served_timestamp_seconds`, `carillon_server_enabled`,
`carillon_source_receive_timestamp_seconds{source}`,
`carillon_source_kernel_timestamp_missing_total{source}`,
`carillon_server_kernel_timestamp_missing_total`, `carillon_leap_pending`, and
`carillon_build_info{version}`. Labels come only from configured source names
or fixed finite enums; operator roles are deliberately not metric labels.

### 10.5 Files

- **Drift file:** one line, frequency in ppm, written atomically (temp +
  rename) every hour and at shutdown, read at start. Same format as chrony's.
- **Statistics** (optional, `[stats] dir`): `loop.tsv` (per update: time, θ,
  freq, ψ, poll, state), `pps.tsv` (per accepted pulse: time, θ), `sources.tsv`.
  A new file per UTC day (`loop.2026-08-23.tsv`); buffered, flushed each
  minute and on exit. Snapshot delivery to the writer is bounded and
  non-blocking: a stalled disk drops and counts statistics snapshots rather
  than delaying the engine. Output errors are rate-limited WARNs and retried;
  they never stop clock discipline. This is what gets plotted when tuning.

---

## 11. Privileges, sandboxing, and init integration

The binary does no privilege dropping; the platform's mechanism grants only
what is needed.

**FreeBSD** (`deploy/freebsd/carillon` rc.d script, `/usr/sbin/daemon -f -p
/var/run/carillon.pid -u carillon`):

```
# /boot/loader.conf
mac_ntpd_load="YES"
security.mac.ntpd.uid=<uid of carillon>     # grants PRIV_ADJTIME, PRIV_CLOCK_SETTIME,
                                         # PRIV_NTP_ADJTIME, PRIV_NETINET_RESERVEDPORT,
                                         # PRIV_NETINET_REUSEPORT to that uid
hw.uart.pps_mode=2                       # DCD capture on all units; per unit at runtime:
                                         # sysctl dev.uart.0.pps_mode=2 (in /etc/sysctl.conf)
# user
pw groupadd carillon; pw useradd carillon -g carillon -G dialer -d /nonexistent -s /usr/sbin/nologin
```

`mac_ntpd` requires `options MAC` (present in GENERIC). Capsicum is a later
hardening item; per-request client sockets (§5.4) would need a pre-opened
socket pool first.

**Linux** (`deploy/systemd/carillon.service`):

```
[Service]
User=carillon
SupplementaryGroups=dialout
AmbientCapabilities=CAP_SYS_TIME CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_SYS_TIME CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/carillon /run
DeviceAllow=/dev/ttyS0 rw
DeviceAllow=char-pps rw
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
```

plus `pps_ldisc` in `/etc/modules-load.d/carillon.conf`. `TIOCSETD` needs write
access to the tty, nothing more, once the module is loaded.

**OpenWrt** (`deploy/openwrt/carillon.init`, procd): runs as root; `respawn`;
config at `/etc/carillon/carillon.toml`; depends on `kmod-pps` and `kmod-pps-ldisc`
or `kmod-pps-gpio`; `[step] panic_at_startup = true` by default.

**All:** `carillon` refuses to start if UDP 123 cannot be bound and says which
daemon is probably holding it. Two disciplining daemons on one host is the
classic "why does my clock wobble" and it must fail loudly, not coexist.

---

## 12. Process model and concurrency

- `main` parses flags, loads config (`-check` exits here), opens the control
  socket, constructs the clock backend (fails fast on privilege errors),
  reads the drift file, then starts the engine.
- **Engine goroutine:** `select` over the measurement channel, a 1 s ticker
  (slew step, holdover accounting, drift-file writes), source-exit
  notifications, and `ctx.Done()`. It is the only caller of the clock actuator and
  the only writer of discipline state. It publishes `Status` by storing a new
  immutable struct into an `atomic.Pointer`.
- **Source goroutines:** one per configured server and per refclock. PPS uses
  a bounded 1.5 s `PPS_FETCH`, which shifts reach on a missing pulse and also
  bounds shutdown latency; a vanished device is reopened with backoff. NMEA
  readers block in `read` and close their fd on shutdown. A `gps` refclock
  uses two threads, which is fine.
- **Server goroutines:** one per listen socket; read `Status` via the atomic
  pointer; never touch engine state. Rate limiter is per socket goroutine
  (no sharing needed: one client hits one socket).
- **Control/monitoring:** a small unix-socket accept loop and standard
  `net/http`; both only read immutable snapshots. Only `waitsync` sends a
  request into the engine and waits on a reply channel.
- **Statistics writer:** receives immutable snapshots through a bounded
  non-blocking queue, owns its buffered daily files, flushes each minute, and
  drains queued snapshots on shutdown. It never calls into the engine.
- **Shutdown:** `SIGTERM`/`SIGINT` cancel the root context; engine writes the
  drift file, leaves the frequency word alone, closes sockets; `main` waits
  with a 5 s deadline.
- **No `SIGHUP` reload** in v1. Restart.

GC and scheduling jitter are not on the accuracy path: PPS and RX timestamps
are taken in the kernel; only TX timestamps and the once-per-second slew
application are user-space-timed, and a few hundred microseconds of lateness
in applying a frequency word is irrelevant at the loop's time constants.

---

## 13. Testing strategy

Everything below runs on the Mac with `go test -race ./...` unless marked.

- **Wire format:** golden packets (hand-assembled from RFC 5905 field
  definitions plus captures of ntpd and chrony replies), round-trip property
  tests, era boundary tests, `FuzzDecode`.
- **CMAC:** RFC 4493 test vectors; a MAC round-trip against a packet with
  extension fields.
- **NMEA:** table tests for RMC/ZDA/GGA, bad checksums, truncated lines,
  multiple talkers, the week-rollover rejection.
- **Selection/cluster/combine:** the RFC 5905 worked examples; falseticker
  scenarios (1 of 3 wrong, 2 of 5 wrong, prefer source wrong); PPS
  qualification/unqualification.
- **Loop / simulation:** `SimClock` models a local clock with configurable
  frequency error, random-walk wander, and white measurement noise; PPS and
  NTP sources are simulated on top of it. Tests assert: convergence to
  `|θ| < 3σ` within a bounded number of updates from a 100 ms initial offset
  and 50 ppm frequency error; steady-state RMS; no step after the startup
  window; correct holdover dispersion growth; step policy edge cases;
  frequency clamp. Deterministic seeds, and a `-sim.plot` flag that writes a
  TSV for eyeballing.
- **Server:** in-process listener on a loopback ephemeral port; verify reply
  fields, version echo, unsynced reply, ACL, rate limit + KoD, MAC required,
  reply length invariant (fuzz: no input produces a longer output).
- **Client source:** a fake server goroutine with scripted replies (wrong
  origin, KoD RATE/DENY, stratum 0, bogus delay) exercising the poller's
  state machine; `testing/synctest` for poll scheduling where Go's version
  permits.
- **Engine:** end-to-end with `clock.Fake` and a fake NTP server: reaches
  SYNCED, steps once at start, never again; snapshot contents.
- **`hwtest` build tag** (`CARILLON_HW_TESTS=1`, run by the user on a target
  host, never by Claude): cgo size/offset checks for the ioctl structs,
  `PPS_GETCAP` on the configured device, one blocking fetch, a read/write of
  the kernel frequency word that restores the previous value.

Acceptance on real hardware is a manual checklist in `deploy/ACCEPTANCE.md`:
`carillonctl refclock` shows locked with σ in the expected band, `carillonctl
tracking` converges, a second host running chrony shows the server within its
expected accuracy, `loop.tsv` plotted over 24 h shows no steps and bounded
frequency wander.

---

## 14. Failure modes and safety

| Failure | Behaviour |
|---|---|
| PPS pulses stop | reach decays over 8 s; PPS unqualified; other survivors take over or HOLDOVER; WARN once |
| PPS present, no numbering source | PPS collected but unused; ERROR once; server unsynced |
| GPS loses fix | NMEA status `V` → sentences ignored; PPS may continue (receivers hold PPS briefly); dispersion grows |
| GPS date rollover / garbage date | rejected by build-date check; ERROR; source unreachable |
| Home unreachable from colo | prefer lost → ERROR once; public survivors used; stratum changes; colo keeps serving |
| Home judged falseticker | same as above, plus the intersection outcome is logged with all intervals |
| Bad/missing MAC from home | replies dropped and counted; home effectively unreachable; ERROR |
| Offset beyond `panic` after startup | no correction; ERROR every update; server unsynced; operator must intervene |
| Offset beyond `panic` at startup, `panic_at_startup=false` | exit 1 with the offset in the message |
| Port 123 busy | exit 1 naming the likely daemon |
| Keys file readable by others | `-check` and startup fail |
| Serial device vanishes (USB unplug) | source goroutine exits with error; engine marks unreachable; reopen retried with backoff |
| Kernel refuses `ntp_adjtime` (privileges) | fail at startup, before any source runs |
| Clock read resolution coarse (VM) | precision reflects it; nothing else changes |

Security posture summary: no modes 6/7, reply never larger than request
(bounded MAC case aside), ACL default-deny, rate limiting with KoD, random
client ports and transmit nonces, origin timestamp check, CMAC-only
authentication, keys file permission check, no dynamic memory growth driven
by attackers (rate-limit table is bounded and LRU).

---

## 15. Milestones

| | Deliverable | Done when |
|---|---|---|
| M0 ✅ 2026-08-23 | Repo skeleton, config, `internal/ntp` wire format + CMAC, `clock.Fake`, discipline package with simulation tests | `go test -race ./...` green on the Mac |
| M1 ✅ 2026-08-23 (initial host acceptance) | NTP client source, engine, Linux + FreeBSD actuators, drift file, `carillonctl tracking/sources` | Fedora and FreeBSD hosts both track and restart from drift; long-duration chrony comparison is now running |
| M2 ✅ 2026-08-23 (real hosts) | Server, ACL, rate limiting, KoD, MAC auth, systemd + rc.d | `gummi` → authenticated `twocom` topology runs end to end without a refclock; see `deploy/ACCEPTANCE.md` |
| M3 ✅ 2026-08-23 (code) | `pps` refclock (FreeBSD uart, Linux ldisc + `/dev/ppsN`), qualification, lock, holdover | kernel API/capability/fetch paths pass on both real hosts; stratum-1 acceptance awaits a live pulse on one of their serial inputs |
| M4 | `gps` refclock (NMEA), leapfile, stats files, read-only JSON/health/Prometheus monitoring, `-check` | home host is stratum 1 with GPS alone and every role is remotely observable |
| M5 | OpenWrt build + procd, hardening (Capsicum socket pool, systemd sandbox), `deploy/ACCEPTANCE.md` | runs on the router |
| later | NTS (RFC 8915) server+client; interleaved mode; `SO_TIMESTAMPING` TX timestamps; slew leap mode; auto `nmea_offset`; regression estimator; FLL branch | as wanted |

---

## 16. Decision log

**D1 — Go, not C or Rust.** Matches every other daemon in `~/Git/daemons`
(TOML config, Prometheus, rc.d via `daemon(8)`), cross-compiles to all three
targets with `CGO_ENABLED=0`, and the accuracy-critical timestamps are taken
in the kernel so the runtime's scheduling jitter is not in the error budget.
Cost: hand-declared ioctl structs (mitigated by `hwtest` cgo checks) and a
~6 MB binary on OpenWrt.

**D2 — User-space discipline; kernel is only an actuator.** Kernel PPS
discipline needs `options PPS_SYNC` (not in FreeBSD GENERIC) or
`CONFIG_NTP_PPS` (not guaranteed on OpenWrt), behaves differently per kernel,
and is opaque to test. Doing filter/select/loop in user space makes behaviour
identical everywhere and lets the whole loop run in a simulation under
`go test`. Slewing via a temporary frequency offset (chrony's "generic"
driver approach) is how the actuator stays to two operations.

**D3 — Colo pulls from home (client/server); no symmetric/push mode.**
Symmetric mode (1/2) is the NAT-friendly alternative but its mutual-sync
semantics are a well-known source of confusion and historical
vulnerabilities, and it would still need authentication. A WireGuard tunnel
or a source-restricted port-forward solves the reachability problem with no
new protocol surface.

**D4 — AES-128-CMAC only; no MD5 MAC; NTS later.** The only authenticated
association is between two copies of this daemon. RFC 8573 deprecates MD5;
ntpd and chrony both speak `AES128CMAC`, so interop is not lost. NTS is the
right answer for authenticating public clients and is a self-contained later
milestone.

**D5 — Modes 6/7 never.** Every NTP amplification attack has gone through
them. Control is a local unix socket. Remote observability is a separately
configured read-only HTTP listener offering versioned JSON, health and
Prometheus endpoints; it has no mutation handlers and defaults to a loopback
ACL.

**D6 — Server disabled unless `allow` is set.** chrony's default; a time
daemon that answers the internet by accident is worse than one that needs a
line of config.

**D7 — Two refclock types.** `pps` (the core interest) and `gps` (PPS + NMEA
on one port). Everything else — SHM, PTP, other receivers' binary protocols —
is out of scope. A future refclock would be a third type, not a framework.

**D8 — ntpd's loop structure, critically damped gains.** The filter →
select → PLL structure and the τ-per-poll scheme are RFC 5905's and ntpd's.
The gains are not: the first implementation used ntpd's (τ = 16·P, integral
gain 1/(16τ²)) and the simulation showed the expected ~15τ slow mode — hours
at poll 6 with no FLL to rescue longer polls — plus integrator windup during
a saturated slew. The loop is now critically damped (τ = 4·P, 1/(4τ²)) with
an anti-windup guard (§6.4): minutes to converge with a drift file, one to
two hours from nothing, 27 µs RMS on a 200 µs-noise simulated path. The
estimator stays behind `Loop.Update/Tick` so a regression estimator
(chrony-style) can still replace it.

**D9 — Second numbering from any qualified source, ntpd-style.** Using the
nearest whole second of the local clock (once within ±0.4 s) is simpler and
more robust than pairing individual NMEA sentences with individual pulses,
and it is what lets a bare PPS work with only an NTP server for numbering.

**D10 — ntpd `ntp.keys` file syntax.** One key file format the user already
knows, shareable with chrony/ntpd hosts on the LAN if ever needed.

**D11 — No `SIGHUP` reload.** Restart costs one drift-file read; a reload
path that has to tear down serial devices and line disciplines correctly is
code that would rarely run and rarely be tested.

**D12 — Hostnames are re-resolved only on failure.** ntpd and chrony both
keep a resolved address until the server stops answering, and the first soak
showed why: periodic re-resolution of a pool name is a server change every
interval, which turns the per-source clock filter into a blend of unrelated
servers. Failure-only re-resolution (8 consecutive timeouts) still follows a
server that moves, and a changed address resets the filter (§5.4).

---

## 17. References

- RFC 5905 — NTPv4 protocol and algorithms (filter §10, selection §11.2,
  discipline §11.3, on-wire §8, KoD §7.4)
- RFC 2783 — Pulse-Per-Second API for UNIX-like systems
- RFC 8573 — Message Authentication Code for NTP (AES-CMAC)
- RFC 4493 — AES-CMAC algorithm and test vectors
- RFC 7822 — NTP extension field framing
- RFC 8633 — NTP Best Current Practices
- RFC 9109 — NTP port randomisation
- RFC 8915 — Network Time Security (later)
- FreeBSD: `uart(4)` (`pps_mode`), `ucom(4)`, `mac_ntpd(4)`, `ntp_adjtime(2)`,
  `timepps(3)`/`<sys/timepps.h>`, `daemon(8)`
- Linux: `Documentation/driver-api/pps.rst`, `<linux/pps.h>`,
  `<linux/timex.h>` (`adjtimex`, `ADJ_SETOFFSET`), `ldattach(8)`,
  `socket(7)` (`SO_TIMESTAMPNS`, `SO_TIMESTAMPING`)
- Mills, *Computer Network Time Synchronization*, 2nd ed. — the PLL/FLL
  derivation behind §6.4
- chrony `sys_generic.c` and `sources.c` — reference for frequency-offset
  slewing and for the regression estimator considered in D8
