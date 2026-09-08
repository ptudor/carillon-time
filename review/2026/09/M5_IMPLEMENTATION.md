# M5 implementation and verification — 2026-09-08

The design baseline is `735c84baa9e416792322290e63a6a7546ff06e7e`.
M5 is implemented in code. Live GPS/PPS and real kernel leap-event acceptance
remain pending. The specification is [leap distribution](../../../docs/leap-distribution.md)
and [DESIGN §6.8](../../../DESIGN.md#68-leap-seconds).

## Result

- Manual imports, a fixed NIST HTTPS seed and explicitly trusted CMAC peers
  all produce immutable objects containing the original file bytes. Relays
  redistribute those bytes through the experimental CLPS NTPv4 extension.
  Metadata dates are unsigned 64-bit whole seconds since 1900; ordinary NTP
  header timestamps retain 32.32 encoding.
- A private cache couples the active/pending objects, provenance, accepted
  dates, execution record and greatest established UTC. File sync, atomic
  rename and directory sync precede activation. Startup reloads under the
  cache lock, rehashes and reparses the objects, and reacquires clock evidence.
- The engine approves and rechecks each committed generation. Acquisition
  continues while missing or expired required leap data withholds kernel,
  NTP and `waitsync` synchronization. NMEA and PPS cannot vote that no leap
  is pending; optional client fallback needs fresh leap-capable survivor LI.
- Independent armed/executed state handles an insertion's repeated final
  second and a deletion's skipped second. Table renewal cannot cancel an
  armed event or execute an old event retroactively.
- Tracking, health, JSON and fixed-label metrics expose readiness, provenance,
  dates, digest, acquisition failures and rejected/pending candidates. An
  unchanged valid seed cache retains its successful-check schedule on restart.

RA6X-023, expired-file authority, is closed by implementation and regression
coverage. This does not close the separate hardware acceptance items.

## Automated checks

All checks below passed. Production builds used `CGO_ENABLED=0`. Race tests
used `CGO_ENABLED=1` and the module's Go 1.27.1 toolchain. Caches and build
outputs were placed in session scratch directories.

| Check | Result |
|---|---|
| macOS `go test ./...`, `go vet ./...`, `go test -race ./...` | Passed |
| FreeBSD 15.0-RELEASE-p12/amd64 on `twocom`: vet and uncached full race suite | 19 test packages passed; 998 test/subtest executions; no failing tests |
| Fedora 43/Linux 6.19.14-200.fc43.x86_64 on `gummi`: vet and uncached full race suite | 19 test packages passed; 1,002 test/subtest executions; no failing tests |
| Pure-Go builds of both binaries | FreeBSD amd64/arm64, Linux amd64/arm64 and Darwin arm64 passed |
| Native `-tags abicheck -run Layout` on both hosts | PPS layouts passed on both; hand-declared `timex` layout passed on FreeBSD. Linux uses `x/sys/unix.Timex` and has no separate clock layout test. |
| CLPS fuzzing, 20 seconds, four workers | 101,884 executions, no failure |
| `scripts/check-configs.py` against the built daemon | Deployment example and packaged client configuration passed offline |
| Manual `-check` with the NIST file retrieved during design work | Passed offline; `#$ 3676924800`, `#@ 4007404800`, 10,720 original bytes |
| `git diff --check` and Go formatting | Passed |

The final native test events were inspected, including individual test starts
and results. Their source archive SHA-256 was
`fc944f183df89c25ad2d7a47067b3cf39374073d214ac55572cce081b46b2aa0`.
That archive contains the final Go implementation; this verification document
and subsequent wording clarifications were added afterwards. The original NIST
file SHA-256 was
`cbf4e71f2d46869532a52dd501139cb925231472f1707fee088193ea38d85f4d`.

The native full-suite commands, after entering the disposable source copy and
setting its `GOCACHE` and `GOMODCACHE`, were:

```sh
CGO_ENABLED=0 go vet ./...
# Linux:
CGO_ENABLED=1 go test -p 4 -race -count=1 -json ./...
# FreeBSD:
CGO_ENABLED=1 proccontrol -m aslr -s disable go test -p 4 -race -count=1 -json ./...
# Safe header comparisons, both platforms:
CGO_ENABLED=1 go test -p 4 -tags abicheck -run Layout ./internal/pps ./internal/clock
```

FreeBSD's initial race invocation exited successfully without executing tests:
the sanitizer printed `This sanitizer is not compatible with enabled ASLR and binaries compiled with PIE`.
Those apparent passes were discarded. The rerun used
[`proccontrol`](https://man.freebsd.org/cgi/man.cgi?query=proccontrol) for the
test process and its children only; it did not change system-wide ASLR or any
running daemon. Both final native JSON logs contain real test executions.

Those native checks also exposed two pre-existing FreeBSD issues, fixed here:

- A specifically bound IPv4 listener supplied `IP_SENDSRCADDR`, making
  `sendmsg` fail with `EINVAL`. The bound address now supplies the reply source;
  wildcard listeners retain the ancillary address. Both paths have loopback
  regression coverage. See the source-selection rules in
  [FreeBSD ip(4)](https://man.freebsd.org/cgi/man.cgi?ip=).
- The public-server warning test assumed Linux's warning count and omitted
  FreeBSD's documented receive-buffer warning. It now checks that warning.

## Coverage to inspect during review

| Area | Regression coverage |
|---|---|
| Identity and parser | `internal/leap/object_test.go`: NIST expiry-only renewal; date/size limits including 2036 and 2038; immutable copies; rollback, equal-date conflict and history preservation |
| Wire and authentication | `object_test.go`, `transfer_test.go`, `internal/server/leap_test.go`: independent golden vector, full packet MAC tampering, strict field/padding/chunk validation, replay/reflection/correlation, authorization and response-size bound |
| Acquisition | `transfer_test.go`, `lifecycle_test.go`: test-HTTPS seed → UDP relay → leaf, denied downstream HTTPS, identical original bytes, renewal, restart, forged digest, changed object, TLS/redirect/encoding/size failures, legacy responses and KoD/backoff |
| Durable activation | `object_test.go`, `transfer_test.go`: injected create/write/file-sync/rename/directory-sync failures, restart recovery, commit/activation races and retained active data; `lifecycle_test.go`: symlink and corrupt-cache refusal |
| UTC and leap execution | `internal/engine/leap_test.go`: disconnected GPS/PPS insertion and deletion with a fake clock, exactly one filter reset, service continuity, pending-event conflicts, missed events, expiry and backward clocks/restart |
| Runtime ownership | Engine tests exercise the real bounded request queue and a running updater; settling UTC can export an eligible table with LI=3 without arming the kernel early |
| Fresh LI and readiness | `internal/discipline/leap_test.go` and engine tests: NMEA/PPS exclusion, latest accepted LI independent of the filter winner, fresh majority versus stale/tied evidence, optional-client fallback |
| Resource bounds | Wire limits, 64 KiB object bound, one acquisition job, deadline/cancellation, fixed global/per-key budgets shared across listeners, ordinary NTP unaffected by the CLPS budget |

## Operational limits

Refclock and serving configurations must choose manual, NIST or trusted-peer
acquisition before upgrading. An empty automatic cache is valid configuration;
synchronized service waits for UTC and a committed current table. Existing
ordinary network client configurations still work. Offline GPS/PPS operation
is covered only through the cached table's actual expiry. Trust and key changes
still require a restart; accepted data updates do not.

The CMAC key authenticates the immediate configured distributor. Relays provide
no independently verifiable NIST origin signature, and a compromised authorized
distributor can still propose plausible false future data. Rollback/history
checks cannot remove that trust assumption. Rejections preserve diagnostics,
not an unbounded archive of rejected file bytes. CLPS type `0xF504` remains an
experimental protocol identifier.

All runtime tests used fake clock actuators and temporary source/state files.
Native ABI checks read system headers. No privileged hardware tests, production
clock changes, service restarts or deployment changes were performed. Live
receiver behavior, actual kernel insertion/deletion and sustained accuracy must
still be accepted on hardware by the operator.
