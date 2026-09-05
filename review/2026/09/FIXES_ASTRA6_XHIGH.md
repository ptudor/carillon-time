# carillon — fixes applied from REVIEW_ASTRA6_XHIGH.md

Source review: `review/2026/09/REVIEW_ASTRA6_XHIGH.md` (Astra6, xhigh, against
commit `8060697`). Work follows the review's "Suggested fix order and
dependencies" table, wave by wave.

One entry per finding: ID, what changed, files touched, verification result.
A finding whose fix specification is ambiguous or contradicted by the code is
logged as SKIPPED with the reason rather than guessed at.

Verification runs on this darwin/arm64 development host with Go 1.27.0 and
`CGO_ENABLED=1 go test -race`. Anything needing a Linux or FreeBSD kernel, a
real serial device, a live PPS edge, or a privileged clock is cross-compiled
and type-checked here and marked **deferred to the target hosts** — per
CLAUDE.md hard rule 1, Claude never runs those.

---

## Wave 0 — enable native verification alongside the fixes

## RA6X-054 — ABI verification tests cannot be built because they import C in test files — FIXED

**Changed.** Go rejects `import "C"` inside a `_test.go` file, so the three
hand-written-ABI comparisons never compiled, let alone ran. The C side moved
into ordinary (non-test) files that are still excluded from every production
build by their tags, and each test file is now plain Go that iterates over the
facts the C file collected:

- `internal/pps/abi_linux_cgo.go` builds `[]cLayout` from `<linux/pps.h>` —
  sizes and offsets for `pps_ktime`, `pps_kinfo`, `pps_fdata`, `pps_kparams`,
  the hand-declared `PPS_API_VERS_1`/`PPS_CAPTURE*`/`PPS_TSFMT_TSPEC`/
  `PPS_TIME_INVALID` constants, and the four `PPS_*` ioctl numbers carillon
  takes from `x/sys` (a mismatch there is just as fatal as one in our own
  constants, so they are compared too). `internal/pps/abi_linux_test.go`
  compares them.
- `internal/pps/abi_freebsd_cgo.go` does the same against
  `<sys/timepps.h>` for `pps_info_t`, `pps_params_t`, `struct pps_fetch_args`
  and the five hand-declared `PPS_IOC_*` numbers, with
  `internal/pps/abi_freebsd_test.go` comparing.
- `internal/clock/abi_freebsd_cgo.go` collects the `struct timex` size, all
  seventeen field offsets, and the fifteen `MOD_*`/`STA_*` constants from
  `<sys/timex.h>`; `internal/clock/abi_freebsd_test.go` compares.

The tag is now `abicheck`, deliberately **not** `hwtest`: reading header
offsets opens no device and touches no clock, so this safe check no longer
requires enabling the tests that do mutate hardware. `make abicheck` runs it.
Coverage is slightly wider than the deleted files (they omitted
`pps_kinfo.clear_sequence`, `pps_kparams.mode`, `pps_fetch_args.tsformat` and
every non-ioctl constant). Production and cross builds stay `CGO_ENABLED=0`.

**Files.** `internal/pps/abi_linux_cgo.go` (new),
`internal/pps/abi_linux_test.go` (new), `internal/pps/abi_freebsd_cgo.go`
(new), `internal/pps/abi_freebsd_test.go` (new),
`internal/clock/abi_freebsd_cgo.go` (new),
`internal/clock/abi_freebsd_test.go` (new),
`internal/pps/pps_linux_cgo_test.go` (deleted),
`internal/pps/pps_freebsd_cgo_test.go` (deleted),
`internal/clock/timex_freebsd_cgo_test.go` (deleted), `Makefile`.

**Verification.** The review's reproduction was `go list -test` with cgo
failing on `use of cgo in test <file> not supported`. All twelve
(GOOS, GOARCH, package) combinations the review names now load cleanly under
both `-tags hwtest` and `-tags abicheck`:

