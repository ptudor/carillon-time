# Current limitations and acceptance status

Carillon is an early project with a written design, automated regression tests,
and recorded deployment acceptance. This page separates implemented behavior
from hardware evidence and unresolved policy. It is not a claim of independent
audit or conformance certification.

## Evidence available

The maintainer’s deployment record covers Linux and FreeBSD network timekeeping,
client/server operation, IPv4/IPv6, authenticated peers, drift persistence, init
integration, and monitoring. See [deployment acceptance](../deploy/ACCEPTANCE.md).
The ordinary suite exercises discipline with fake clocks and deterministic
simulations. Linux CI additionally compares ABI declarations with system headers.

Release builds cover Linux and FreeBSD on amd64 and arm64. Cross-build success
does not prove native runtime or hardware behavior. GitHub CI does not run a
FreeBSD kernel or change a runner’s clock.

## Acceptance still needed

- A live GPS/NMEA/PPS stratum-1 run on the supported hardware, including pulse
  loss, antenna loss, reacquisition, and long holdover. Autonomous leap handling
  still needs hardware evidence beyond the fake-clock positive/negative leap
  simulations in [the M5 acceptance matrix](leap-distribution.md#observability-and-acceptance).
- A sustained accuracy comparison against a measured reference. UART, USB,
  receiver, kernel, and host scheduling behavior all matter.
- Native FreeBSD ABI/race checks and device acceptance on each deployed
  architecture. Native Linux ARM testing does not substitute for FreeBSD ARM.

## Open review items

| Item | Current concern | Operator implication |
| --- | --- | --- |
| RA6X-021 | ZDA can supply time while recent receiver status reports an invalid fix. Receiver-specific time-validity policy needs captures and a decision. | Do not equate accepted ZDA measurements with independently established receiver time validity. Validate startup, invalid-status, and antenna-loss behavior with your receiver. |
| RA6X-057 | Source count does not prove an achievable, independent quorum. | Choose independent upstreams and test source loss; several hostnames can resolve to the same underlying source. |
| RA6X-040 | Negative root-delay interoperability needs an explicit representation policy and peer evidence. | Test the intended peer implementations and inspect root-delay behavior before depending on this edge case. |

The details and closure criteria are recorded in the
[Astra6 review fixes](../review/2026/09/FIXES_ASTRA6_XHIGH.md). These items are
not closed by packaging or by a successful CI build.

## Leap-data trust and offline coverage

M5 implements manual refresh, NIST HTTPS acquisition, authenticated CLPS
transfers, durable caches and synchronized-service gating. This closes
RA6X-023: expired data loses authority and cannot be revived by a clock setback.
NMEA and PPS do not vote on leap state. See [verification](../review/2026/09/M5_IMPLEMENTATION.md).

A SHA-256 identifies bytes, and CMAC authenticates the immediate configured
distributor. There is no independent NIST origin signature. A compromised
authorized distributor can still invent a plausible future event; public
unauthenticated relay trust is outside this implementation. Offline coverage
ends at the file's expiry. Keep that deadline beyond the intended disconnected
deployment, and verify it in `carillonctl tracking` before disconnecting.

## Scope

NTS, PTP, hardware NIC timestamping, and general ntpd/chrony feature parity are
outside this release. AES-CMAC uses pre-shared keys; it is not NTS. Public NTP
serving requires explicit ACLs and suitable capacity settings. macOS supports
development and `query`, with no system-clock discipline backend. The existing
OpenWrt notes describe deferred work, not a supported Go release target.
