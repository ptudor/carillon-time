# M5 leap-authority review (Fable 5.1)

Review date: 2026-09-09. Scope: `735c84b..3413e23`, the single commit
"Implement durable leap authority and authenticated NTP distribution"
(47 files, +4,087/−269). Compared against `DESIGN.md` §6.8 and §8.5,
`docs/leap-distribution.md`, and the claims in
`review/2026/09/M5_IMPLEMENTATION.md`. Review only: no production source was
changed, no daemon was run against a real clock, no host was contacted. The
only repository changes are this report, the probe overlay in
`m5-review-repro/`, and a pointer in `CLAUDE.md`.

There are **8 findings: 1 High, 3 Medium, 3 Low, 1 Informational**. The High
finding is a policy conflict between the implementation and the design's
holdover and network-only client rows; it needs a maintainer decision, and the
existing engine test encodes the implemented behaviour as intended. Nothing
found breaks the disconnected GPS/PPS path, the leap execution logic, the
persistence coupling, or the CLPS authentication; those hold up under the
existing tests and the reading below.

## Method and verification

Every file in the diff was read, plus the surrounding engine, discipline,
server, source and control code the leap paths call into. Data flows traced:
NIST HTTPS / manual file / CLPS chunks → `leap.Object` → updater approval,
durable commit, engine recheck and activation → `applyLeap` readiness →
kernel status, NTP responder, control socket, health and metrics; and the
reverse export path `Engine.CurrentLeap` → `leap.Distributor` → responder.

