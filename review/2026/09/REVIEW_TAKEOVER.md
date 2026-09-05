Review of `adbfbfe`, `9cb190e`, `751c23c`, `5561238`, and the filter → source → system → loop path, 2026-09-05.

Code locations and numerical results below refer to `adbfbfe`.

`adbfbfe` arrived during the review and committed the other chat's final handoff. Both it and `9cb190e` change documentation only. The two preceding code fixes improve shutdown persistence and settling, but the handoff overstates their protection and misidentifies part of the filter mechanism. Production source and host configurations were not changed during this review.

**[P1] Historical filter offsets are applied as present-time feedback, destabilizing the faster loop.**

Location: `internal/discipline/system.go:359–361`, `internal/discipline/loop.go:248–255`, and `internal/discipline/filter.go:89–92`.

The system checks whether `At` advances, but then passes `sel.Offset` and delivery time `now` to the loop without accounting for observation age. A strictly increasing sequence of delays makes the eight-stage filter release the oldest remaining sample on each wrap. Those are distinct samples, but up to seven poll intervals old. The phase slew and frequency correction made since observation are not reflected in the historical offset. Even frequent loop updates can therefore supply harmful feedback; eliminating long gaps alone is insufficient.

A deterministic 12-hour simulation uses the actual Filter, System and Loop, a perfect external reference, the correct initial frequency correction (+15.4 ppm against −15.4 ppm crystal drift), and only 10 ms initial phase error. RTT rises by 50 µs per poll and resets every 128 polls, ranging from 20 to 26.35 ms. Symmetric delay changes introduce **no offset bias**. With fixed poll 6, selected observations reach 448 s old; peak frequency error is **358.227 ppm**, peak true offset **315.264 ms**, and final-hour RMS **167.077 ms**. This is an isolated stability reproduction, not a replay of gummi's traffic or proof of the precise live cascade.

`TestLoopConvergesWhateverTheUpdateSpacing` does not cover this: it calls `clk.offset()` at every delivery, so all observations are current. Its passing result cannot clear the filter/loop combination. A fix needs an explicit policy for delayed observations and gains compatible with that policy; merely changing the integration interval or replaying the previous estimate is insufficient.

**[P1] The drift-file gate mistakes a starved estimator for a settled estimator.**

Location: `internal/engine/engine.go:583`, `720–734`, and `739–746`; introduced by `5561238`.

`handle` records the base frequency on every one-second tick. `frequencySettled` checks only elapsed history and frequency spread. During starvation, the base estimate cannot change, so the gate opens after 900 seconds even when no new evidence supports it. Neither `FreqKnown` nor the synchronization state establishes current frequency accuracy.

The engine regression starts with a good **6.125000 ppm** drift file. Two accepted measurements move the estimate to **30.539062 ppm**. Subsequent successful polls report no fresh filter estimate, while normal engine ticks continue. After 900 seconds without a loop update, the periodic writer replaces the good file with **30.539062**, while status still says `synced`. Shutdown uses the same gate. The test fails on the actual persisted file contents, using `clock.Fake` throughout.

The recorded refusals at the two live restarts are useful and consistent with this code: their windows still contained large frequency changes. They do not establish protection against a later plateau. Persistence must require continuing accepted discipline evidence spanning the stability window, with a freshness policy for long polls, and preserve the previous file when evidence stops. A constant correction during a sustained upstream rate error is another reason spread alone cannot prove crystal accuracy.

**[P2] The latest commit's explanation of the 2533-second gap contradicts the code.**

Location: `DESIGN.md:575–592` and the latest deployment entry in `deploy/ACCEPTANCE.md`.

After `now - sample.t > 2048`, this filter ranks the sample as `MaxDistance + dispersion`, where `MaxDistance = 1.5` seconds. A fresh 25 ms reply therefore beats a 20 ms reply older than 2048 seconds; the old reply does **not** also need to lose on raw delay. The regression explicitly verifies this at age 2533 seconds. Before then, eight successful new arrivals also evict the sample.

Demotion happens on arrival, not on a timer. With one early 20 ms reply and subsequent 25 ms replies, the observed gaps in the fixed-poll reproduction are:

