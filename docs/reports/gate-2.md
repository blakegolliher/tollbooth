# Gate 2 report: Phase 2, Rate limiting

Written 2026-11-09 on branch `phase-2` (PRs #10 to #14, stacked). Dev box: RHEL 9, 2 vCPUs (AMD
EPYC 7402P), 7.5 GiB, Go 1.27.3. CI: ubuntu-latest, 4 vCPUs. Every number below has a command
in section 7.

## 1. Scope delivered

| Planned item (docs/SPEC.md, Phase 2 row) | Status | Where |
| --- | --- | --- |
| Rule schema and loader | done (schema in `internal/config` since Phase 1; compile and bounds D-037) | #10 |
| Matchers (exact sets, longest prefix, CIDR table, op bitset, per-class candidate lists) | done | #10 |
| GCRA (`T`, `tau = (burst − 1) × T`, monotonic `TAT`, `rate: 0` branch, node_count division) | done | #10 |
| Single-pass evaluation (config order, first denial decides, earlier charges stay) | done | #10 |
| `slowdown` (503) and `too_many_requests` (429) responses | done, plus `x-amz-retry-after` (D-043) | #11 |
| `Expect: 100-continue` behavior measured | done: zero body bytes, 503 in 0.27–1.7 ms, one connection per denied upload, 338 µs over a keep-alive denial; per tool in `docs/THROTTLE.md` | #11, #13 |
| Hot reload | done: one serialized path for `SIGHUP` and `POST /admin/reload`, rule set first then config pointer (D-036); 20 reloads under load, slowest 0.60 ms, 0 failed requests | #11 |
| Limiter metrics and `/admin/limits` | done: the six catalog rows registered; `/admin/limits`, `/admin/limits/top`, `/admin/limits/test` (D-039), `/admin/config`, `/admin/reload` | #11 |
| `rules list`, `rules show`, `rules test`, `rules top` | done, through the shared `admin.Client`, `--format table\|json\|csv` | #12 |
| Not in the row, delivered | bench harness of ADR-0002 (`bench/loadgen`, `bench/run.sh`, `make bench-e2e`), first regression baseline from the fixed runner, e2e SDK suite, client-tool throttling rows (`docs/THROTTLE.md`), 1,000,000-key and 60 s flood measurements | #13, #14 |
| Not in the row, deferred | `config show/reload/diff` verbs (Phase 4 per the specification; the routes exist); `OPERATIONS.md` client guidance (Phase 5) | |

## 2. Decisions and assumptions

No ADR was written this phase; ADR-0002's warp cross-check remains Blake's open decision for
Phase 5. `docs/DECISIONS.md` gained D-036 to D-044:

| Id | One line |
| --- | --- |
| D-036 | The compiled `RuleSet` is published by `Limiter.Apply` behind the limiter's own pointer, before the config swap, not inside the `Config` value (import direction; no single-implementation interface) |
| D-037 | Arithmetic bounds: non-zero `rate` in [1e-6, 1e9], `burst / rate ≤ 1e9 s`, `node_count ≤ 1024`, so no GCRA sum overflows int64 |
| D-038 | `rules test` continues past the first denial to show every matching rule's verdict; the real pass stops |
| D-039 | `POST /admin/limits/test` served from Phase 2 (`rules test` is a Phase 2 verb) |
| D-040 | `POST /admin/reload` is 200 published / 422 rejected with the result JSON; `GET /admin/config` carries generation, load time, last reload; the JSON view uses the file's key names via a YAML round trip |
| D-041 | The throttled path is 6 allocations, pinned and deterministic across build modes; request ids from `math/rand/v2` (opaque correlation ids) because `crypto/rand.Read`'s interface made the count differ between `go test` and `-race` |
| D-042 | Access keys never appear in logs at any level (tightens CLAUDE.md's "only at debug"); `rules top` and showback carry them. CodeQL flagged the Authorization-derived field in a log call |
| D-043 | `x-amz-retry-after: <ms>` next to `Retry-After: <s>`; aws-sdk-go-v2 honors it with `AWS_NEW_RETRIES_2026=true` (opt-in in v1.47.8), ignores both by default. Spec text edited, flagged below |
| D-044 | Category 7 recorded per client and retry mode; completion asserted only for modes that can guarantee it (see §5) |

Specification edits this phase, all flagged for veto: the enforcement-modes paragraph lists
`x-amz-retry-after` (D-043). Proposed, not yet edited: category 7's "completes with its default
retry settings" (D-044) and CLAUDE.md's "access keys only at debug" (D-042).

## 3. Test results

Commands are in section 7; counts are from the runs recorded under `run/` on the dev box and the
CI runs named below. "Pass" counts are test functions; subtests are noted.

| # | Category | Where | Result |
| --- | --- | --- | --- |
| 1 | Classifier | `internal/s3id` | unchanged from Gate 1; pass |
| 2 | Identity | `internal/s3id` | unchanged; pass |
| 3 | Limiter | `internal/limiter`: 25 tests with a fake clock (exactly burst; burst + rate × 10 over a closed 10 s window; rate 0 always denies; later denial charges earlier rule; eviction lossless; reload keeps state; 64 goroutines on one key; zero allocations on allow, deny, and dry run; key cap fails open; panic fails open; matchers; group_by; node_count; fractional rate; modes; top; status and explain; compile errors; class lists; out-of-range enums) + 6 benchmarks + 2 gated measurements | 25 pass, 0 fail, 0 skip (2 gated tests run with `TOLLBOOTH_BIG=1`); package coverage 96.2% |
| 4 | Fuzzing | `FuzzParse` ×2, `FuzzScanQuery`, `FuzzClassify`; no new parser this phase (rules compile from the validated config) | 4 targets × 60 s on the dev box (G2 run), no crashers; CI runs 60 s per target on every PR and 1 CPU-hour nightly; corpus unchanged |
| 5 | Byte-equivalence | `test/s3diff -record` every PR; Garage and versitygw in CI `integration` | CI run 42630160653: Garage 293 cases, 0 diffs, 28 backend gaps; versitygw 293 cases, 0 diffs, 8 gaps |
| 6 | Client compatibility | `make compat`, `docs/COMPAT.md` | 122 cells, 0 failures, 2 backend gaps (unchanged) |
| 7 | Throttling behavior | `test/e2e` (4 tests, 6 subtests) every PR; `test/integration/throttle.sh` in CI (7 rows, `docs/THROTTLE.md`) | e2e 4 pass; THROTTLE 7 rows, 5 completed: aws cli default 80/200 landed, boto3 default 190/200, aws cli adaptive 200/200, boto3 adaptive 200/200, s5cmd 200/200, rclone 200/200, s3cmd 40/40 (D-044) |
| 8 | Showback | Phase 3 | skipped: not this phase |
| 9 | Chaos and resilience, Phase 2 rows | invalid reload under load (`TestReloadUnderLoad`), key cap (`TestKeyCapFailsOpen`), limiter panic (`TestLimiterPanicFailsOpen`), 10,000 new keys/s for 60 s (`TestUniqueKeyFlood`, gated) | pass; flood: 60 s × 10,000 new keys/s with `max_keys` 100,000 and a 1 s sweep: peak live keys 20,000, 0 overflow decisions, 0 denied, heap 0.4 → 4.1 MiB, RSS 16 MiB, store empty at the end |
| 10 | Performance regression | `test/bench/baseline.txt` committed from nightly run 42629583831 (fixed runner); nightly compares at ±10% | baseline committed; dev-box `make bench-compare` against the CI baseline is not comparable (different CPU) and is reported in §4 for the mechanism only |
| 11 | Static analysis | CI on every PR | lint 0 issues, vet clean, govulncheck "No vulnerabilities found" (v1.9.0, G2 run), go-licenses clean, CodeQL clean after D-042, Semgrep clean after the two `nosemgrep` markers (D-041) |
| 12 | Operator surface | Phase 4 (parity test); `docs/OPERATOR_SURFACE.md` updated for the Phase 2 routes and verbs | skipped: not this phase |

Race suite: `make race` clean (every package, `-race -count=1`, 7 packages with tests plus `test/e2e`).
Coverage over `internal/` and `cmd/`: **94.5%** (floor 85%; Gate 1 was 92.1%).

Allocation pins: forwarded 4 KiB GetObject 7.00 with no rules and 7.00 with 100 matching rules
(budget 7); throttled GetObject 6.00 (pinned, D-041); limiter decision 0 (allow, deny, dry run).

## 4. Performance

Targets are for a 16 vCPU reference node (Phase 5). Everything here ran on the 2 vCPU dev box with
the generator, the null backend, and the proxy sharing those two cores, so the end-to-end rows
measure contention, not the proxy; they are recorded so Phase 5 has a before.

| Measure (docs/SPEC.md, Performance targets) | Target | Measured | Environment |
| --- | --- | --- | --- |
| Limiter decision, 100 rules, 1,000,000 live keys | ≤ 10 µs p99 (stretch ≤ 3 µs) | **p50 2.6 µs, p90 3.7 µs, p99 4.7 µs, p99.9 17.5 µs**; microbenchmark 100 rules, one matching: 980 ns dev box, 335 ns CI runner; all 100 matching: 25.8 µs dev box; GCRA alone 120 ns dev, 62 ns CI; 0 allocs | dev box, `TOLLBOOTH_BIG=1`; CI baseline |
| Memory per limiter key | ≤ 1 GiB RSS at 1M keys and 250k tuples (whole process) | **226 B per key**, 215 MiB heap at 1,000,000 keys; sweep of 1,000,000 keys 165 ms | dev box |
| Config reload under full load | ≤ 100 ms, zero failed requests | **slowest of 20 reloads 0.60 ms**, 2,278 client requests during the reloads, 0 failed | dev box, 4 keep-alive clients |
| Startup to ready | ≤ 1 s | **65 ms** (0 rules), **78 ms** (102 rules), exec to `/readyz` 200 | dev box |
| Added latency, 4 KiB GetObject, 100 rules, plaintext | p50 ≤ 150 µs, p99 ≤ 1 ms, p99.9 ≤ 5 ms | direct p50 808 µs / p99 7,959 µs; proxied p50 2,869 µs / p99 14,950 µs; **added p50 2,061 µs, p99 6,992 µs, p99.9 14,467 µs** (medians of 3 interleaved 20 s runs, concurrency 8). Not comparable to the target: three processes on two cores; the direct run alone is 5× the target's p50 budget | dev box, `make bench-e2e` |
| Small-op throughput, plaintext, 100 rules | ≥ 50,000 req/s | direct 6,284 req/s, proxied 2,194 req/s at concurrency 8 (same caveat; showback not yet built) | dev box |
| SDK mix (GET 70 / PUT 20 / HEAD 10), aws-sdk-go-v2 | (no target; the throttle-path harness) | direct 2,357 req/s, proxied 1,404 req/s; 0 errors | dev box |
| Throttle path, GET at 2× a 500 req/s rule, SDK standard retries | (harness check) | 10,640 attempts, 141 throttled, 0 client errors, p50 1.0 ms, p99.9 1.9 s (retry waits included) | dev box |
| Small-op throughput, TLS; large-object streaming; showback increment | Phase 5 / Phase 3 | not measured this phase | |

CPU profile of the proxy during the proxied raw run (`bench/results/2026-11-09-devbox-profile/raw-cpu.pprof`,
10 s, 4.01 s of samples): 44% in `Syscall6` (socket reads and writes; `deadlineWriter.Write`
and its per-chunk flush 27% cumulative, the backend `readLoop` 16%); `limiter.Decide` 1.25%,
`s3id.Parse` 0.75%, `Handler.build` 1.5%, `mallocgc` 6.5%. On this box the proxy's cost is the
two extra socket hops, not the decision. Allocation profile alongside (`raw-allocs.pprof`).

Raw results: `bench/results/2026-11-09-devbox/` (three scenarios, JSON per run, `env.txt`, the
exact config, `metrics.txt`, `limits.json`) and `bench/results/2026-11-09-devbox-profile/`
(the profiled raw run). `make bench-compare` on the dev box against the CI baseline:
benchstat prints the two machines side by side without deltas because the rows carry different `GOMAXPROCS` suffixes (`-2` dev box, `-4` runner); the dev box is 1.8–3× slower across the limiter rows (Decide100Rules 756 ns ± 23% vs 339 ns ± 2%, GCRA 105 ns vs 62 ns, SmallGET 4.95 µs vs 1.33 µs), allocation counts identical (0 limiter, 7 handler). The ±10% gate therefore lives in the nightly job on the runner, where the first comparable run is the night after this baseline merges

## 5. Known gaps and risks

| Gap or risk | Proposed fix | Phase |
| --- | --- | --- |
| **aws cli v2 and boto3 in their default retry mode do not complete a sync at twice the rule's rate** (80/200 and 190/200 landed; three to five attempts with random backoff, `Retry-After` ignored). Both complete in adaptive mode (`AWS_RETRY_MODE=adaptive`), as do s5cmd, rclone, and s3cmd with defaults. The Go SDK's default mode fails a sequential object under a 1 req/s rule with ~6% probability per throttled object; its 2026 behavior (`AWS_NEW_RETRIES_2026=true`, honors `x-amz-retry-after`) and adaptive mode complete deterministically (D-044) | Client guidance in `docs/OPERATIONS.md` (adaptive mode or more attempts under sustained throttling); hold-and-forward is the proxy-side remedy (ADR in Phase 6); amend the specification's category 7 wording to record per mode | 5, 6 |
| Herd retries: `Retry-After` names the time to the next single token, so N clients denied together retry together and N − 1 are denied again | Inherent to a stateless throttle; shaping (Phase 6) or jitter guidance to clients; no change to the GCRA | 6 |
| 100 rules all matching one request cost 25.8 µs per decision on the dev box (10 µs budget assumes a handful match) | A per-rule precomputed hash seed would cut the shard hash from 67 ns to ~20 ns per rule; `check-config` could warn when more than N rules have no match conditions. Measure on the reference node first | 5 |
| End-to-end latency and throughput could not be measured meaningfully on 2 shared vCPUs | Reference-node run with pinned CPU sets | 5 |
| Sweep holds each shard lock ~0.65 ms at 1,000,000 keys (DESIGN estimated tens of µs) | Acceptable (256 shards, every `sweep_interval`); if p99.9 shows it, sweep in slices | 5 if seen |
| The deny ring allocates two strings per denial until a key has lapped it once; a storm over many keys allocates 2 per denial | Bounded by the ring; acceptable | — |
| D-034 (backend answers and closes before reading the body) carried from Gate 1 | unchanged | 5 or 6 |
| `TestUniqueKeyFlood` and `TestDecisionLatencyAtOneMillionKeys` run only with `TOLLBOOTH_BIG=1` (60 s and ~10 s) | The nightly `bench-regression` job runs them after the benchmarks (added this phase) | done |

## 6. Questions

1. **D-043** `x-amz-retry-after` on throttle responses, next to `Retry-After`, with the
   specification edited to list it: keep? Recommended: **yes** (what aws-sdk-go-v2 is converging on;
   proxy-originated response only).
2. **D-044** Category 7 is recorded per client and retry mode; aws cli and boto3 default modes do
   not complete at 2× overload and the test asserts completion only for the modes that guarantee it.
   Accept, and amend the specification's category 7 wording? Recommended: **yes**, with the client
   guidance landing in `docs/OPERATIONS.md` at Phase 5 and hold-and-forward staying a Phase 6 ADR.
3. **D-042** Access keys never in logs at any level (CLAUDE.md says "only at debug"): accept and edit
   CLAUDE.md? Recommended: **yes**.
4. **D-039** `POST /admin/limits/test` served now rather than in Phase 4: fine? Recommended: **yes**.
5. The dev-box end-to-end rows are recorded as "not comparable"; is a reference-node run wanted
   before Phase 3, or at Phase 5 as planned? Recommended: **Phase 5** (nothing in Phase 3 depends on
   them).
6. Phase 3 (Showback) may begin on `phase-3` once #10 to #14 are merged? Recommended: **yes**.

## 7. Reproduce

From a clean checkout of `p2/bench` (or `phase-2` after the merges):

```
make all                                     # build, lint, test (incl. test/e2e), race with the 85% floor, s3diff record mode
make fuzz FUZZ_TIME=60s                      # four targets
go test -run '^$' -bench . -benchmem ./internal/limiter/            # microbenchmarks
TOLLBOOTH_BIG=1 go test -run 'TestDecisionLatencyAtOneMillionKeys|TestUniqueKeyFlood' -v ./internal/limiter/
go test -v -run 'Allocations|ReloadUnderLoad|ThrottledExpect' ./internal/proxy/ ./cmd/tollbooth/
go test -v ./test/e2e/                        # SDK throttling behavior
make compat                                  # Garage and versitygw: s3diff, client matrix (COMPAT.md), throttle rows (THROTTLE.md); CI runs this
DURATION=20s WARMUP=10s RUNS=3 CONCURRENCY=8 make bench-e2e        # bench/results/<date>/
go tool pprof -top bin/tollbooth bench/results/<date>/raw-cpu.pprof
make bench-compare                           # against test/bench/baseline.txt (meaningful on the fixed runner: nightly job)
make vuln && make licenses
```

Startup to ready: start `bin/nullbackend -listen 127.0.0.1:19000`, then time `bin/tollbooth serve
--config <file>` against `curl -sf http://<admin>/readyz` in a loop (the measurement in §4 used
`run/startup.yaml` and the 102-rule bench config).

## Appendix: guardrail passes

### G1 Simplicity review

Exported identifiers added this phase and their second caller:

| Identifier | Callers |
| --- | --- |
| `limiter.New`, `Apply`, `Decide`, `Now`, `Run`, `Keys`, `Explain`, `Top`, `Status` | `cmd/tollbooth/serve.go`, `internal/proxy/handler.go`, `internal/admin/server.go`, tests |
| `limiter.Decision`, `Mode`, `ModeSlowDown`, `ModeTooManyRequests`, `Rule` (`ID`) | `internal/proxy/handler.go` |
| `limiter.Metrics` | `internal/admin/metrics.go` |
| `limiter.RuleStatus`, `TopEntry`, `Explanation`, `RuleMatch` | `internal/admin/server.go`, `cmd/tollbooth/rules.go` |
| `limiter.Rules`, `RuleSet.Len` | **removed** (tests only) |
| `admin.Client`, `Error`, `ReloadResult`, `ConfigView`, `Limits`, `Top`, `TestRequest`, `TestResponse` | `cmd/tollbooth/rules.go`, `cmd/tollbooth/serve.go`, tests |
| `config.MinRate`, `MaxRate`, `MaxRefillSeconds`, `MaxNodeCount` | `validate.go`, tests |
| `config.Config.MarshalJSON` | `encoding/json` through `admin.ConfigView` |
| `proxy.New` (signature gained the limiter) | `serve.go`, `test/s3diff`, tests |

Dependencies added: `github.com/HdrHistogram/hdrhistogram-go` v1.4.0 (MIT, bench only; the
stdlib has no histogram with exact percentiles and lossless merge, and a hand-written one would be
~300 lines); `github.com/prometheus/client_model` became a direct test-only requirement (already
linked through `client_golang`). Body buffering: none added (`grep -n 'bytes.Buffer\|io.ReadAll'
internal/` finds only the admin request decoder, bounded at 64 KiB, and tests). Feature flags: none;
kill switches: `limiter_disable` default false, tested. LOC (origin/phase-1..p2/bench, excluding
generated docs and results): 55 files, +5,589 / −123; code (non-test Go in `internal/`, `cmd/`,
`bench/`) 4,283 → 6,709; tests 3,352 → 5,778; test-to-code ratio 0.78 → 0.86.

### G2 Validation

Run on the dev box after the last code change (`run/g2.log`, each command's full output is in
that file; summary lines pasted):

```
== make lint          0 issues; go vet ./... clean                                         lint exit 0, vet exit 0
== make race          every package ok under -race -count=1; total coverage 94.5%            race exit 0
== make fuzz 60s      FuzzParse (config), FuzzClassify, FuzzParse, FuzzScanQuery (s3id): no crashers   fuzz exit 0
== make s3diff-record 293 cases, 0 diffs, 0 backend gaps (requests and responses, in-process)   s3diff-record exit 0
== make bench-compare benchstat baseline (runner, -4) vs new (dev box, -2): no deltas computable (different suffixes); allocs equal   exit 0
== make vuln          No vulnerabilities found.                                               vuln exit 0
== make licenses      THIRD_PARTY_NOTICES.md regenerated, no change                           licenses exit 0
```

Garage and versitygw s3diff and the client matrix ran in CI (run 42630160653: 293 cases, 0 diffs
on each backend; 122 cells, 0 failures). Phase 2 e2e scenarios: `go test ./test/e2e/` 4 tests
pass (42 s); the throttle rows are in `docs/THROTTLE.md`. No test was changed to pass; the two
test changes this phase were corrections of wrong expectations (the throttled allocation count
measured 8 under `-race` before D-041 made it deterministic; the 1 req/s completion was asserted
for a retry mode that cannot guarantee it, D-044). Benchmark regression over 5%: none measurable
yet (first baseline this phase).

### G3 Telemetry and docs

Every new path and what observes it: the decision (`tollbooth_limiter_decision_seconds`,
`tollbooth_proxy_overhead_seconds`), denials (`tollbooth_limiter_denied_total{rule}`,
`tollbooth_requests_total{result="throttled",status_class="throttled"}`, the sampled debug line
with `rule`, `retry_after`, `request_id`, `bucket`, `source_ip`), key store (`tollbooth_limiter_keys`,
`tollbooth_limiter_keys_overflow_total`), limiter faults (`tollbooth_limiter_errors_total`),
reloads (`tollbooth_config_reload_total{result}`, the `config reloaded` / `config reload failed`
lines with `generation`, `rules`, `duration_ms`, `GET /admin/config`), the deny ring
(`GET /admin/limits/top`), per-rule counters (`GET /admin/limits`). `TestEveryMetricIsInTheCatalog`
(17 metrics) and `TestLabelSetsMatchTheCatalog` pass. `docs/CONFIG.md` (D-037 bounds, reload
semantics), `docs/STATUS.md`, `docs/OPERATOR_SURFACE.md`, `docs/telemetry-catalog.md` (logging
fields), `docs/DESIGN.md` §4.8, §5, §7.2, §9, §12 updated. Contradictions with the specification:
`x-amz-retry-after` added (D-043, edited and flagged); category 7 "completes with default retry
settings" does not hold for aws cli v2 and boto3 (D-044, proposed edit); CLAUDE.md "access keys
only at debug" tightened to never (D-042, proposed edit).

### G4 Dependency and license audit

`make licenses` (go-licenses v2.0.1 with the GPL/LGPL/AGPL/SSPL deny list) clean;
`THIRD_PARTY_NOTICES.md` regenerated and unchanged, because the one new module,
`github.com/HdrHistogram/hdrhistogram-go` v1.4.0 (MIT), is linked only into `bench/loadgen`, never
into `bin/tollbooth` (`make build` greps the binary for `HdrHistogram`; depguard forbids the import
outside `bench/`). `make vuln` (govulncheck v1.9.0): no vulnerabilities. `github.com/prometheus/client_model`
v0.6.3 (Apache-2.0) became a direct test-only requirement; it was already in the binary through
`client_golang` and in the notices. No code was lifted this phase (no new `Modified by` headers; the
Phase 1 lifts from shunt are unchanged and listed in `docs/deps.md`). No code from MinIO, Garage, warp,
or elbencho; they run only as processes in `test/integration`. YAML is `go.yaml.in/yaml/v4`
directly and `go.yaml.in/yaml/v2` transitively through `prometheus/common`; no `gopkg.in/yaml`
(`make build` check). `docs/deps.md` has a row for every module in `go.mod`.
