# Gate 5 report: Phase 5, Hardening and v0.1.0

Final. Written 2026-11-10 on `p5/soak` (PR #25 → `phase-5`) after the 24 h soak ended
(2026-11-09T18:40Z to 2026-11-10T18:40Z). It covers Phase 5 as merged through PR #35 (`phase-5` at
18cacac): the planned hardening work, and two pieces Blake added during the phase by ADR, the
operator console (ADR-0006) and rules an operator can read (ADR-0007). An earlier draft of this
report went in with PR #24 while the soak was running. Fable grades; Blake decides; v0.1.0 is
tagged only on Blake's word.

## 1. Scope delivered

| Planned item (docs/SPEC.md, Phase 5 row) | Status | Where |
| --- | --- | --- |
| Full bench runs with profiles | done on the dev box (2 vCPUs), **not on the reference node** (ADR-0005) | `bench/results/2026-11-09T181541Z/` (raw, sdk mix, throttle; CPU and allocation profiles), commit 8d2a209 |
| Tuning | none applied: the profile is kernel-bound (46.6% `Syscall6`, 35.8% under the per-chunk `Flush`, the proxy's own code under 3%); `GOMEMLIMIT=1GiB` is set by the unit and the image | ADR-0005 §Context |
| 24 h soak at 50% of peak with chaos injections | **done**: 24.00 h at 804 req/s (half of the 1,608 req/s calibrated peak at concurrency 16), 69,520,214 requests, 141 chaos steps, memory flat, 0 unexplained errors (section 3, row 9). It ran the build of PR #24 (40264aa); what changed in the data path since is in section 5 | `bench/soak.sh`, `make soak`, D-056; `bench/results/soak-2026-11-09T183931Z/` |
| Resource caps | done: `MemoryMax=1536M`, `GOMEMLIMIT=1GiB`, `LimitNOFILE=65536`, `TasksMax=16384` in the unit; `listen.max_inflight`, the limiter key cap, and the showback tuple cap in config (Phases 2 to 4) | `deploy/tollbooth.service`, `docs/OPERATIONS.md` §5 |
| `docs/OPERATIONS.md` runbook | done: install (tarball, systemd, container), configure, reload, upgrade without dropping connections, sizing, metric meanings, two tasks, retry modes, failure behavior, performance expectations | commit e33bd77 |
| systemd unit | done; verified by `systemd-analyze verify` in CI against the built binary (no `\|\| true`) | `deploy/tollbooth.service`, CI job `release` |
| Container image | done; built by `make image` in CI (distroless static, nonroot); the dev box has no docker | `deploy/Dockerfile`, `.dockerignore`, CI job `release` |
| `CHANGELOG.md` | done (`[Unreleased]` and an empty `[0.1.0]` awaiting the tag) | `CHANGELOG.md` |
| `make release` | done: linux/amd64 and linux/arm64 tarballs, `SHA256SUMS`, version check; artifacts uploaded from CI | `Makefile`, CI job `release` |
| Tagged release | **pending**: ADR-0005 (what v0.1.0 claims) is Blake's decision; the tag follows that decision | ADR-0005 |
| Not in the row, delivered | null backend `-control` fault injection; `loadgen -rate`; `bench/crosscheck.sh` (elbencho, ADR-0005 amending ADR-0002); CI `release` job | 849054e, 99d3494, c629b07, 65cb517 |
| Added by Blake during the phase: the demo kit for the POC | done: `demo/run.sh` (null backend or a real S3 endpoint), `demo/DEMO.md` with every output captured from a running stack, `loadgen` against a real endpoint with real credentials, health-probe knobs | PRs #26, #29, #30, #33 |
| Added by Blake: the TUI as the operator console (ADR-0006) | done: Graphs screen (braille charts of rates, TTFB, denials per rule, in-flight, memory), a Config editor that validates on every keystroke and saves atomically with a backup, the palette and navigation | PRs #27, #31, #32 |
| Added by Blake after the first demo: rules an operator can read (ADR-0007) | done: the `limits` schema in S3 operation names with the unit in the key, every rule read back as a sentence, `tollbooth_limiter_matched_total{rule}`, the Limits screen with live rates per rule and per operation, the guided form that writes a rule into the file, strict pacing as a named choice. **Replaces the rule schema of Phases 2 to 4** (nothing was tagged with it) | PRs #34, #35; D-058 to D-062 |
| Fix found on the way | `test/s3diff` signed the direct and the proxied send of a streaming case with two timestamps a second apart, a CI flake | PR #28 |

## 2. Decisions and assumptions

- [ADR-0005](../adr/0005-performance-targets-at-v0.1.0.md), **proposed, awaiting Blake**: (1) what
  v0.1.0 claims, recommendation (b): tag with the five hardware-independent rows met on the dev box
  and the four end-to-end rows marked unverified until a reference-node run; (2) stay on Go,
  reopened only by a reference-node profile that shows the runtime rather than the kernel at the
  top; (3) elbencho replaces warp as the external cross-check.
- [ADR-0006](../adr/0006-tui-operator-console.md), **accepted** (Blake, by merging #27): challenged
  the fixed decision that the TUI is read-only and nothing in tollbooth writes the config file.
  The TUI writes it on an explicit save; the CLI never does.
- [ADR-0007](../adr/0007-rules-an-operator-can-read.md), **accepted** (Blake, 2026-11-10):
  challenged the rule schema of the specification's Rate limiting section. The ADR carries an
  as-built table of where the implementation differs from the proposal.
- D-056 (the soak's definitions), D-057 (the editor's validation scope and the Graphs screen's
  sizes), D-058 (the unit in the key, the burst default, `per` on a blocked rule), D-059 (the form
  edits the file as text and verifies by re-parsing), D-060 (what the Limits screen is built
  from; the matches-nothing mark), D-061 (no class or mode word in operator-facing output), D-062
  (Blake's answers: the burst stays the default, strict pacing is `burst: 1` under a name; the mark
  stays).
- Specification text that now lags the accepted ADRs and the decisions log, for Fable to amend:
  the Rate limiting schema block and its "three parts" paragraph (ADR-0007); the Operator
  surface's sentence that neither the CLI nor the TUI writes the config, and its Limits row
  (ADR-0006, ADR-0007); test-plan category 7's "completes with default retry settings" (D-044,
  from Gate 2); test-plan row 8's hour of a request that crosses the boundary (D-047, from Gate 3).
- The specification's Phase 5 row assumes the reference node exists; it does not yet (ADR-0005).

## 3. Test results

G2 ran on the dev box on the final build (`run/g2-phase5.sh` at cca1c4b, which is `phase-5` at
18cacac merged into this branch; outputs under `run/g2-phase5/`; summary lines in the appendix).
CI on PR #35 at 1ec0a95, the same tree as 18cacac: run 42872079961, all green (`unit`, `audit`,
`codeql`, `semgrep`, `release`, `integration`). The first push of that PR failed `unit` and
`integration`; both failures were real and are in section 5.