```
GOOS={linux,freebsd} GOARCH={amd64,arm64} CGO_ENABLED=1 \
  go list -test -tags abicheck ./internal/pps ./internal/clock   → OK (was: rejected)
```

`CGO_ENABLED=1 go test -race ./...` passes natively; all four production
cross-builds still report `CGO_ENABLED=0` under `go version -m`. **Deferred to
the target hosts:** actually compiling the C against real `<linux/pps.h>` and
`<sys/timepps.h>`/`<sys/timex.h>` and comparing the layouts needs native
Linux and FreeBSD — `make abicheck` there.

## RA6X-055 — The documented race-test Make target fails on deployment platforms — FIXED

**Changed.** The Makefile's global `export CGO_ENABLED = 0` reached the `test`
recipe, whose `-race` needs the cgo-backed race runtime; on Linux and FreeBSD
`make test` therefore died with `go: -race requires cgo` before running a
single test. The global export is gone, replaced by two explicit variables:
`BUILD_ENV := CGO_ENABLED=0` applied to `build`, `vet` and the `cross`
template, and `TEST_ENV := CGO_ENABLED=1` applied to `test`. `-race` was not
dropped. The new `abicheck` target (RA6X-054) sets `CGO_ENABLED=1` itself.
Comments now state that native race testing needs a C compiler and that
cross-compiled race testing is unsupported — run `make test` on each host.

**Files.** `Makefile`.

**Verification.** Baseline reproduced: `GOOS=linux GOARCH=amd64
CGO_ENABLED=0 go test -race ./internal/discipline` → `go: -race requires cgo`.
With cgo enabled that error is gone and the build proceeds to the C toolchain
(which then fails only for want of a *cross* C compiler on this Mac, exactly
as the fix documents). `make test GO=/opt/local/bin/go` passes natively; `make
dist` produces all four binaries and `go version -m` reports `CGO_ENABLED=0`
for each. **Deferred to the target hosts:** `make test` on Linux and FreeBSD
with their native compilers.

## RA6X-056 — The serial EOF regression asserts the wrong kernel event path — FIXED

**Changed.** Closing a pipe's write end makes `poll(2)` report `POLLHUP`,
which `ReadTimeout` correctly classifies as a device error *before* it calls
`read(2)` — so the old test's `errors.Is(err, io.EOF)` assertion failed on
Linux and FreeBSD while never reaching the zero-length-read branch it was
written to protect. Production classification is unchanged. Instead, `Port`
gained a two-function syscall seam (`poll`, `read`), nil in production where
the real `unix.Poll`/`unix.Read` are used and set only by tests; a `Port` is
owned by one refclock goroutine, so no locking is involved. Four tests now
cover the path:

- `TestReadTimeoutClosedPipeIsNotATimeout` — the real closed pipe, asserting a
  prompt (< 1 s against a 2 s timeout) non-timeout disconnect rather than a
  specific error identity.
- `TestReadTimeoutReportsEOF` — the seam produces the pair no pipe can, `n=0,
  err=nil` after a readable poll, and requires it to wrap `io.EOF`.
- `TestReadTimeoutHangupIsNotEOF` — pins the production HUP contract: `read(2)`
  is never called, and the error is neither a timeout nor an `io.EOF`.
- `TestReadTimeoutStillTimesOut` — the ordinary no-data path is still
  `ErrTimeout` and does not call `read(2)`.

**Files.** `internal/serial/serial.go`, `internal/serial/read_unix.go`,
`internal/serial/read_unix_test.go`.

**Verification.** The package is `//go:build linux || freebsd`, so it cannot
execute on this host. `go vet` passes for both GOOSes and the test binary
compiles for linux/amd64, linux/arm64, freebsd/amd64 and freebsd/arm64.
`CGO_ENABLED=1 go test -race ./...` still passes natively. **Deferred to the
target hosts:** running the four tests on Linux and FreeBSD.