| Poll exponent | Interval | Baseline gap | `delay/2 + dispersion` candidate gap |
|---|---:|---:|---:|
| 4 | 16 s | 128 s | 128 s |
| 6 | 64 s | 512 s | 192 s |
| 8 | 256 s | 2048 s | 256 s |
| 9 | 512 s | 2560 s | 512 s |
| 10 | 1024 s | 3072 s | 1024 s |

A 2533-second gap is compatible with five jittered poll-9 intervals; packet-level history is needed to establish whether that explains this incident. The existing starvation test's 2048-second result is ring eviction at exactly eight 256-second arrivals, not Allan demotion: that branch uses strict `>`.

The proposed age calculation also needs the half-delay term. With equal initial dispersions, a **10 ms RTT advantage** is offset by aging after approximately `0.010 / (2 * 15e-6) = 333 s`, or 5.6 minutes before poll quantization. Eleven minutes would correspond to a 10 ms advantage already expressed as one-way distance.

**Assessment of the two proposed changes.**

Age-aware ranking is promising, but the one-line change is not a sufficient release fix. In the simulation above it removes historical feedback and converges. It also passes all existing `TestSim*` cases, including unknown frequency, steps, panic, falsetickers, holdover, and tick jitter. However, increasing the RTT rise to 1 ms per poll at poll 4 makes the half-delay advantage grow faster than dispersion. The candidate again selects observations 112 seconds old and reaches **293.710 ppm** peak frequency error with **28.505 ms** final-hour RMS, even with symmetric delays. The broader issue is delayed feedback relative to the loop's time constant.

Increasing twocom's poll minimum can reduce its response to a given upstream slew, but also lengthens the eight-sample window. With the existing ranking, changing fixed poll 4 to poll 6 did not cure the symmetric-delay reproduction; the final-hour RMS increased from 28.505 to 167.077 ms. This does not predict the exact result of an adaptive `poll_min` change on twocom, but rules out treating it as an independently sufficient stability fix. Retain it as a possible temporary tuning measure after testing the chain's actual settings and upstream transient.

The local filter and loop must be evaluated together. RFC 5905 describes delay sorting and the eight-stage register; its introductory text distinguishes protocol requirements from the example implementation. ntpd's source also documents the eight-arrival lifetime, but its post-Allan metric and loop behavior are not identical to this daemon's. Copying its ranking does not establish stability for carillon's deliberately faster gains. Sources: [RFC 5905 §1](https://www.rfc-editor.org/rfc/rfc5905.html#section-1), [§10](https://www.rfc-editor.org/rfc/rfc5905.html#section-10), and [ntpd clock_filter source](https://github.com/ntp-project/ntp/blob/stable/ntpd/ntp_proto.c).

**Live state and verification.**

Read-only SSH snapshots at approximately 14:52–14:54 UTC confirmed all three hosts running `9cb190e`, reporting `synced`, with zero steps since restart. Gummi still had +14.145 ms pending slew; twocom −1.014 ms; navlisten2026 +69.088 ms. Navlisten's selected source at that instant was `debian-2`, with gummi a survivor. These are timestamped snapshots, not settled-frequency measurements or a claim about later state.

The unmodified repository passes `go test -race ./...` and `go vet ./...` on Go 1.27.0/darwin-arm64. Socket tests required running outside the sandbox; clock tests used fakes. The proposed distance variant was evaluated through a Go overlay, without editing production code. Linux/FreeBSD kernel behavior and an adaptive, multi-host incident replay were not tested by these reproductions.

Reproduce with `python3 review/2026/09/takeover-repro/reproduce.py baseline` and then `python3 review/2026/09/takeover-repro/reproduce.py distance`. Both **intentionally exit nonzero** on this revision: baseline exposes the persistence and delayed-feedback failures; distance still fails persistence and the steeper poll-4 delayed-feedback case. The `.go.txt` fixtures are loaded into their corresponding internal packages via Go's overlay and do not participate in the normal test suite. Optional `GOCACHE` and `GOMODCACHE` environment variables can point to existing scratch caches; otherwise the runner uses temporary caches and may need to download the pinned modules.
