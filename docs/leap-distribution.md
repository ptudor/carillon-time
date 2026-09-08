# Leap-table distribution over NTP

Status: **design accepted 2026-09-08; implementation pending (M5)**. This is
the detailed specification for [DESIGN §6.8](../DESIGN.md#68-leap-seconds)
and §8.5. Configuration below is proposed syntax; current releases reject
the new keys. This document does not describe a deployed wire protocol.

## Purpose and deployment

A GPS/NMEA/PPS host needs advance leap information even when its network
sources disappear. Carillon servers and refclock hosts require a valid local
table before advertising synchronization. One installation fetches the NIST
file; other Carillon hosts learn and persist the same file from explicitly
trusted upstreams. All continue to use their ordinary time sources. A cached
leap table supplies neither a clock measurement nor PPS second numbering.

```mermaid
flowchart LR
    N[NIST HTTPS] --> S[Configured seed]
    S -->|Authenticated NTP extension| H[GPS/PPS host with local cache]
    H -->|Authenticated NTP extension| C[Downstream Carillon with local cache]
    C -->|Standard NTP leap indicator| O[Ordinary NTP clients]
```

Any host can be the seed. The leap distribution graph need not follow the
selected time source. An authenticated `noselect` association may distribute
a table without contributing time. Each additional hop must be configured
for leap trust; reception does not authorize a new distributor automatically.

The protocol is a pull exchange: "I have this SHA-256, data-update date and
expiry; what do you have?" An equal digest requires no transfer. A newer
acceptable manifest triggers requests for the actual file. A hash alone never
supplies leap authority, including at first boot.

## Trust

For the initial implementation, acceptance requires all of:

- The distributor is a configured `[[server]]` with `leap_trust = true` and
  a nonzero AES-128-CMAC key ID. Trust is explicit even when that server is
  already trusted for time. Plain NTP, a low stratum, `prefer`, and an ACL
  cannot substitute for this authorization.
- Every manifest and chunk is authenticated by that key over the entire NTP
  header and extension. The response comes from the queried endpoint, echoes
  the request's NTP origin value and fresh 128-bit request ID, and matches an
  outstanding operation. Authentication precedes allocation or state changes.
- The assembled bytes match the advertised digest, parsed metadata matches
  the manifest, and freshness, history and rollback checks below pass.

The server also requires the request key in `serve.leap_keys`, in addition to
the ordinary NTP ACL and rate limits. A time-service key is not automatically
permission to request leap data. Different downstream associations should
have different keys: sharing a symmetric key lets its holders impersonate
one another. An unauthenticated request receives no leap extension or file.

This is trust in the distributor, inherited at each configured hop. A relay
does not provide independently verifiable proof of a NIST download. A
compromised authorized distributor or stolen key can still supply a plausible
false future event. Digest checks, replay rejection and history checks limit
the failure but cannot establish the truth of such an event. Public,
untrusted relays would require separately provisioned origin signing keys and
signed objects; that is outside M5. No trust-on-first-use, public-peer voting,
Autokey negotiation, or packet-supplied download URLs are introduced.

## File identity and seed retrieval

The object is the **exact original byte sequence** of `leap-seconds.list`.
Comments and line endings are preserved; relays never normalize, re-date or
extend it. SHA-256 covers the entire object. Its manifest contains:

| Field | Meaning |
|---|---|
| `sha256` | 32 binary digest bytes; hexadecimal only in operator output |
| `data_updated` | File's `#$` date of the last change to leap records |
| `expires` | File's `#@` validity deadline, also advanced for no-new-leap renewals |
| `size` | Exact byte length, at most 65,536 bytes |

Both dates are unsigned, big-endian **64-bit whole seconds since
1900-01-01T00:00:00Z**. They are not NTP's 32.32 timestamp format and do not
wrap in 2036. Values outside the implementation's supported UTC calendar are
rejected before conversion. HTTP timestamps, file mtime and local receipt time
never become `data_updated`. The current parser accepts manual files
without `#$`; M5 requires it for new manual imports too. An operator must
obtain a dated source file rather than invent an update date locally.

**Date encoding rationale.** These are absolute file dates, with
no fractional-second information to preserve. NTP's ordinary header timestamps
remain 32.32; their 32-bit seconds component needs era context after its 2036
wrap. Persisted metadata uses the full seconds value so comparison and rollback
checks do not depend on reconstructing an era from the machine's current
clock. The epoch is still NTP's. See [RFC 5905 §6](https://www.rfc-editor.org/rfc/rfc5905.html#section-6).
Do not encode these fields using the packet timestamp's `ntp.Time` helpers.

**Expiry-only renewal.** NIST's file comments explicitly
allow `#@` to advance while `#$` and the transition records stay unchanged.
The file retrieved on 2026-09-08 has `#$ 3676924800` (2016-07-08) and
`#@ 4007404800` (2026-12-28). Therefore `#$` alone is not a revision number,
and the age of `#$` does not determine freshness. Acceptance compares both
dates and the digest under the rules below.
[Source: NIST leap file](https://tf.nist.gov/leap-seconds.list).

The `nist` acquisition mode uses
[`https://tf.nist.gov/leap-seconds.list`](https://tf.nist.gov/leap-seconds.list),
the HTTPS location published by
[NIST's Internet Time Service](https://www.nist.gov/pml/time-and-frequency-division/time-distribution/internet-time-service-its).
It validates the system CA chain, hostname and certificate dates, requests
identity content encoding, rejects redirects, and caps the response at 64 KiB
with a 30-second overall deadline. HTTP downgrade and TLS verification bypass
are prohibited. The embedded file hash, if present, is not an origin signature.

The seed checks on startup when no usable table exists, then every 24 hours
with ±10% jitter. Errors back off from 15 minutes to six hours with jitter;
an existing valid table stays active. Conditional requests may use HTTP ETag
or Last-Modified, but `304 Not Modified` never renews the table's expiry.
Refreshing because an unchanged table approaches expiry does not justify a
tighter retry loop. Peer mode never automatically switches to NIST: this
keeps one designated fetcher and one externally observable download policy.

Offline operators may provision an authenticated copy as a local file. Before
departing the network they must check coverage through the intended deployment
period. Beyond expiry, a new announcement or refreshed no-event assurance is
needed; retaining the old file cannot extend that assurance.

## NTP exchange

Only NTPv4 client/server modes 3/4 are used, on the existing UDP/123 listener.
The experimental extension type is **0xF504**, with magic `CLPS` and version 1.
It is a Carillon experiment, not an IANA allocation. The
[IANA registry](https://www.iana.org/assignments/ntp-parameters#ntp-parameters-3)
reserves 0xF000–0xFFFF for experiments; a public interoperable specification
needs registration before claiming a permanent type. Do not reuse the
existing Autokey leapseconds message types. A magic/version mismatch grants
no capabilities and is handled as an unsupported extension.

The field follows [RFC 7822 framing](https://www.rfc-editor.org/rfc/rfc7822.html)
and requires a trailing AES-128-CMAC MAC: four-byte key ID and 16-byte tag over
the header and all extension bytes, including padding. This reuses Carillon's
[RFC 8573 authentication](https://www.rfc-editor.org/rfc/rfc8573).
One CLPS field is allowed per packet; duplicate fields or invalid lengths
are rejected. No unauthenticated discovery exchange is required.

All integer fields use network byte order. Offsets are from the beginning of
the extension, including its four-byte framing header:

| Offset | Bytes | Field |
|---|---|---|
| 0 | 2 | Type = 0xF504 |
| 2 | 2 | Entire extension length, including padding; multiple of four |
| 4 | 4 | ASCII magic `CLPS` |
| 8 | 1 | Version = 1 |
| 9 | 1 | Operation: PROBE=1, MANIFEST=2, GET=3, DATA=4 |
| 10 | 2 | Result: OK=0, NO_TABLE=1, NOT_FOUND=2 |
| 12 | 16 | Random request ID, echoed by the reply |
| 28 | 8 | Leap-record update date (`data_updated`) |
| 36 | 8 | Expiry date |
| 44 | 32 | SHA-256 |
| 76 | 4 | Total object size |
| 80 | 4 | Chunk byte offset |
| 84 | 2 | Requested or returned chunk byte count |
| 86 | 2 | Reserved, zero |
| 88 | variable | Chunk bytes or request padding, followed by zero word padding |

An unknown operation/result, nonzero reserved data, inconsistent metadata,
or unexpected response operation rejects the exchange. Request results are
always OK. The base field is 88 bytes; the NTP header and MAC add 68 bytes.

1. **PROBE → MANIFEST.** A 156-byte authenticated request includes the
   receiver's cached manifest, or a zero manifest if it has none. Offset and
   count are zero. The equally sized response describes the distributor's
   valid, durably stored object. With none available it returns NO_TABLE and
   a zero manifest. An expired table may be reported in local diagnostics
   but is never advertised as available. Comparing the digest avoids repeated
   transfer; a different digest is only a candidate, not permission to replace.
2. **GET → DATA.** Each request names the exact manifest, an offset aligned
   to 256 bytes, and count=256. It includes 256 zero padding bytes, yielding
   a 412-byte datagram. The reply echoes that manifest and offset, and returns
   `min(256, size-offset)` file bytes with the corresponding count and word
   padding. Validate `offset < size`, bounds and all additions before slicing.
   A last partial chunk can therefore produce a shorter response.
3. **Changed object.** The responder serves one immutable active object and
   needs no per-client transfer state. A GET naming an object it no longer
   holds returns DATA/NOT_FOUND, echoes the requested manifest and offset,
   and sets count=0 with no chunk bytes. The learner discards its partial
   candidate and probes again with backoff. An expired current object is
   handled the same way. No response mixes versions.
4. **Acceptance.** Once every byte is present, the learner recomputes SHA-256,
   validates the file and commits it as described below. Only then can it
   advertise that manifest to its own downstreams.

Every response is no larger than its request. There is one response per
request, no push, broadcast, external URL, or bulk reply. The largest CLPS
datagram is 412 bytes; all packets, including padding, are authenticated.
Existing ingress ACL, martian filtering, MAC checks and rate limits apply.
Authenticated leap traffic also has a per-key limit of one request per four
seconds with burst two. Across all listeners, CLPS processing has a global
limit of 32 requests per second with burst 32; exhausting it drops leap
requests while ordinary requests continue under their normal limits.

The learner uses a separate bounded worker and request sockets. PROBE/GET
responses never enter the clock filter, refresh time-source reach, qualify
PPS, or change the ordinary source polling schedule. Conversely, a valid
authenticated table may be received from a distributor whose NTP header says
LI=3: table provenance and clock synchronization are independent. This does
not allow that response to become a time sample. KoD, including RATE, is
honored and never treated as a manifest.

Probe eligible peers at startup, then every six hours with jitter. Try peers
in configuration order, with at most one transfer and one outstanding request
globally, 64 KiB of assembly storage, and a 30-minute transfer deadline.
Send at most one transfer request every four seconds, yielding to ordinary
time polls; retry an operation at most three times with backoff and a new
request ID and transmit nonce each time. Duplicate, delayed and unsolicited
responses have no effect. Other authorized peers can be tried after failure.

An older server may return an ordinary authenticated NTP reply without CLPS,
or drop the extension-bearing request. Record unsupported capability and
retry no sooner than 24 hours later; ordinary time polling continues on its
own schedule. Unsupported version responses behave likewise. No CLPS is sent
to unconfigured or unauthenticated public peers, and ordinary requests receive
ordinary NTP replies. Missing capability never weakens acceptance policy.

## Validation, persistence and activation

The worker validates a candidate before proposing it to the engine:

- Bound file and line lengths; require well-formed, unique `#$` and `#@`
  records for distributed objects, ordered transition dates, supported UTC
  boundaries, and single-second offset changes. Expiry follows `data_updated`
  and the last transition. Manifest dates and size match the parsed bytes.
- Establish plausible UTC from normal GPS calendar/NTP acquisition or an
  operator-established clock before trusting expiry or performing HTTPS.
  Acquisition may run while kernel/server synchronization remains withheld.
  Without a usable UTC estimate, downloaded data stays provisional and cannot
  be activated or redistributed. Reject `data_updated` more than five minutes
  in the future and expiry more than 400 days beyond established UTC; these are
  explicit Carillon acceptance limits, not claimed NIST format constraints.
- Require unexpired coverage of current UTC. Preserve the initial TAI-UTC
  baseline and all transitions already effective in the accepted history;
  a new file cannot omit or rewrite past events. A newly learned event already
  in the past is recorded as history, never scheduled as a retroactive leap.
- Compare against the durably recorded accepted dates and digest, even after
  expiry or restart. Either date decreasing is a rollback. An equal digest
  is a no-op and does not renew freshness. Both dates equal with a different
  digest is a conflict. At least one date must advance for a replacement:
  when only expiry advances, require identical parsed transition records and
  baseline; when `data_updated` advances, future records may change subject
  to the history and armed-event rules. A rejected advertisement never
  advances either accepted date. An expiry-only renewal changes the file's
  bytes and digest, and is an ordinary accepted update.

Multiple peers carrying one digest are copies, not independent votes. On a
conflict with identical dates, retain the accepted object, quarantine the candidate
and alert. Automatic recovery must not lower stored dates to escape a
poisoned future revision. An operator can explicitly replace trust settings
and locally reset the acceptance record with an audited action; no network
message can request that reset. An authorized distributor's compromise remains
the trust limitation described above.

Store original bytes, manifest, immediate-provider identity/key ID, acceptance
time and rollback/expiry state as one atomic generation in the daemon-owned
state directory. Use a same-filesystem temporary generation, file fsync,
atomic replacement and directory fsync. A single commit point couples the
object and rollback state, so power loss cannot install only one of them.
Retain the previous active object alongside a committed candidate until the
engine acknowledges activation; a failed applicability recheck must not discard
the last usable copy. Re-hash and re-parse after restart. Never overwrite a
configured operator-owned `daemon.leapfile`, follow peer-supplied paths, or
serve a partial download. At most the active generation and one candidate are
needed.

Network, hashing, parsing and filesystem work stay off the engine and NTP
responder goroutines. The worker offers an immutable candidate through a
bounded channel; the engine checks applicability, the worker commits the
approved generation, then the engine rechecks time/event validity before
activation. A commit acknowledgment identifies its exact generation. If time
has crossed expiry or a leap boundary meanwhile, it stays inactive and is
reported; persistence alone does not grant authority. Disk failure preserves
the active table and readiness, subject to its original expiry.

Apply the active table and leap-readiness result in the engine before the
next kernel status and server snapshot are published. Keep an armed
transition and its execution record separate from table replacement. During
the final day, quarantine revisions that remove or change the armed event;
unchanged events and expiry-only updates can proceed. Reset boundary-spanning
measurements exactly once when the kernel executes the event. Recovery after
a missed boundary requires fresh time evidence under the ordinary step policy.

An expired hash remains expired across backward clock changes and restart;
persist that fact and the last established UTC bound. Until a new clock
estimate reconciles a startup clock behind that bound, leap readiness stays
false. Expired bytes remain available for diagnosis/history checks but cannot
override live upstream LI or be advertised to downstreams. For required-table
hosts, missing authority means kernel STA_UNSYNC and NTP LI=3 / stratum 16
with experimental refid `XLEP`; clock acquisition continues. A network-only
client may fall back to fresh leap-capable survivors. RMC/ZDA and bare PPS
never vote "no leap" merely because they cannot announce one.

## Proposed configuration

These additions are **not accepted by current releases**. `daemon.leapfile`
remains the read-only manual source. With it configured, acquisition defaults
to `manual`; otherwise it defaults to `peers` when a leap-trusted association
exists, or `off` when none exists. Selecting any mode other than `manual`
together with `daemon.leapfile` is an error, so authority is never ambiguous.
Automatic modes use a cache directory `leap` beneath the platform's normal
state directory (`/var/db/carillon` or `/var/lib/carillon`).

`require_table` defaults true whenever a refclock or NTP service is configured,
and false for a network-only client. Setting it false with a refclock or NTP
service, or choosing `off` when a table is required, is a configuration error.
A plain network client therefore needs no new leap settings. Acquisition of
a table cannot supply the missing calendar source of a bare-PPS configuration.
Manual mode reads the configured file at startup and checks for replacements
once an hour, validating and committing them through the same worker. It
performs no network fetch and never changes the operator's file.

Seed additions to an otherwise complete server configuration:

```toml
[leap]
acquire = "nist"             # off | manual | nist | peers
require_table = true

[serve]
leap_keys = [1]               # augment the ordinary listener/ACL settings
```

Learner additions, using its existing configured CMAC key:

```toml
[leap]
acquire = "peers"
require_table = true

[[server]]
name = "home"
address = "10.9.0.1:123"
key = 1
leap_trust = true

[serve]
leap_keys = [2]               # optional redistribution using another key
```

`leap_trust` defaults false and `leap_keys` defaults empty. Key IDs must exist
in the private keys file; exports require an enabled ordinary NTP listener
and its ACL. Peer mode requires at least one eligible distributor; at most
four leap-trusted associations are accepted. It may learn while time sources
are `noselect` or unsynchronized. No discovery adds new servers, keys or ACLs.

`-check` remains offline: validate policy, keys, cache ownership and any
existing object; fail for invalid manual data or impossible acquisition
settings. An empty automatic cache is reported as awaiting acquisition,
with startup synchronized service withheld until a durable valid copy arrives.
A valid cache permits disconnected startup. Data updates are runtime inputs;
configuration, trust changes and key changes still require a restart.

## Observability and acceptance

Tracking/JSON report effective readiness, whether a table is required,
authority (`file`, `nist`, `peer`, `sources`, `unknown`), digest, data-update date,
expiry, immediate provider/key ID, last fetch result and last rejection reason.
Do not label a relayed object "NIST verified" merely because a peer said so.
Report accepted, pending and rejected candidates separately. `waitsync` uses
effective readiness. Missing required data, expiry and conflicts have explicit
health reasons; an expiring table is degraded 30 days out. A fetch failure
does not withdraw a still-valid active table.

Add bounded counters for probes, bytes, accepted updates, failures, rollback
and conflict rejection, and cache-write failures. Expose data-update/expiry
as gauges and readiness as a boolean. Digests, dates, remote-supplied strings
and candidate identities must not become Prometheus labels. Successful update
logs identify the provider and old/new manifest once, without per-chunk noise.

M5 is complete only after these tests and acceptance evidence exist:

- Independent wire vectors, 2036/2038 date cases, MAC coverage of every field
  and padding, and fuzzing of lengths, offsets, duplicate fields and chunk
  order. Every accepted request preserves the response-size bound.
- Poisoning probes: unauthenticated and unauthorized keys, forged hashes,
  replay after restart, older update/expiry dates, conflicts with identical dates, altered
  history, future dates, expiry inflation, peer changes during transfer, and
  reflected or stale request IDs. Rejection leaves the active table intact.
- Crash/disk-failure cases at each persistence stage; time changes between
  validation, commit and activation; expiry across clock setbacks/restarts;
  no sample or reach side effects from leap transfer traffic.
- A simulated chain seeded once over test HTTPS, with outbound HTTPS denied
  to downstreams: identical bytes propagate, persist across restart, and
  refresh without restarting the loop. Include a NIST-style renewal with
  unchanged `#$`, advanced `#@`, identical leap records and a changed digest;
  it must propagate rather than be rejected as a same-version conflict.
  A lost distributor and failed NIST refresh leave valid local data usable.
  Ordinary and legacy NTP peers keep interoperating, including ignored/dropped
  extension probes and KoD backoff.
- Positive and negative leap simulations with GPS/NMEA/PPS as the only live
  time input, all network upstreams disconnected, and a valid local table:
  downstream LI, kernel flags, one boundary reset, continuous valid service
  and correct post-event seconds. Test final-day conflicting updates, a
  missed event, no table and expiry; none may quietly become `LeapNone`.
- Native Linux and FreeBSD validation of table activation and safe kernel
  transitions, followed by the live GPS/PPS acceptance already required by
  the main design. Simulation success alone is not live leap-event evidence.