| Check | Result |
|---|---|
| `CGO_ENABLED=0 go build ./...`, `go vet ./...` (Go 1.27.1 darwin/arm64, module caches in the session scratchpad) | Pass |
| `CGO_ENABLED=1 go test -race -count=1 ./...` | Pass, 17 packages with tests |
| `gofmt -l .`, `git diff --check 735c84b..3413e23` | Clean |
| `go test -fuzz=FuzzCLPS -fuzztime=10s -parallel 4 ./internal/leap/` | 134,955 executions, no failure |
| `scripts/check-configs.py` against a scratch-built daemon | Both shipped configurations pass (`-check` offline) |
| `m5-review-repro/run_probes.py -race` (this review's overlay probes) | 3 probes fail as designed; see RM5-001, RM5-002 |

Not verified here, by policy (no SSH, no outbound fetch during a review):
the FreeBSD 15 and Fedora 43 native race runs, the `abicheck` layout tests,
the `IP_SENDSRCADDR` behaviour on FreeBSD, the live NIST file digest, and the
source archive SHA-256 quoted in `M5_IMPLEMENTATION.md`. Those remain the
maintainer's target-host evidence.

## Findings

### RM5-001 — Plain network clients lose kernel synchronization whenever fresh survivor LI is unavailable

**Severity: High** (policy decision required). **Reproduced** by
`m5-review-repro/engine_test.go.txt`, both probes.

`applyLeap` (`internal/engine/leap.go:88-113`) grants readiness to a host
without a table only when `st.Leap` is not `LeapUnsync`, and otherwise demotes
a SYNCED or HOLDOVER discipline to `StateUnsynced`, stratum 16, refid `XLEP`
and LI=3. `syncKernel` (`internal/engine/engine.go:1107-1125`) then sets
`STA_UNSYNC`. `st.Leap` comes from `majorityLeap`
(`internal/discipline/select.go:645-664`), which only counts a source whose
last accepted LI is at most two poll intervals old, and returns `LeapUnsync`
for no voters or no strict majority.

The discipline itself keeps a source as a survivor for eight poll intervals
(`freshnessDeadline`, `select.go:241-253`) and then coasts in HOLDOVER for
`holdover_max` (3600 s, `system.go:538-545`). So for a `require_table = false`
host with no table — the configuration `docs/leap-distribution.md:337` says
"needs no new leap settings" — the effective state diverges from the
discipline for most of the interval the discipline considers usable:

| Situation (one upstream, poll 6) | `clock_state` | Effective `state` / refid | Kernel |
|---|---|---|---|
| 129 s after the last reply (two lost polls) | synced | unsynced / `XLEP` | `STA_UNSYNC` |
| 529 s after the last reply (holdover) | holdover | unsynced / `XLEP` | `STA_UNSYNC` |
| One reply later | synced | synced | synchronized |
| Two upstreams, one announces the final-day leap and one does not | synced | unsynced / `XLEP` for the whole day | `STA_UNSYNC` |

Probe output (`run_probes.py -race ./internal/engine`):

```
after two lost polls: clock_state=synced state=unsynced stratum=16 refid=XLEP leap=unsynchronized leap_ready=false reason="missing table" kernel_synced=false
holdover:             clock_state=holdover state=unsynced stratum=16 refid=XLEP leap=unsynchronized leap_ready=false reason="missing table" kernel_synced=false
split vote:           clock_state=synced state=unsynced stratum=16 refid=XLEP leap=unsynchronized leap_ready=false reason="missing table" kernel_synced=false
```

This conflicts with the design in three places. `DESIGN.md:922-926` says a
host in HOLDOVER holds frequency and keeps serving with growing dispersion
until `holdover_max`, and `system.go:379` states that HOLDOVER "is served as
synchronized by both the wire and the kernel". The §6.8 table
(`DESIGN.md:1058-1059`) prescribes `STA_UNSYNC` and LI=3/stratum 16 only for
the refclock/serving row; the network-only row says "otherwise leap knowledge
is unknown", which is a status, not a synchronization state. And
`docs/leap-distribution.md:337` promises that a plain client needs no new
settings, yet its kernel synchronization now depends on two-poll-fresh LI.

Impact: `navlisten2026`, the deployed client-only host, will flap its kernel
`STA_UNSYNC` bit, `carillonctl waitsync`, and health on ordinary packet loss
after the upgrade, and will report unsynchronized during every network outage
instead of holding over. `TestExpiredTableFallsBackOnlyToFreshNetworkEvidence`
(`internal/engine/leap_test.go:315-348`) asserts the demotion at 129 s, so the
implementer treated it as intended; the pre-M5 daemon served LI=0 with the
kernel synchronized in the same situations.

Recommendation, in order of preference: for `LeapRequired = false`, represent
unknown leap knowledge as LI=3 in the published status and on the wire while
leaving `State`, stratum, the kernel synchronized bit and the leap flags as the
discipline computed them; that is what ntpd and chrony do with an absent
warning. Alternatively, amend `DESIGN.md` §6.5 and the §6.8 table to say
explicitly that a network-only client is unsynchronized whenever survivor LI
is unknown, and document the two-poll window and its interaction with
`holdover_max` in `docs/leap-distribution.md`. Either way the engine test
should cover the lost-poll and holdover cases for a plain client, which it does
not today.

### RM5-002 — The NIST seed fetch honours `HTTPS_PROXY` from the process environment

**Severity: Medium.** **Reproduced** by
`m5-review-repro/leap_test.go.txt`.

`FetchNIST` (`internal/leap/fetch.go:21-27`) clones `http.DefaultTransport`,
whose `Proxy` is `http.ProxyFromEnvironment`. With `HTTPS_PROXY` set in the
daemon's environment the fixed-URL download is routed through that proxy:

```
FetchNIST: leap: NIST fetch: Get "https://tf.nist.gov/leap-seconds.list": proxyconnect tcp: dial tcp 127.0.0.1:1: connect: connection refused
```

TLS is still negotiated end-to-end through CONNECT, so a proxy cannot alter
the bytes; the exposure is that the download path can be steered by an
environment variable that appears nowhere in the TOML, contrary to the
function's own contract (`fetch.go:19-20`, "Neither a peer nor configuration
can replace the URL or TLS policy"), to `docs/leap-distribution.md:109`
("HTTP downgrade and TLS verification bypass are prohibited"), and to the
project's no-environment-variables rule.
A wrong or dead proxy in a unit's `Environment=` produces silent "io"
rejections and backoff rather than a configuration error. Fix: set
`transport.Proxy = nil` on the cloned transport, and add a test that the
fetch ignores `HTTPS_PROXY`.

### RM5-003 — A transient probe timeout parks a peer for 24 hours

**Severity: Medium.** Static trace; the existing
`TestProbeTimeoutIsBoundedAndCancels` (`internal/leap/transfer_test.go:404-421`)
asserts the 24-hour value for a timeout.

`peerClient.exchange` (`internal/leap/fetch.go:89-169`) returns
`PeerError{RetryAfter: 24h}` after three unanswered PROBE attempts, about
16 s in total, and `Updater.Run` (`internal/leap/updater.go:327-335`) applies
`max(interval, RetryAfter)` and a positive-only jitter, so the peer's next
probe is at least 24 hours away. The specification reserves the 24-hour floor
for a peer that lacks the capability (`docs/leap-distribution.md:224`) and
gives errors a 15-minute to six-hour backoff (`:113`), but a dropped datagram
during a WAN blip or a distributor restart is indistinguishable from
"unsupported" on this path.

On the documented home → colo topology with one `leap_trust` peer, a learner
that boots with an empty cache during such a blip withholds synchronized
service (kernel unsynchronized, LI=3, stratum 16, `waitsync` false) for a day
unless an operator restarts it. With a valid cache the effect is only a
delayed refresh. Recommendation: treat a timeout as an ordinary error with the
15-minute backoff, and reserve the 24-hour floor for an authenticated reply
that carries no CLPS field or an unsupported version, which is unambiguous
evidence of a legacy peer.

### RM5-004 — No operator recovery path for the durable acceptance record

**Severity: Medium** (operations, documentation). Static.

`docs/leap-distribution.md:267-269` promises that "an operator can explicitly
replace trust settings and locally reset the acceptance record with an audited
action", but neither `carillon`, `carillonctl`, `-check`, `deploy/README.md`
nor the specification describes one. The only recovery is stopping the daemon
and deleting `leap/state.json`, which also discards the rollback anchor, the
execution record and the NIST check schedule. Situations that require it:

- **Forward-wrong clock.** `applyLeap` advances `utcBound` whenever the
  discipline is synced or coarse-settled (`internal/engine/leap.go:30-39`),
  and the updater persists it every minute. A falseticker or a wrong RTC that
  the step policy accepts (up to `panic` = 1000 s by default, unbounded with
  `panic_at_startup`) writes a future bound; after correction every table is
  "expired" or "UTC not established" until the real clock passes the bound,
  and a required-table host withholds service for that long. The behaviour is
  what the design specifies for setbacks, but its trigger and duration are
  not documented.
- **Conflict or armed rejection.** `Updater.reject`
  (`internal/leap/updater.go:77-95`) keeps `LastRejection` until the next
  activation, so a manual file edited without changing its dates keeps health
  degraded (`leap_conflict`) and logs a warning every hour indefinitely.
- **Poisoned pending generation.** A committed pending object whose activation
  keeps failing is retried every 15 minutes (`updater.go:239-244`) and remains
  the rollback anchor for new candidates.

Recommendation: document the stop-daemon, remove-`state.json`, restart
procedure and its consequences in `deploy/README.md` and
`docs/leap-distribution.md`, and consider an offline `carillon leap reset`
subcommand that takes the cache lock, logs what it discards, and can clear
only the UTC bound or only the pending generation.

### RM5-005 — A learner's RATE spacing never decays, and the default KoD poll makes a real file untransferable

**Severity: Low.** Static.

`checkReply` (`internal/leap/fetch.go:179-191`) turns a RATE kiss into
`MinInterval = 2^poll`; the responder sends `defaultMinPoll = 6`
(`internal/server/responder.go:20`, `:424`), so the learner's spacing becomes
64 s. `Peer.fetch` (`fetch.go:240-243`) refuses any transfer whose chunk count
times spacing reaches 30 minutes: the current NIST file is 10,720 bytes, 42
chunks, 44.8 minutes. `Updater.Run` (`updater.go:330`) only ever raises
`s.spacing`, so after a single RATE from a Carillon distributor that learner
reports a rate-policy conflict on every attempt until restart. The default
address limiter (8 pps, burst 16) makes this unlikely, but the "tuned for a
public server" example uses 0.25 pps, exactly the learner's own request rate.
Recommendation: reset the spacing after a successful exchange, or exempt
authenticated CLPS traffic from the address limiter since it already has the
per-key budget.

### RM5-006 — Status reports the table's leap indicator while the discipline is SETTLING

**Severity: Low** (observability only). Static.

`System.Status` sets LI=3 for SETTLING and UNSYNCED; `applyLeap`
(`internal/engine/leap.go:71-87`) then overwrites `st.Leap` with the table
indicator whenever the table is valid and UTC is coarse-known, regardless of
`st.State`. The kernel and the wire are unaffected because `syncKernel` and the
server status function derive LI from the synchronized flag, but
`carillonctl tracking`, the JSON status and `carillon_leap_pending` show
`insert` or `none` next to `state: settling`. Pre-M5 they showed LI=3. Apply
the indicator only when `synced`, or keep the override but report it under a
separate `table_leap` field.

### RM5-007 — Coverage gaps

**Severity: Low.** The suite is broad for the wire, store and engine leap
paths; the following are the untested edges found while tracing.

- **Engine, plain client.** No test of a `LeapRequired = false` host through
  lost polls, holdover, or a split vote (RM5-001). The only fallback test
  uses an expired table.
- **Updater scheduling.** `Updater.Run`'s peer order, the 24-hour
  unsupported floor (RM5-003), RATE spacing retention (RM5-005), DENY/RSTR
  stop, `nextActivation` retry and the 15-minute cache-failure retry are only
  reachable through `Run` and have no tests; `exchange` and `install` are
  tested in isolation.
- **Monitor, control, carillonctl.** No tests for `leap_table_unavailable`,
  `leap_conflict`, `leap_source_disagreement`, the `LeapValid` derivation,
  `carillon_leap_events_total`, or the new tracking lines.
- **Store validation.** `loadState` rejects a bad provider kind, a pending
  record inconsistent with the active one, acceptance or execution after the
  UTC bound, an oversize file and permissive modes; only the symlink and
  unknown-field cases are tested.
- **`CheckUpdate` bounds.** The 400-day expiry bound, a changed TAI-UTC
  baseline, and `now` before the baseline are untested.
- **Boundary detector timing.** No test of a delayed engine tick (more than
  one second between `crossLeap` calls) across an insertion, where the
  repeated-second detector cannot fire and the forward crossing resets
  instead; the design accepts a spanning observation there, which should be
  pinned.
- **Manual mode.** No test for a replacement with unchanged dates and
  different bytes (conflict, degraded health) or for a temporarily missing
  file, and no `-check` test with an existing cache or a `#$`-less file,
  which the daemon now refuses at startup.
- **Peers integration.** The only updater + engine + store integration is
  manual mode (`TestWorkerActivatesTableIntoRunningGPSClock`); no test runs a
  UDP relay against a live engine.

### RM5-008 — Per-minute full-state rewrite and no save at shutdown

**Severity: Informational.**

The UTC checkpoint (`internal/leap/updater.go:227-238`) marks the state dirty
once a minute and `Store.Save` (`internal/leap/store.go:262-329`) rewrites the
whole `state.json`, including both base64 objects, with a file fsync, rename
and directory fsync: about 1,440 rewrites of 15–175 KiB per day, on the
updater goroutine. `Run` also returns on cancellation without a final save, so
the bound and `last_seed_check` can be up to a minute stale after a clean
stop. Both are within the specification (`docs/leap-distribution.md:329`) and
the engine tolerates a one-second backward bound, but a separate small
checkpoint file or a longer interval would suit flash media better.

## Verified against the design and the implementation notes

| Area | Result |
|---|---|
| Disconnected GPS/NMEA/PPS, positive and negative leap | `TestDisconnectedGPSPPSPositiveAndNegativeLeap` models only the kernel's repeat or skip on a fake clock: one filter reset per source, continuous service, kernel flags armed then cleared, no daemon step, correct post-event UTC, execution record persisted. The repeated-second detector (`engine.go:944-966`) matches both kernels, which decrement the seconds counter at midnight so 23:59:59 repeats. |
| UTC bootstrap | Coarse settling within the 0.4 s guard establishes UTC without arming the kernel (`TestSettlingUTCMayExportWithoutArmingKernel`); NIST fetch and peer probes wait for `LeapView().Known`; the startup bound blocks a clock behind the saved bound. |
| Expiry across setbacks and restarts | `validAt = max(wall, utcBound)` in `applyLeap`; the bound never decreases, is persisted, and reloads as `startupUTCbound`. `TestRequiredTableMissingExpiredAndClockSetback` covers setback and restart. RA6X-023 is closed. |
| Crash-safe activation | Temp file, fsync, rename, directory fsync; pending retained beside active until the engine acknowledges the exact generation; every stage fault-injected in `TestStoreCrashPointsAndRestart`; activation rechecked after commit in `TestCommitFailureAndActivationRecheck` and `TestLeapActivationArmedConflictAndExpiryRace`. |
| Rollback, history, expiry-only renewal | `CheckUpdate` (`object.go:135-185`) rejects either date decreasing, equal dates with different bytes, a changed baseline, changed records on an expiry-only renewal, and any change to transitions already effective; an armed event cannot be removed (`checkLeap`). NIST-style renewal with unchanged `#$` propagates through the seed → relay → leaf chain. |
| CMAC authorization | Server: verified key must be in `serve.leap_keys` (`Distributor.Authorized`), a time-only key or unsigned request gets an ordinary reply, MAC covers header and extension. Learner: `leap_trust` requires a nonzero key, every reply must verify before any state changes. |
| Replay and correlation | Fresh 128-bit ID and 64-bit origin nonce per attempt; reply must echo both plus the requested manifest and offset; every byte of an authenticated reply is tamper-tested; reflection and prior-exchange replay rejected. Server is stateless. |
| Transfer bounds and amplification | One in-flight job, 30-minute deadline, 64 KiB assembly, 4 s spacing, three attempts per operation; `ntp.Decode` handles 156- and 412-byte datagrams; every reply is no longer than its request (`TestWireGoldenAndBounds`, `TestLeapExportAuthorizationAndAmplification`). The golden vector at `object_test.go:112-124` is hand-built with post-2036 dates. |
| NIST policy | HTTPS only, redirects refused, identity encoding, 64 KiB and 30 s caps, system trust store; an unchanged cache keeps its 24-hour check schedule across restart. See RM5-002 for the proxy exception. |
| Ordinary NTP compatibility, server side | Unauthenticated, unauthorized and legacy requests take the ordinary path; a CLPS budget refusal does not affect time replies. See RM5-001 for the client side. |
| `M5_IMPLEMENTATION.md` claims | macOS build, vet, race, formatting, diff check, fuzzing and `check-configs.py` reproduced here. Native FreeBSD/Linux runs, `abicheck`, `IP_SENDSRCADDR` and the NIST file digest not reproducible in this session. The `IP_SENDSRCADDR` change (`listener.go:316-322`) reads correctly and has a wildcard-and-specific loopback test. |

## Suggested order

1. Decide RM5-001 and either change `applyLeap` for `LeapRequired = false`
   or amend the design; add the plain-client tests either way.
2. RM5-002 (`transport.Proxy = nil`) and RM5-003 (timeout is an error, not
   "unsupported"); both are a few lines with an obvious test.
3. RM5-004 documentation, then RM5-005, RM5-006 and the RM5-007 tests as
   ordinary follow-ups.
