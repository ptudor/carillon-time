# M5 Fable 5.1 review fixes

Work date: 2026-09-09. Review: [REVIEW_M5_FABLE51.md](REVIEW_M5_FABLE51.md).
Starting commit: `2d3b07c`. Entries are updated with each completed checkpoint.
All clock tests use fake clocks.

## RM5-001 — Network-only client synchronization

Implemented the review's preferred policy, consistent with DESIGN §6.5 and
§6.8: missing fresh LI does not demote a plain client's discipline state,
stratum or kernel synchronization. Public LI remains 3 when unknown. Kernel
event flags are clear unless an earlier warning already armed a transition;
that event survives lost polls. Required-table hosts retain readiness gating.
The actuator's LI=3-to-STA_UNSYNC mapping is accounted for explicitly.

Validation: `go test -race ./internal/engine ./internal/clock` passes. New
regressions cover a tableless client after 129 seconds without replies, entry
to holdover, recovery on one reply, holdover expiry, and a split final-day
vote. The expired-table regression also checks that loss of fresh LI keeps an
already armed kernel insertion. Updated the older source-exit test to match
the documented plain-client holdover policy.

## RM5-006 — SETTLING leap status

A usable table can establish readiness and be exported during coarse UTC
settling, but its indicator and any latched-event override are published only
while SYNCED or HOLDOVER. SETTLING keeps LI=3 and cannot arm a kernel leap.

Validation: the same race run passes; the coarse-UTC export test now requires
public LI=3 as well as an unsynchronized kernel and no armed event.

RM5-007 boundary coverage is also added here: a delayed engine tick spanning
an insertion's repeated second and midnight resets each source once, records
execution, and preserves valid service without a daemon step.

## Remaining work

RM5-002, RM5-003, RM5-004, RM5-005, RM5-007 and RM5-008 are in progress.