| Test plan category (docs/SPEC.md numbering) | Result | Where |
| --- | --- | --- |
| 1 Classifier unit tests | pass, in the race suite (`internal/s3id`) | `run/g2-phase5/race.log` |
| 2 Identity unit tests | pass, in the race suite (`internal/s3id`) | same |
| 3 Limiter unit tests | pass, in the race suite (fake clock), with `TestMatchedCounter` for the new counter; microbenchmarks in `run/g2-phase5/bench.log` (144 benchmark lines; `Decide100Rules` 1,237 to 2,091 ns/op, 0 allocations) | same |
| 4 Fuzzing | 6 targets × 60 s, 0 crashers: `FuzzParse` (config) 453,069 executions, `FuzzEditRules` 247,828, `FuzzClassify` 1,710,949, `FuzzParse` (s3id) 1,270,156, `FuzzScanQuery` 1,245,483, `FuzzRead` 659,860. CI's 60 s run found a crasher in `FuzzEditRules` on the first push of PR #35 (section 5); fixed, then 300 s of that target clean (455,723 executions) | `run/fuzz.log` |
| 5 Byte-equivalence | 293 cases, 0 diffs, 0 backend gaps (record mode, dev box); Garage 293 cases, 0 diffs, 28 backend gaps and versitygw 293 cases, 0 diffs, 8 backend gaps in CI | `run/g2-phase5/s3diff-record.log`; run 42872079961 |
| 6 Client compatibility matrix | 122 cells, 0 failures, 2 backend gaps on Garage and versitygw (CI `integration`); `docs/COMPAT.md` unchanged **Against a VAST cluster** (5.5.3.0, the lab cluster, by hand on 2026-11-10, §8): byte-equivalence 184 cases, 0 diffs, 0 backend gaps; client matrix 48 cells, 43 pass, 0 failures, 5 backend gaps | `docs/COMPAT.md`; run 42872079961; `docs/COMPAT-vast.md` |
| 7 Throttling behavior | 7 rows, 5 completed, as in every run so far: aws cli default 68/200 (no), aws cli adaptive 200 (yes), boto3 default 189/200 (no), boto3 adaptive 200 (yes), s5cmd, rclone, s3cmd yes (D-044) | `docs/THROTTLE.md` (updated from run 42872079961) |
| 8 Showback correctness | pass, in the race suite (reconciliation, rollover, restart recovery); `FuzzRead`; the soak's 17 `showback-ro` steps: 25 failed snapshots logged, counting continued in memory, the next snapshot wrote everything | `run/g2-phase5/race.log`; the soak's `tollbooth.log` |
| 9 Chaos and resilience, and the Phase 5 soak | **24.00 h, 1,413 samples, 69,520,214 requests at 804 req/s** (half of the calibrated peak), 0 throttled, 141 fault steps: 18 each of backend 503, bounded stall, reset, process down, and invalid reload, 17 each of valid reload, read-only showback directory, and SIGHUP. Error events (proxy errors, backend 5xx, backend transport errors, flush errors) by window: backend-503 8,756 over 1,744,512 requests; backend-down 17,963 over 1,735,587; backend-reset 12,146 over 1,765,111; backend-stall 360 over 1,718,166; showback-ro 25 over 2,507,267; reload-invalid, reload-valid, and sighup 0. **Outside every window: 0.** 34 reloads applied and 18 invalid ones rejected under load. The proxy's log holds exactly what the rotation predicts: 54 "backend endpoint marked down" (18 each timeout, reset, dial), 18 rejected reloads, 25 failed snapshots, nothing else above `info`. RSS 27 to 37 MiB, final 29 MiB, slope over the second half −0.19 MiB/h; heap 5.9 MiB and 43 goroutines at the end (29 to 98 during the run). The generator counted 7,304 failed operations in 69,465,616 (0.0105%): the SDK giving up inside a fault window. Verdict of `bench/soak-report.py`: flat memory, zero unexplained errors | `bench/results/soak-2026-11-09T183931Z/` (`soak-report.txt`, `soak.csv`, `chaos.log`, `tollbooth.log`) |
| 10 Performance regression | nightly 42885875806 on the final build (47da8e0), started by Blake after this report was merged: **the comparison failed, 15 rows; no regression in the proxy was found.** The three causes, each checked: the job drew a different CPU model than the baseline's, and code untouched since Phase 1 measured 30 to 99% slower there; the comparison command pooled three packages' `BenchmarkParse` and reported an allocation increase no package had (config 914 → 913); and `SmallGETEndToEnd` went from 161 to 164 allocations because the null backend it runs against gained a response header for the demo (161 again with the header removed; the handler alone is 7 on both builds). The command and the baseline are corrected in the close-out PR (D-063); re-run against the new baseline, the same output gives 24 compared, 0 failed. Its `fuzz-1h` job passed: six targets at 600 s each (`FuzzParse` and `FuzzEditRules` in config, `FuzzClassify`, `FuzzParse`, and `FuzzScanQuery` in s3id, `FuzzRead` in showback), one CPU-hour, 0 crashers. The earlier nightly (42673827885, PR #24's build) had reported 13 compared, 0 failed, with the same pooling in it | `test/bench/compare`; §8 |
| 11 Static analysis | golangci-lint 0 issues (with the depguard rule), `go vet` clean, govulncheck no vulnerabilities, go-licenses clean with the copyleft deny list; Semgrep and CodeQL green in CI | `run/g2-phase5/`; run 42872079961 |
| 12 Operator surface | `TestOperatorSurfaceParity` green; 38 CLI golden outputs in three formats; 70 TUI golden frames at 80×24 and 120×40, including the Limits screen with live rates, a rule that matches nothing, and eleven states of the form. For the rule schema (ADR-0007): 32 invalid rule files each rejected with the key and the reason, the old schema among them with the new key named; 16 sentence cases; byte-exact edit tests over nine file layouts. The form was also driven in the real console (tmux) against a running proxy under 100 req/s: add, change, and delete each wrote the file, left a backup, reloaded in 0.3 to 0.6 ms, and afterwards the file was byte-identical to the original | `cmd/tollbooth`, `cmd/tollbooth/tui`, `internal/config`; `demo/DEMO.md` §6b |

The draft of this report numbered its rows 1 to 10 in its own order; the rows above follow the
specification's twelve categories.

Race: 10 packages ok under `-race -count=1`, 0 failures, coverage **89.8%** of statements (floor
85%). Skipped: none. Not run on the dev box: the client matrix and throttling rows (they need the Garage
and versitygw binaries; CI ran them, green).

## 4. Performance

Environment for every row: dev box, AMD EPYC 7402P, **2 vCPUs**, 7.5 GiB, Linux 5.14.0-570, Go
1.27.3, `GOMAXPROCS` default, `GOMEMLIMIT` default for the bench (1GiB for the soak), three
processes sharing the two cores (during the soak, more: section 5). Raw results, profiles, and the exact config:
`bench/results/2026-11-09T181541Z/` (`env.txt`, `tollbooth.yaml`, `raw-cpu.pprof`,
`raw-allocs.pprof`, the three JSON files). Method: 30 s warm-up, 30 s measured, three runs each,
direct and proxied interleaved, median reported.

| Row (docs/SPEC.md, Performance targets) | Target | Measured | Verdict |
| --- | --- | --- | --- |
| Added latency, 4 KiB GET, 100 rules, plaintext | p50 ≤ 150 µs, p99 ≤ 1 ms, p99.9 ≤ 5 ms | direct p50 3,174 µs / p99 21,840 µs; proxied p50 13,509 µs / p99 39,191 µs; added p50 10,334 µs, p99 17,351 µs, p99.9 25,788 µs at concurrency 32 | **unverified**: the direct run alone is 21× the p50 budget; two cores for three processes (ADR-0005) |
| Small-op throughput, plaintext, 100 rules, showback on | ≥ 50,000 req/s | direct 7,065 req/s, proxied 2,243 req/s (raw); sdk mix direct 2,967, proxied 1,607 | **unverified**, same cause |
| Small-op throughput, TLS terminated | ≥ 30,000 req/s | not measured | **unverified** |
| Large-object streaming, 32 × 64 MiB | ≥ 80% of line rate | not measured (loopback, no NIC) | **unverified** |
| Limiter decision, 100 rules, 1,000,000 keys | ≤ 10 µs p99 | p99 4.7 µs (Gate 2, gated test). The matched counter added this phase is one atomic add and one counter increment per rule a request is charged against: `Decide100Rules` +3.5% and the all-match case +1.3% before and after on the dev box, 0 allocations; 1,237 to 2,091 ns/op across six counts in the final G2 run, with a demo stack running on the same two cores | met |
| Showback increment | ≤ 2 µs p99; ≤ 2% on/off | 198 ns, 0 allocations (Gate 3); on/off within noise on shared cores (D-055) | increment met; on/off **unverified** |
| Memory at 1,000,000 keys + 250,000 tuples | ≤ 1 GiB RSS | 226 B/key + 257 B/tuple ≈ 290 MiB for the stores (Gates 2, 3). Soak: RSS 27 to 37 MiB over 24 h at 804 req/s, slope −0.19 MiB/h over the second half, heap 5.9 MiB and 43 goroutines at the end (the soak's key space is small: 40 limiter keys, 208 showback tuples, so it shows steadiness, not the store sizes) | met for the stores; flat over 24 h |
| Config reload under load | ≤ 100 ms, 0 failed | 0.60 ms, 0 failed (Gate 2). Soak: 34 reloads applied (17 by `POST /admin/reload`, 17 by SIGHUP) and 18 invalid ones rejected under 804 req/s, 0 error events in those windows over 5,114,845 requests. Reloads from the console's form against a running proxy under 100 req/s: 0.3 to 0.6 ms | met |
| Startup to ready | ≤ 1 s | 65 to 78 ms (Gate 2) | met |

Profile (`raw-cpu.pprof`, 10 s during the proxied raw run, 4.16 s of samples): `Syscall6` 46.6%
flat; `bufio.(*Writer).Flush` 35.8% cumulative (the per-chunk flush transparency requires, D-006);
`limiter.(*Limiter).evaluate` 2.6% cumulative; map hashing 2.4%. Allocation profile
(`raw-allocs.pprof`, objects): `net/textproto.readMIMEHeader` 17.8%, `persistConn.roundTrip` 8.8%,
`persistConn.readLoop` 7.0%, context creation 13.8% across three frames, `Header.Clone` 4.1%: all
in `net/http`'s client and server, none in the handler.

Throttle scenario (sdk mix against a 1 req/s rule): 16,432 attempts, 918 throttled, 0 errors,
p50 946 µs, p99 1.68 s (the SDK's backoff).

Soak latency, through the proxy at 804 req/s for 24 h with a fault injected every 10 minutes (sdk
mix, GET 70 / PUT 20 / HEAD 10, concurrency 16, the shared box): p50 2,466 µs, p90 12,378 µs,
p99 31,965 µs, p99.9 55,181 µs; the maximum, 15.06 s, is a request held by a backend-stall step.
The rate held: over the 1,113 one-minute intervals outside any fault the mean was 805.3 req/s and
the lowest full interval 780 or above.

## 5. Known gaps and risks

| Gap | Proposed fix | Phase |
| --- | --- | --- |
| **The benchmark regression gate was wrong twice in this phase.** benchstat compared nothing (found and replaced earlier in the phase), and its replacement matched benchmarks by name across packages and enforced timings across CPU models (found by the nightly on the final build). On a nightly that draws a CPU model other than the baseline's, the 10% timing threshold is now reported and not enforced; allocations always are | fixed in the close-out PR (D-063) with tests for both defects and a re-recorded baseline. What remains open is a machine that does not change: a self-hosted runner, or one baseline per CPU model | 5 (done); the runner question is Blake's |
| The client compatibility matrix had never been run against a VAST cluster, which test-plan category 6 asks for; this report's first version did not list it | **done before the tag** (§8): `make compat-vast` with `VAST_BUCKET`, results in `docs/COMPAT-vast.md`. Not covered there: make-bucket and remove-bucket (the key may not create buckets), virtual-hosted addressing, and TLS to the cluster | 5 (done); the three uncovered items with the reference-node session |
| **The 24 h soak ran the build of PR #24 (40264aa), not the final one.** Between the two, the data path changed in `internal/limiter` only: the matched counter in the decision pass (22 lines in `limiter.go`) and rule compilation from the new schema; `internal/proxy`, `internal/showback`, and `internal/s3id` are byte-identical | the soak smoke on the final build: 0.11 h at 638 req/s, 246,314 requests, 12 chaos steps, 0 unexplained errors, RSS 28 to 33 MiB (`bench/results/soak-2026-11-10T185447Z/`). A 24 h run on the final build has not been done; question 6 | 5 (Blake's call) |
| **The dev box was shared during the soak**: the POC demo stack (a second tollbooth and a load generator at 300 req/s against the VAST lab cluster) from about 2026-11-10T01:00Z to 18:25Z, the validation runs for PR #35 between about 16:55Z and 18:20Z (the race suite, about 12 minutes of fuzzing at full CPU, throwaway demo stacks at 100 req/s), and a new demo at 100 req/s from 18:28Z. The soak held its rate throughout (section 4), so its error and memory verdicts stand; its latency percentiles include that contention | a soak on a machine with nothing else on it is part of the reference-node session | v0.1.1 |
| **A resolver outage makes the proxy report not ready while it is serving.** Observed in the POC demo on 2026-11-10 from 18:29Z: the dev box's DNS server stopped answering, the health probe (which resolves the endpoint name on every check) marked the only backend down, `/readyz` went not ready, and the proxy went on serving 100 req/s with 0 errors on its open connections. A load balancer acting on `/readyz` would have pulled a node that was working | **fixed before the tag** on Blake's answer (§8): ADR-0008. The pool remembers what each endpoint's name last resolved to and connects and probes there when a lookup fails; a failed lookup is counted and shown and no longer marks an endpoint down by itself | 5 (done, close-out PR) |
| The POC ran against the VAST lab cluster over plaintext HTTP (the cluster presents a self-signed certificate and a start with `insecure_skip_verify` was not done). `backend.tls` with a private CA bundle is covered by `internal/proxy`'s tests, not by a run against a real cluster | the reference-node session, with the cluster's CA in `backend.tls.ca_bundle` | v0.1.1 |
| CI's fuzzer found a panic in the form's file editing that 60 s on the dev box had not (`FuzzEditRules`, 21 s in CI: a rule ending in an empty block scalar on the file's last line). Nothing would have been written, but the console would have died | fixed in PR #35 (bdf9ed5) with the input as a seed and a unit test; 300 s of the target afterwards clean; the nightly's hour of fuzzing now includes the target | 5 (done) |
| The matches-nothing mark on the Limits screen also appears on a rule for an idle tenant: it cannot tell "wrong key" from "no traffic yet" | kept by Blake's decision (D-062); the form flags an access key or bucket the proxy has not seen at the moment the rule is written, which is the sharper check | — |
| From ADR-0007's proposal, not built: the rule's sentence under the editor's cursor, and the `l` key on the Limits screen | listed in the ADR's as-built table; neither blocks the flow the form covers | 6 if asked for |
| Four end-to-end performance rows unmeasured on the reference node | run `make bench-e2e` and `bench/crosscheck.sh` on the 16 vCPU node with pinned CPU sets; v0.1.1 gate (ADR-0005 option b) | 5 → v0.1.1 |
| `bench/crosscheck.sh` has not run anywhere yet (needs a real S3 backend for elbencho's writes; the dev box has no backend process outside CI) | run it in the nightly against Garage from `test/integration/backends.sh`, or on the reference node | 5 → v0.1.1 |
| Container image built in CI only; never run on a host with docker by this project | `docker run … version` runs in CI (`make image`); a host run is part of the reference-node session | v0.1.1 |
| The unit file is verified by `systemd-analyze`, not by a start on a real host | the reference-node session installs the tarball with the runbook §1 and runs the two tasks from it | v0.1.1 |
| `bench/nullbackend` and `bench/loadgen` have no unit tests (pre-existing; the null backend is exercised by `internal/proxy`'s tests, the generator by every bench and soak run) | a test for the control listener's parsing and for `-rate`'s pacing arithmetic | 6 |
| Test-to-code ratio moved from 0.654 at Gate 4 to 0.616 (10,173 test lines over 16,511): the console's screens and form are tested through 70 golden frames (and the CLI through 38 golden outputs), which are text files and not counted, and the bench tooling has no unit tests | the row above for the bench tooling; coverage under `-race` is 89.8% of statements | 6 |
| The soak's error attribution is by sample, so a step that ends just before a sample and the next that begins just after share a label (`a/b`) | acceptable at 60 s samples and 10 min spacing (steps never share a sample); recorded in D-056 | — |
| **The benchmark regression gate never enforced anything**: benchstat printed the baseline and the new run as two tables with no deltas (Gate 2 and 3: different `GOMAXPROCS` suffixes; nightly: a `go: downloading` line captured into `new.txt` made it a different configuration) and exited 0, so the specification's ±10% threshold and "an allocation on the decision path fails the build" were claims, not checks, from Phase 2 until today | fixed this phase (6093653): `test/bench/compare` (stdlib, tested) takes medians, compares same-name benchmarks, exits 1 past the threshold or on any allocs/op increase; `make bench-compare` and the nightly use it; benchstat dropped from `docs/deps.md` | 5 (done) |

## 6. Questions

1. ADR-0005 (1): tag v0.1.0 as option (b), with the four end-to-end rows marked unverified in
   `CHANGELOG.md` and `docs/OPERATIONS.md` §11? **Recommended: yes.** The soak has now reported
   flat memory and zero unexplained errors; (c) if a larger machine appears first.
2. ADR-0005 (2): accept "stay on Go" with the reopen condition (a reference-node profile that puts
   the runtime, not the kernel, at the top)? **Recommended: yes.**
3. ADR-0005 (3): accept elbencho as the external cross-check in place of warp, run on the
   reference node and in the nightly against Garage? **Recommended: yes.**
4. Who tags: Blake tags `v0.1.0` himself, or I tag on his word after merging `phase-5` → `main`?
   **Recommended: on his word, after the merge, same as `phase-N` tags.**
5. Is a 24 h soak on the dev box acceptable as the Phase 5 soak, with a second one on the
   reference node at v0.1.1? **Recommended: yes**; the soak tests the proxy's steadiness, not the
   hardware's speed.
6. The soak ran the build of PR #24; the final build differs in the limiter's decision pass by the
   matched counter and passes the soak smoke. Accept that, or run another 24 h on the final build
   before the tag? **Recommended: accept**, on the grounds that the change is one atomic add and one
   counter increment covered by the limiter suite and the benchmark gate, and schedule the next
   24 h run for the reference node. If the tag can wait a day, the rerun costs nothing but the day.
7. Specification amendments (Fable's edits, listed in section 2): the Rate limiting schema block,
   the Operator surface's two sentences, category 7's wording, test-plan row 8. Proceed?
8. The resolver finding (section 5): for v0.1.0, is the documented workaround (endpoints by
   address) enough, with the health-check change as a Phase 6 ADR? **Recommended: yes.**

## 7. Reproduce

```
git checkout p5/soak                         # this report's branch; phase-5 at 18cacac plus the soak results
make release                                 # dist/*.tar.gz, SHA256SUMS, version line
make build && mkdir -p run/unit && sed "s#/usr/local/bin/tollbooth#$PWD/bin/tollbooth#g" deploy/tollbooth.service > run/unit/tollbooth.service && systemd-analyze verify --man=no run/unit/tollbooth.service
make image                                   # where docker exists
run/g2-phase5.sh                             # lint, vet, race, fuzz 60 s, s3diff-record, bench, vuln, licenses, build, tidy → run/g2-phase5/
make bench-e2e                               # bench/results/<date>/ with profiles (section 4)
SOAK_HOURS=0.1 CHAOS_EVERY=15s SAMPLE_SECS=10 CALIBRATE=5s CONCURRENCY=8 make soak   # the smoke
make soak                                    # the 24 h run (SOAK_HOURS=24 CHAOS_EVERY=10m SAMPLE_SECS=60 CONCURRENCY=16)
bench/soak-report.py bench/results/soak-2026-11-09T183931Z    # the verdict, recomputed from soak.csv
make demo && source demo/env.sh              # the demo stack; demo/DEMO.md §6b is the console and the form
gh run download 42872079961 -n compat-logs   # docs/COMPAT.md and docs/THROTTLE.md as CI produced them
gh workflow run nightly.yml --ref p5/soak    # the fixed-runner benchmark comparison (category 10)
```

## 8. After the report: the nightly and Blake's answers (2026-11-10)

PR #25 was merged at 19:29Z. Blake then started the nightly on the final build (category 10 above)
and answered the questions of section 6, one by one:

| # | Question | Answer |
| --- | --- | --- |
| 1 | What v0.1.0 claims (ADR-0005) | Tag, with the four end-to-end rows marked unverified; the reference-node run is the gate for v0.1.1 |
| 2 | Go or Rust (ADR-0005) | Stay on Go; reopen only on a reference-node profile that shows the runtime at the top |
| 3 | The bench cross-check (ADR-0005) | elbencho replaces warp |
| 4 | Who tags | The builder, on Blake's word, after Blake merges `phase-5` into `main` |
| 5 | The dev-box soak | Accepted; a second soak on the reference node at v0.1.1 |
| 6 | The soak ran an earlier build | Accepted with the smoke on the final build; no rerun |
| 7 | Specification amendments | Blake or Fable make them; the places and the proposed wording are in `docs/reports/gate-5-spec-amendments.md` |
| 8 | The resolver finding | **Fix before the tag**: an ADR and the health-check change now, not in Phase 6. Scope, asked next: the health check **and** new connections. Built as ADR-0008 in the close-out PR: the pool remembers what each name last resolved to and dials it when the lookup fails |
| extra | The VAST client matrix (category 6) | **Before the tag** |

ADR-0005 is accepted. Gate 5 therefore passes on three conditions, all before the tag: the
resolver fix (ADR-0008) and the benchmark-gate correction, both in the close-out PR, and the client
matrix against a VAST cluster, which follows it. Phase 6 does not start before them.

### The client matrix against VAST (2026-11-10, Blake's run)

`VAST_ENDPOINT=… VAST_BUCKET=rate make compat-vast` against the lab cluster (`Server: vast
5.5.3.0`), tollbooth at b5b8bd1 with a default config in front of it, path-style addressing over
plaintext HTTP, inside an existing bucket under prefixes of the run's own.

| Stage | Result |
| --- | --- |
| Byte-equivalence (`test/s3diff`: the Go SDK's operation matrix, 108 cases with objects up to 64 MiB; the five checksum algorithms; 16 signing modes at four sizes each: unsigned payload, SHA-256 payload, streaming-signed with and without each of five trailers, unsigned with each of five trailers, presigned, SigV2 header and presigned; `Expect: 100-continue` accepted and refused) | **184 cases, 0 diffs**, 0 backend gaps: the cluster's answer through the proxy was the same as its answer direct in every case |
| Client matrix (aws cli, boto3, s5cmd, rclone, s3cmd, elbencho) | 48 cells: **43 pass, 0 failures**, 5 backend gaps |

A failure would be an operation that works direct and not through the proxy; there is none. The
five gaps failed the same way direct and through the proxy, so the proxy relayed them faithfully.
What they are:

| Cell | What happened, direct and through the proxy alike |
| --- | --- |
| aws cli `get-object-attributes` | the CLI could not parse the cluster's answer as XML; what came back was binary data |
| boto3 `get_object_attributes` | HTTP 500 from the cluster |
| boto3 `delete_objects` (five keys, not quiet) | 200, without the five `Deleted` entries the test expects in the answer |
| boto3 `get_object` with `PartNumber=2` on a six-part object | the bytes returned are not part 2's. The aws cli cells for the same request pass because they check only that the call succeeds |
| aws cli `put-object --checksum-algorithm CRC64NVME` | not the cluster: the aws cli on the dev box (2.15.31) does not know the algorithm. boto3's checksum cell, which includes CRC64NVME, passes |

The first two attempts tested nothing and found two things. The key may not create buckets on
that cluster (`CreateBucket` is answered AccessDenied), so the matrix gained a mode that works
inside an existing bucket and a self-test, run before every matrix, that fails if any command
there could make or remove a bucket or name a path outside the run's prefixes
(`test/integration/selftest.sh`). And with a default config tollbooth marked the cluster down two
seconds after starting: VAST answers the health probe's unsigned `HEAD /` after 2.0 s, which was
the default timeout. The default is now 5 s (D-064); in the run recorded here the proxy's log has
no "marked down" line.

With that, the three conditions are met. What remains before the tag is Blake's: merge `phase-5`
into `main` and say so.

## Appendix: guardrail passes

### G1 Simplicity review

Whole phase, `phase-4` tag to `phase-5` at 18cacac, excluding `bench/results/`: 205 files, +11,459
/ −1,378 lines; Go: 51 files, +7,015 / −777, of which tests +2,393 / −303. `bench/results/` adds
recorded output only.

Exported identifiers added in `internal/` and `cmd/`: 32, all in `internal/config`, all with a
caller outside the package's tests. The rule model (`Rule`, `StringList`, `Number` and their YAML
methods; `Count`, `Rate`, `BurstSize`, `GroupBy`, `Mode`, `RespondStatus`, `Operations`, read by
`internal/limiter`'s compile), the read-back (`Sentence`, `Summary`, `LimitText`, `Strict`,
`Spacing`, `FormatCount`, `ModeStatus`, `ModeText`, read by the CLI, the admin view, and the
console), operation names (`ResolveOperation`, `OperationNames`, read by the form's picker and the
Limits screen), the file edits (`RulesIn`, `AddRule`, `ReplaceRule`, `DeleteRule`, `ErrLayout`,
`CheckRule`, called by the form), and `Flatten` (the CLI and the console, which each had a private
copy before). `internal/limiter`, `internal/proxy`, `internal/showback`, `internal/s3id`, and
`internal/admin` gained none. **Interfaces added: none.** Body buffering in new code under
`internal/`: none. No middleware, container, or plugin. Feature flags and kill switches: unchanged
from Phase 4.

Dependencies: `go.mod` is unchanged since the `phase-4` tag; the Config editor's textarea is part
of `charmbracelet/bubbles`, already a TUI-only dependency, and brought one indirect `go.sum` entry.
`benchstat` left `docs/deps.md` when `test/bench/compare` replaced it. Removed: the rule schema of
Phases 2 to 4 and its validation (replaced, not kept beside the new one), the two private copies of
the config-flattening function, `rate_limits.default_on_limit`.

Test-to-code ratio: 10,173 / 16,511 = 0.616 (0.654 at Gate 4), discussed in section 5. One binary
with the same subcommands; the console is still `tollbooth tui`.

Against "nothing in tollbooth writes the config file" (CLAUDE.md, Simplicity rules): the console
now does, on an explicit save, by ADR-0006 and ADR-0007. CLAUDE.md's sentence is the
specification's and lags the two accepted ADRs in the same way (section 2).

### G2 Validation

On the final build (cca1c4b), dev box, with a demo stack serving 100 req/s on the same two cores:

```
== lint exit 0 (15 s)           golangci-lint 0 issues; go vet clean
== vet exit 0 (1 s)
== race exit 0 (139 s)          10 packages ok under -race -count=1; coverage 89.8%
== fuzz exit 0 (389 s)          FuzzParse, FuzzEditRules (config), FuzzClassify, FuzzParse, FuzzScanQuery (s3id), FuzzRead (showback), 60 s each, 0 crashers
== s3diff-record exit 0 (5 s)   293 cases, 0 diffs, 0 backend gaps
== bench exit 0 (192 s)         144 benchmark lines with -benchmem; 0 allocations on Decide and Record, 7 on SmallGET
== vuln exit 0 (11 s)           No vulnerabilities found.
== licenses exit 0 (5 s)        THIRD_PARTY_NOTICES.md regenerated, no change
== build exit 0 (5 s)
== tidy exit 0 (0 s)
done 2026-11-10T18:54:45Z
```

Then the soak smoke on the same build: 0.11 h, 246,314 requests, 12 fault steps, 0 unexplained
errors (`bench/results/soak-2026-11-10T185447Z/soak-report.txt`).

CI run 42872079961 (PR #35 at 1ec0a95, the tree merged as 18cacac): `unit` (make all, tidy, fuzz
60 s), `audit`, `codeql`, `semgrep`, `release` (make release, unit verify, make image, dist
artifact), `integration` (s3diff against Garage and versitygw, client matrix, throttle rows): all
success. The PR's first push (run 42867295463 at 90d05ad) failed `unit` (the fuzz crasher) and
`integration` (the throttle stage read a field the PR had renamed); both fixed in the PR, both in
section 5. Earlier in the phase: run 42674520784 (PR #24), green; nightly 42673827885 on
`p5/release`: `bench-regression` success, `fuzz-1h` success (five targets at 720 s each, 0
crashers). No nightly has run on the final build yet (category 10).

### G3 Telemetry and docs

One metric added this phase: `tollbooth_limiter_matched_total{rule}`, in
`docs/telemetry-catalog.md` before it was implemented (22 registered metrics; the catalog and
cardinality tests pass in the race suite; the `rule` label is bounded by the config, as for
`limiter_denied_total`). The console reads requests per second by S3 operation from the existing
`tollbooth_requests_total`; no route was added. `docs/CONFIG.md`: the `limits` section rewritten
key by key, including the two named burst settings; unknown keys are still errors and the old
schema's keys are answered with the new ones. `docs/OPERATIONS.md`: section 7 is "add or change a
limit", from the console's form and by hand; section 9 says what per-minute and per-hour limits do
to a client in both burst settings (measured); section 10 has the resolver row.
`docs/OPERATOR_SURFACE.md`: the form's keys and what writes the file. `demo/DEMO.md`: every output
re-captured from a stack built from the final rule schema. `CHANGELOG.md`: an Added entry and a
Changed entry that says the schema was replaced. `docs/STATUS.md`: the Phase 5 record. ADRs: 0005
proposed; 0006 and 0007 accepted, 0007 with an as-built table. The specification lags in the four
places listed in section 2.

### G4 Dependency and license audit

`govulncheck`: no vulnerabilities. `go-licenses` with the copyleft deny list: clean;
`THIRD_PARTY_NOTICES.md` regenerated with no diff. No direct dependency added or removed in the
phase (`go.mod` unchanged since `phase-4`); `docs/deps.md` updated for the textarea's use and
benchstat's removal. No code lifted this phase: the chart rasterizer, the editor, the form, and the
file edits are written here on the standard library and the charm components; the two projects
Blake pointed at for the console (nfsview, vast-bucket-policy-manager) were read for behavior and
palette, and no file was copied from them. No code from MinIO, Garage, or warp: elbencho (GPL-3.0)
and Garage (AGPL-3.0) run as processes only (`test/integration/clients.sh`, `backends.sh`). YAML is
`go.yaml.in/yaml`, including transitively (`make build` output links no `gopkg.in/yaml`). The
container image's base is `gcr.io/distroless/static-debian12:nonroot` (Apache-2.0); the build stage
is `golang:1.27.3-bookworm`; neither ships code into the binary.
