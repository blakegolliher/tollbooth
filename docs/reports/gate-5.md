# Gate 5 report: Phase 5, Hardening and v0.1.0

Written 2026-11-09 on `p5/release` (PR #24 → `phase-5`), with the 24 h soak **still running**
(started 2026-11-09T18:40Z, ends 2026-11-10T18:40Z); section 3's soak row and section 4's memory
row are filled in from `bench/results/soak-2026-11-09T183931Z/soak-report.txt` when it ends. Every
other number below is final. Fable grades; Blake decides; v0.1.0 is tagged only on Blake's word.

## 1. Scope delivered

| Planned item (docs/SPEC.md, Phase 5 row) | Status | Where |
| --- | --- | --- |
| Full bench runs with profiles | done on the dev box (2 vCPUs), **not on the reference node** (ADR-0005) | `bench/results/2026-11-09T181541Z/` (raw, sdk mix, throttle; CPU and allocation profiles), commit 8d2a209 |
| Tuning | none applied: the profile is kernel-bound (46.6% `Syscall6`, 35.8% under the per-chunk `Flush`, the proxy's own code under 3%); `GOMEMLIMIT=1GiB` is set by the unit and the image | ADR-0005 §Context |
| 24 h soak at 50% of peak with chaos injections | harness done and smoke-tested (6 min, 11 chaos steps, 0 unexplained); **24 h run in progress** at 804 req/s (half of the 1,608 req/s calibrated peak at concurrency 16) | `bench/soak.sh`, `make soak`, D-056; `bench/results/soak-2026-11-09T183931Z/` |
| Resource caps | done: `MemoryMax=1536M`, `GOMEMLIMIT=1GiB`, `LimitNOFILE=65536`, `TasksMax=16384` in the unit; `listen.max_inflight`, the limiter key cap, and the showback tuple cap in config (Phases 2 to 4) | `deploy/tollbooth.service`, `docs/OPERATIONS.md` §5 |
| `docs/OPERATIONS.md` runbook | done: install (tarball, systemd, container), configure, reload, upgrade without dropping connections, sizing, metric meanings, two tasks, retry modes, failure behavior, performance expectations | commit e33bd77 |
| systemd unit | done; verified by `systemd-analyze verify` in CI against the built binary (no `\|\| true`) | `deploy/tollbooth.service`, CI job `release` |
| Container image | done; built by `make image` in CI (distroless static, nonroot); the dev box has no docker | `deploy/Dockerfile`, `.dockerignore`, CI job `release` |
| `CHANGELOG.md` | done (`[Unreleased]` and an empty `[0.1.0]` awaiting the tag) | `CHANGELOG.md` |
| `make release` | done: linux/amd64 and linux/arm64 tarballs, `SHA256SUMS`, version check; artifacts uploaded from CI | `Makefile`, CI job `release` |
| Tagged release | **pending**: ADR-0005 (what v0.1.0 claims) is Blake's decision; the tag follows the soak and that decision | ADR-0005 |
| Not in the row, delivered | null backend `-control` fault injection; `loadgen -rate`; `bench/crosscheck.sh` (elbencho, ADR-0005 amending ADR-0002); CI `release` job | 849054e, 99d3494, c629b07, 65cb517 |

## 2. Decisions and assumptions

- [ADR-0005](../adr/0005-performance-targets-at-v0.1.0.md), **proposed**: (1) what v0.1.0 claims,
  recommendation (b): tag with the five hardware-independent rows met on the dev box and the four
  end-to-end rows marked unverified until a reference-node run; (2) stay on Go, reopened only by a
  reference-node profile that shows the runtime rather than the kernel at the top; (3) elbencho
  replaces warp as the external cross-check.
- D-056 (`docs/DECISIONS.md`): the soak's definitions: peak, 50%, the chaos rotation, "explained"
  and "unexplained" error events, "flat" memory; the script refuses taken ports and rebuilds the
  binary every run.
- No specification contradiction found this phase. The specification's Phase 5 row assumes the
  reference node exists; it does not yet (ADR-0005).

## 3. Test results

G2 ran on the dev box (`run/g2-phase5.sh`, outputs under `run/g2-phase5/`; summary lines
pasted in the appendix). CI on PR #24 at 8d2a209: run 42674520784, six jobs, all green
(`unit`, `audit`, `codeql`, `semgrep`, `release`, `integration`).

| Test plan category | Result | Where |
| --- | --- | --- |
| 1 Byte-equivalence | 293 cases, 0 diffs, 0 backend gaps (record mode, dev box); Garage and versitygw in CI (`integration`, green) | `run/g2-phase5/s3diff-record.log`; run 42674520784 |
| 2 Classifier | in the race suite; `FuzzClassify`, `FuzzParse`, `FuzzScanQuery` 60 s each, 0 crashers | `run/g2-phase5/race.log`, `fuzz.log` |
| 3 Limiter | in the race suite (fake clock); microbenchmarks in `run/g2-phase5/bench.log` (102 benchmark lines, `Decide100Rules` 639 to 1,336 ns/op across six counts on the 2 vCPU box, 0 allocations) | same |
| 4 Hot reload | in the race suite; the soak's `reload-valid` and `reload-invalid` steps every 80 minutes for 24 h (smoke: 200 and 422, running config kept) | `bench/results/soak-*/chaos.log` |
| 5 Showback | in the race suite; `FuzzRead` 60 s; the soak's `showback-ro` step (smoke: 1 flush error logged, counting continued, next snapshot succeeded) | same |
| 6 Client compatibility | 59 client rows PASS/PASS on Garage and versitygw (CI `integration`); `docs/COMPAT.md` unchanged | `docs/COMPAT.md` |
| 7 Client throttling | 7 rows, 5 completed in this CI run: aws cli default 75/200 (no), aws cli adaptive 200 (yes), boto3 default 194/200 (no), boto3 adaptive 200 (yes), s5cmd, rclone, s3cmd yes (D-044) | `docs/THROTTLE.md` (updated from run 42674520784) |
| 8 Failure injection | the soak smoke: backend 503, bounded stall, reset, process down, invalid and valid reload, read-only showback directory, SIGHUP; every error event attributed to its window, 0 unexplained | `run/soak-smoke` in the PR description; 24 h run pending |
| 9 Soak | **running**: 24 h at 804 req/s, chaos every 10 min, samples every 60 s | `bench/results/soak-2026-11-09T183931Z/` |
| 10 Performance regression | nightly 42673827885 on the fixed runner versus the committed baseline (run 42629583831): **13 benchmarks compared, 0 failed**, deltas −6.2% to +6.4% (`SmallGET` +6.4% is the largest), allocations identical (7 on `SmallGET`, 161 end to end, 0 on `Decide` and `Record`). Finding: benchstat had never compared the two files (§5); replaced by `test/bench/compare` (6093653), which fails the build past 10% or on any extra allocation | `run/compare-nightly.txt`; `test/bench/compare` |

Race: 9 packages ok under `-race -count=1`, 0 failures, coverage **90.2%** of statements (floor
85%). Fuzz: 5 targets × 60 s, 0 crashers. Skipped: none. Not run this phase: the SDK e2e suite
in `test/e2e` beyond CI's `unit` job (it ran there, green).

## 4. Performance

Environment for every row: dev box, AMD EPYC 7402P, **2 vCPUs**, 7.5 GiB, Linux 5.14.0-570, Go
1.27.3, `GOMAXPROCS` default, `GOMEMLIMIT` default for the bench (1GiB for the soak), three
processes sharing the two cores. Raw results, profiles, and the exact config:
`bench/results/2026-11-09T181541Z/` (`env.txt`, `tollbooth.yaml`, `raw-cpu.pprof`,
`raw-allocs.pprof`, the three JSON files). Method: 30 s warm-up, 30 s measured, three runs each,
direct and proxied interleaved, median reported.

| Row (docs/SPEC.md, Performance targets) | Target | Measured | Verdict |
| --- | --- | --- | --- |
| Added latency, 4 KiB GET, 100 rules, plaintext | p50 ≤ 150 µs, p99 ≤ 1 ms, p99.9 ≤ 5 ms | direct p50 3,174 µs / p99 21,840 µs; proxied p50 13,509 µs / p99 39,191 µs; added p50 10,334 µs, p99 17,351 µs, p99.9 25,788 µs at concurrency 32 | **unverified**: the direct run alone is 21× the p50 budget; two cores for three processes (ADR-0005) |
| Small-op throughput, plaintext, 100 rules, showback on | ≥ 50,000 req/s | direct 7,065 req/s, proxied 2,243 req/s (raw); sdk mix direct 2,967, proxied 1,607 | **unverified**, same cause |
| Small-op throughput, TLS terminated | ≥ 30,000 req/s | not measured | **unverified** |
| Large-object streaming, 32 × 64 MiB | ≥ 80% of line rate | not measured (loopback, no NIC) | **unverified** |
| Limiter decision, 100 rules, 1,000,000 keys | ≤ 10 µs p99 | p99 4.7 µs (Gate 2, gated test); 639 to 1,336 ns/op microbenchmark this run, 0 allocations | met |
| Showback increment | ≤ 2 µs p99; ≤ 2% on/off | 198 ns, 0 allocations (Gate 3); on/off within noise on shared cores (D-055) | increment met; on/off **unverified** |
| Memory at 1,000,000 keys + 250,000 tuples | ≤ 1 GiB RSS | 226 B/key + 257 B/tuple ≈ 290 MiB for the stores (Gates 2, 3); soak RSS **pending** (smoke: 23 to 34 MiB at 700 req/s) | met for the stores; soak pending |
| Config reload under load | ≤ 100 ms, 0 failed | 0.60 ms, 0 failed (Gate 2); soak smoke reload 1.1 ms | met |
| Startup to ready | ≤ 1 s | 65 to 78 ms (Gate 2) | met |

Profile (`raw-cpu.pprof`, 10 s during the proxied raw run, 4.16 s of samples): `Syscall6` 46.6%
flat; `bufio.(*Writer).Flush` 35.8% cumulative (the per-chunk flush transparency requires, D-006);
`limiter.(*Limiter).evaluate` 2.6% cumulative; map hashing 2.4%. Allocation profile
(`raw-allocs.pprof`, objects): `net/textproto.readMIMEHeader` 17.8%, `persistConn.roundTrip` 8.8%,
`persistConn.readLoop` 7.0%, context creation 13.8% across three frames, `Header.Clone` 4.1%: all
in `net/http`'s client and server, none in the handler.

Throttle scenario (sdk mix against a 1 req/s rule): 16,432 attempts, 918 throttled, 0 errors,
p50 946 µs, p99 1.68 s (the SDK's backoff).

## 5. Known gaps and risks

| Gap | Proposed fix | Phase |
| --- | --- | --- |
| Four end-to-end performance rows unmeasured on the reference node | run `make bench-e2e` and `bench/crosscheck.sh` on the 16 vCPU node with pinned CPU sets; v0.1.1 gate (ADR-0005 option b) | 5 → v0.1.1 |
| `bench/crosscheck.sh` has not run anywhere yet (needs a real S3 backend for elbencho's writes; the dev box has no backend process outside CI) | run it in the nightly against Garage from `test/integration/backends.sh`, or on the reference node | 5 → v0.1.1 |
| Container image built in CI only; never run on a host with docker by this project | `docker run … version` runs in CI (`make image`); a host run is part of the reference-node session | v0.1.1 |
| The unit file is verified by `systemd-analyze`, not by a start on a real host | the reference-node session installs the tarball with the runbook §1 and runs the two tasks from it | v0.1.1 |
| `bench/nullbackend` and `bench/loadgen` have no unit tests (pre-existing; the null backend is exercised by `internal/proxy`'s tests, the generator by every bench and soak run) | a test for the control listener's parsing and for `-rate`'s pacing arithmetic | 6 |
| Test-to-code ratio moved from 0.654 to 0.649: 86 Go lines added this phase (bench tooling), 0 test lines | the row above | 6 |
| The soak's error attribution is by sample, so a step that ends just before a sample and the next that begins just after share a label (`a/b`) | acceptable at 60 s samples and 10 min spacing (steps never share a sample); recorded in D-056 | — |
| **The benchmark regression gate never enforced anything**: benchstat printed the baseline and the new run as two tables with no deltas (Gate 2 and 3: different `GOMAXPROCS` suffixes; nightly: a `go: downloading` line captured into `new.txt` made it a different configuration) and exited 0, so the specification's ±10% threshold and "an allocation on the decision path fails the build" were claims, not checks, from Phase 2 until today | fixed this phase (6093653): `test/bench/compare` (stdlib, tested) takes medians, compares same-name benchmarks, exits 1 past the threshold or on any allocs/op increase; `make bench-compare` and the nightly use it; benchstat dropped from `docs/deps.md` | 5 (done) |

## 6. Questions

1. ADR-0005 (1): tag v0.1.0 as option (b), with the four end-to-end rows marked unverified in
   `CHANGELOG.md` and `docs/OPERATIONS.md` §11? **Recommended: yes**, after the 24 h soak reports
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

## 7. Reproduce

```
git checkout p5/release                      # 40264aa or later
make release                                 # dist/*.tar.gz, SHA256SUMS, version line
make build && mkdir -p run/unit && sed "s#/usr/local/bin/tollbooth#$PWD/bin/tollbooth#g" deploy/tollbooth.service > run/unit/tollbooth.service && systemd-analyze verify --man=no run/unit/tollbooth.service
make image                                   # where docker exists
run/g2-phase5.sh                             # lint, vet, race, fuzz 60 s, s3diff-record, bench, vuln, licenses, build, tidy → run/g2-phase5/
make bench-e2e                               # bench/results/<date>/ with profiles (section 4)
SOAK_HOURS=0.1 CHAOS_EVERY=15s SAMPLE_SECS=10 CALIBRATE=5s CONCURRENCY=8 make soak   # the smoke
make soak                                    # the 24 h run (SOAK_HOURS=24 CHAOS_EVERY=10m SAMPLE_SECS=60 CONCURRENCY=16)
gh run download 42674520784 -n compat-logs   # docs/COMPAT.md and docs/THROTTLE.md as CI produced them
gh workflow run nightly.yml --ref p5/release # the fixed-runner benchmark comparison (category 10)
```

## Appendix: guardrail passes

### G1 Simplicity review

Exported identifiers added this phase: one, `nullbackend.Server.StallFor` (`atomic.Int64`), read by
`ServeHTTP` and set by the control listener in `cmd/nullbackend` (second caller). `test/bench/compare`
is a `main` package with unexported functions and its own tests. No new types, interfaces, or
functions in `internal/` or `cmd/tollbooth`. Dependencies added: none (`go.mod`,
`go.sum`, `docs/deps.md` unchanged; `make tidy` clean). Body buffering in new code: none
(`bytes.Buffer`, `io.ReadAll` absent from the diff). Feature flags and kill switches: unchanged
from Phase 4 (showback kill switch defaults to on). LOC this phase excluding `bench/results/`:
+893 / −29 over 18 files (86 Go lines, the rest shell, YAML, Dockerfile, unit, Markdown);
`bench/results/` adds 6,942 lines of recorded output; `test/bench/compare` adds 190 lines of code
and 95 of tests after that count. Test-to-code ratio 8,083 / 12,446 = 0.649 before the compare
command (was 0.654): the Go added earlier in the phase is bench tooling with no unit tests, listed in
section 5. Deletions
made: none needed.

### G2 Validation

```
== lint exit 0 (4 s)            golangci-lint 0 issues; go vet clean
== vet exit 0 (0 s)
== race exit 0 (84 s)           9 packages ok under -race -count=1; coverage 90.2%
== fuzz exit 0 (320 s)          FuzzParse (config), FuzzClassify, FuzzParse, FuzzScanQuery (s3id), FuzzRead (showback), 60 s each, 0 crashers
== s3diff-record exit 0 (4 s)   293 cases, 0 diffs, 0 backend gaps
== bench exit 0 (133 s)         102 benchmark lines with -benchmem; 0 allocations on Decide and Record
== vuln exit 0 (8 s)            No vulnerabilities found.
== licenses exit 0 (4 s)        THIRD_PARTY_NOTICES.md regenerated, no change
== build exit 0 (3 s)
== tidy exit 0 (0 s)
done 2026-11-09T18:38:27Z
```

CI run 42674520784 (PR #24 at 8d2a209): `unit` (make all, tidy, fuzz 60 s), `audit`, `codeql`,
`semgrep`, `release` (make release, unit verify, make image, dist artifact), `integration` (s3diff
against Garage and versitygw, client matrix, throttle rows): all success. The first run of the PR
(42672430358) failed in 51 s with GitHub's "workflow file issue" and no jobs; its re-run started
four of six jobs and reported failure; the next push ran all six green. Nightly 42673827885
on `p5/release`: `bench-regression` success (the comparison above, recomputed with `test/bench/compare`
from the job log into `run/compare-nightly.txt`); `fuzz-1h` **pending** (one hour per target).

### G3 Telemetry and docs

New code paths this phase are in the bench tooling and deployment, not the proxy: the null
backend's faults are observed by the proxy's existing `tollbooth_requests_total{status_class}`,
`tollbooth_backend_errors_total{kind}`, and `tollbooth_showback_flush_errors_total`, which is what
the soak samples; no new metric, so no catalog change (`docs/telemetry-catalog.md` still 21
metrics; the catalog and cardinality tests pass in the race suite). `docs/CONFIG.md`: no key added.
`docs/OPERATIONS.md`: the full runbook. `docs/STATUS.md`: Phase 5 in progress; the record section
follows the soak. ADRs: ADR-0005 added; DESIGN §13 points the Go-versus-Rust row at it. No
divergence from the specification.

### G4 Dependency and license audit

`govulncheck`: no vulnerabilities. `go-licenses` with the copyleft deny list: clean;
`THIRD_PARTY_NOTICES.md` regenerated with no diff. No dependency added or removed; every entry in
`docs/deps.md` unchanged (shunt lifts with origin, commit, and license; charm, teatest, HdrHistogram,
client_model, smithy-go rows). No code from MinIO, Garage, or warp: elbencho (GPL-3.0) and Garage
(AGPL-3.0) run as processes only (`test/integration/clients.sh`, `backends.sh`). YAML is
`go.yaml.in/yaml`, including transitively (`make build` output links no `gopkg.in/yaml`). The
container image's base is `gcr.io/distroless/static-debian12:nonroot` (Apache-2.0); the build stage
is `golang:1.27.3-bookworm`; neither ships code into the binary.
