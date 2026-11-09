# Gate 3 report: Phase 3, Showback

Written 2026-11-09 on branch `phase-3` (PR #18, `p3/showback`). Dev box: RHEL 9, 2 vCPUs (AMD EPYC
7402P), 7.5 GiB, Go 1.27.3. CI: ubuntu-latest, 4 vCPUs. Every number below has a command in
section 7.

## 1. Scope delivered

| Planned item (docs/SPEC.md, Phase 3 row) | Status | Where |
| --- | --- | --- |
| Aggregator (struct-keyed tuples, 256 locked shards, counters under the lock, hour read inside `Record`) | done | #18 `internal/showback/agg.go` |
| Snapshot and seal (temp file, fsync, rename every `flush_interval`; seal at the hour boundary; zero-row file for an idle hour; sealed files never overwritten) | done | #18 `store.go` |
| NDJSON files with headers (schema version 1 as documented) | done, `docs/SHOWBACK_SCHEMA.md` unchanged | #18 |
| `tollbooth usage` verbs (`query`, `top`, `verify`) and `GET /admin/usage` | done, same `Query` and `Verify` functions | #18 `cmd/tollbooth/usage.go`, `internal/admin/server.go` |
| Dimensions and overflow cap (`__overflow__` row per operation and status class) | done | #18 |
| Restart from snapshot (same hour loaded with no double count; earlier hour sealed as `recovered`) | done | #18 `Store.Recover` |
| Multi-node sum | done: `Query` over files of many nodes; `TestMultiNodeSum` | #18 |
| Not in the row, delivered | `FuzzRead`; retention sweep; showback metrics (five, twenty-one total); drain takes the final snapshot; the on/off cost A/B; the gated 250,000-tuple measurement | #18 |
| Deferred | `doctor`'s tmpfs warning for `showback.dir` (Phase 4, with `doctor`); `OPERATIONS.md` chargeback-export walkthrough (Phase 5) | |

## 2. Decisions and assumptions

No ADR this phase. `docs/DECISIONS.md` gained D-046 to D-049:

| Id | One line |
| --- | --- |
| D-046 | `showback.dimensions` and `showback.prefixes` are restart-only (the row shape and prefix tables are built once) |
| D-047 | The hour is the completion hour read inside `Record`; the specification's test-plan row 8 ("lands in hour 10") contradicts its own column definition and the Gate 0 correction, and `TestHourIsTheCompletionHour` pins hour 11. Proposed edit flagged below |
| D-048 | `showback.Status` mirrors `admin.StatusClass` value for value (import direction); `TestShowbackStatusMirrorsStatusClass` pins it |
| D-049 | The writer seals every table that was ever open exactly once from a list the aggregator hands it; a sealed file is never overwritten (`.dup` beside it, logged); the writer wakes every flush interval and half a second after each hour boundary |

Specification text proposed for Fable to amend: test plan row 8, "A request starting at
10:59:59.9 and ending at 11:00:00.1 lands in hour **11**" (D-047).

## 3. Test results

| # | Category | Where | Result |
| --- | --- | --- | --- |
| 1, 2 | Classifier, identity | `internal/s3id` | unchanged; pass |
| 3 | Limiter | `internal/limiter` | unchanged; pass |
| 4 | Fuzzing | the four Phase 1 targets plus `FuzzRead` (NDJSON reader, the parser the spec names) | 5 targets × 60 s on the dev box, no crashers (`FuzzRead` seeded with a real snapshot, an empty file, a header-only file, and a row with an IPv6 source); CI runs 60 s per target on every PR and 1 CPU-hour nightly |
| 5 | Byte-equivalence | `test/s3diff -record` every PR; Garage and versitygw in CI | record mode 293 cases, 0 diffs; CI run 42651798110: Garage 293 cases, 0 diffs, 28 backend gaps; versitygw 293 cases, 0 diffs, 8 gaps |
| 6 | Client compatibility | CI `integration` | 122 cells, 0 failures, 2 backend gaps (unchanged) |
| 7 | Throttling behavior | `test/e2e`, `docs/THROTTLE.md` | e2e pass; THROTTLE rows unchanged in substance (aws cli default 80/200, boto3 default 192/200, adaptive and the other tools 200/200) |
| 8 | Showback correctness | `internal/showback`, injected clock: completion hour (D-047); rollover seals exactly and an idle hour still gets a file; restart with no double count and a kill between snapshots losing exactly the unflushed requests; an earlier hour's snapshot sealed as recovered; overflow fold; clock step back never reopens a sealed hour; zero allocations; prefixes (ADR-0001); encode/read round trip; query (filter, group, sum, partial, absent dimension fails loudly); multi-node sum; verify detects a removed hour, a tampered header, an unreadable file; sealed never overwritten and retention; flush error keeps counting; 32 writers across a seal. `test/e2e`: reconciliation exact (puts, gets, lists, throttled, aborted download) and the on/off cost A/B | 19 tests + `FuzzRead` + 3 benchmarks + 1 gated measurement: pass, 0 fail, 0 skip (gated test run with `TOLLBOOTH_BIG=1`); package coverage 86.6%; e2e 2 pass |
| 9 | Chaos, Phase 3 rows | showback directory unwritable (`TestFlushErrorKeepsCounting`), clock step back (`TestClockStepBack`), tuple flood past the cap (`TestOverflowFolds`), SIGTERM/drain takes the final snapshot (`TestServeShowbackSnapshotAndDrain`) | pass |
| 10 | Performance regression | `make bench-compare` against the Gate 2 baseline (runner) | benchstat lists runner (`-4`) and dev-box (`-2`) rows side by side without deltas (different `GOMAXPROCS` suffixes); allocation counts identical everywhere; the new showback benchmarks have no baseline row yet and join the baseline at Gate 3 when the nightly runs on the merged branch |
| 11 | Static analysis | CI on every PR | lint 0 issues, vet clean, govulncheck "No vulnerabilities found" (v1.9.0), go-licenses clean (no new module), CodeQL and Semgrep clean on #18 |
| 12 | Operator surface | Phase 4; `docs/OPERATOR_SURFACE.md` updated for `/admin/usage` and the `usage` verbs | skipped: not this phase |

Race suite: `make race` clean. Coverage over `internal/` and `cmd/`: **92.3%** (floor 85%; Gate 2
was 94.5%; the drop is the new query and verify code, covered at 86.6% in its package).

Allocation pins: forwarded 4 KiB GetObject with showback on 7.00 (budget 7); `Record` on an existing
tuple 0.

## 4. Performance

| Measure (docs/SPEC.md, Performance targets) | Target | Measured | Environment |
| --- | --- | --- | --- |
| Showback increment | ≤ 2 µs p99; ≤ 2% throughput cost on versus off | **`Record` 198 ns, 0 allocations** (existing tuple; `BenchmarkRecord`); parallel 140 ns; the in-process on/off A/B through httptest on 2 vCPUs is within run-to-run noise (one run: off 4,007 req/s, on 4,372 req/s, i.e. −9.1% "cost"; the pass line is 10% slower, not 2%, and both numbers are logged), so the 2% claim is checked on the reference node in Phase 5 | dev box |
| Memory per showback tuple | spec expects 350–450 B; ≤ 1 GiB RSS at 1M keys + 250k tuples | **257 B per tuple** at 250,000 tuples (61 MiB heap); with the limiter's 226 B per key the two stores at their caps are about 290 MiB | dev box, `TOLLBOOTH_BIG=1` |
| Snapshot of the open hour at the tuple cap | (no target; every `flush_interval`, writer goroutine) | 250,000 tuples: **1.09 s** (copy under the shard locks, sort, encode, fsync, rename), 49 MiB file; 10,000 tuples: 24 ms | dev box |
| Reload under load, startup to ready, limiter decision | Phase 2 | unchanged | |
| Added latency, throughput, TLS, streaming | Phase 5 | not measured this phase | |

The 1.09 s snapshot at the cap is the writer's own time; requests are not blocked (each shard lock
is held for the copy of that shard only). On the reference node it will be faster, and the sort can
go if it costs: it exists so a snapshot's row order is deterministic for diffing.

## 5. Known gaps and risks

| Gap or risk | Proposed fix | Phase |
| --- | --- | --- |
| The specification's test-plan row 8 says hour 10 where its column definition, D-025, and the code say hour 11 (D-047) | Fable amends the row | now |
| A snapshot at the 250,000-tuple cap takes about 1 s on the dev box, during which the shard locks are taken one at a time; a request landing on a locked shard waits for that shard's copy (~1,000 tuples, microseconds) | Acceptable; measure on the reference node; drop the sort if it shows | 5 |
| The showback on/off cost could not be measured to 2% on 2 shared vCPUs | Reference-node A/B | 5 |
| `doctor` does not yet warn about `showback.dir` on tmpfs | `doctor` is a Phase 4 verb | 4 |
| Query reads every file in range fully; a year of hourly files from many nodes is a few GB and a `usage query` over all of it is seconds to minutes | Documented: anything beyond filter, group, and sum is DuckDB; `--from`/`--to` prune by file name before opening | — |
| D-034 and the Gate 2 gaps carried | unchanged | 5, 6 |

## 6. Questions

1. **D-047**: amend the specification's test-plan row 8 to "lands in hour 11"? Recommended: **yes**.
2. **D-046**: `showback.dimensions` and `showback.prefixes` restart-only, like `showback.dir`? Recommended: **yes**.
3. The spec's query interface asks for `--format table|csv|json` on `usage query` and `format=` on `/admin/usage`; the admin route serves `json` and `csv` only (a table on an API is not useful). Fine? Recommended: **yes**.
4. Phase 4 (Operator surface) may begin on `phase-4` once #18 is merged into `phase-3`? Recommended: **yes**.

## 7. Reproduce

```
make all                                     # build, lint, test, race (85% floor), s3diff record mode
make fuzz FUZZ_TIME=60s                      # five targets including FuzzRead
go test -race -v ./internal/showback/        # category 8 with the injected clock
TOLLBOOTH_BIG=1 go test -run TestTupleMemoryAtCap -v ./internal/showback/
go test -run '^$' -bench . -benchmem ./internal/showback/
go test -v -run 'TestShowback' ./test/e2e/   # reconciliation and the on/off A/B
go test -v -run 'TestServeShowbackSnapshotAndDrain|TestHandlerAllocationsWithShowback' ./cmd/tollbooth/ ./internal/proxy/
make compat                                  # CI runs this
make bench-compare && make vuln && make licenses
```

## Appendix: guardrail passes

### G1 Simplicity review

| Identifier | Callers |
| --- | --- |
| `showback.NewAggregator`, `Aggregator.Record`, `Prefix`, `Tuples`, `Hour`, `Dimensions` | `cmd/tollbooth/serve.go`, `internal/proxy/handler.go`, tests; `Dimensions` and `Prefix` are read by the tests and `Prefix` by `Record` |
| `showback.NewStore`, `Store.Recover`, `Run`, `Flush` | `serve.go`, tests |
| `showback.Read`, `ReadFile`, `Files`, `Query`, `Result`, `Group`, `Verify`, `VerifyReport`, `NodeReport`, `ParseWhere`, `CSV`, `Dimensions` | `internal/admin/server.go`, `cmd/tollbooth/usage.go`, tests |
| `showback.Header`, `Row` (`Requests`, `BytesIn`, `BytesOut`, `Dimension`), `Status`, `ParseStatus`, `Overflow`, `SchemaVersion`, `CurrentPath`, `SealedPath`, `HourString`, `Metrics` | `internal/admin`, `internal/proxy`, `cmd/tollbooth`, tests |
| `admin.Usage` | `serve.go` |
| `proxy.New` (signature gained the aggregator) | `serve.go`, `test/s3diff`, tests |

Removed before the PR: nothing had a single caller except `Store.String` (kept: `fmt` verb for
logs). Dependencies added: none (`encoding/json`, `bufio`, `hash/maphash`, `os`, `path/filepath`
from the standard library). Body buffering: none; the NDJSON reader is a line scanner bounded at
1 MiB per line and `Query` holds one file's rows at a time. Feature flags: none; kill switch
`showback_disable` default false, tested. LOC (`phase-3..p3/showback`): 29 files, +3,169 / −65; code (non-test Go) 6,709 → 8,472, tests 5,778 → 7,186; test-to-code ratio 0.86 → 0.85, the one-point drop being the query, verify, and CLI rendering code, which is covered at 86.6% in its package but with fewer test lines per line than the GCRA store.

### G2 Validation

Run on the dev box after the last code change (`run/g2-phase3.log`, full output there; summary
lines pasted):

```
== make lint            0 issues; go vet ./... clean                                                     lint exit 0, vet exit 0
== make race (earlier)  every package ok under -race -count=1; total coverage 92.3%                      race exit 0
== make fuzz 60s        FuzzParse (config), FuzzClassify, FuzzParse, FuzzScanQuery (s3id), FuzzRead (showback): no crashers   fuzz exit 0
== make s3diff-record   293 cases, 0 diffs, 0 backend gaps                                               s3diff-record exit 0
== make bench-compare   benchstat runner vs dev box, no deltas computable; allocs equal                   exit 0
== make vuln            No vulnerabilities found.                                                         vuln exit 0
== make licenses        THIRD_PARTY_NOTICES.md regenerated, no change                                    licenses exit 0
== TOLLBOOTH_BIG=1      250,000 tuples: 257 bytes per tuple; snapshot 961 ms; file 49.1 MiB
```

Garage and versitygw s3diff, the client matrix, and the throttle rows ran in CI on #18 (section
3). No test was weakened; the two test changes during the phase were wrong expectations of mine
(a row's sort position, and the backend's count including the test's own measurement request).

### G3 Telemetry and docs

New paths and what observes them: the increment (`tollbooth_showback_tuples`, and every row is in
the file), the fold (`tollbooth_showback_collapsed_total`, one log line per hour), a stepped-back
clock (`tollbooth_showback_clock_skew_total`), snapshot and seal (`tollbooth_showback_flush_duration_seconds`,
`tollbooth_showback_flush_errors_total`, a log line once per minute on failure, `info` lines on
recovery at start), the files themselves (`usage verify`). `TestEveryMetricIsInTheCatalog` (21)
and `TestLabelSetsMatchTheCatalog` pass. Docs updated: `docs/CONFIG.md` (restart-only keys,
prefixes rule), `docs/DESIGN.md` §6 and §12, `docs/OPERATOR_SURFACE.md`, `docs/STATUS.md`,
`docs/DECISIONS.md`. Contradiction with the specification: test plan row 8 (D-047, proposed edit).

### G4 Dependency and license audit

No module added or changed (`go.mod` and `go.sum` untouched this phase; the showback package uses
`encoding/json`, `bufio`, `hash/maphash`, `os`, `path/filepath`, `time`). `make licenses`
(go-licenses v2.0.1, deny list GPL/LGPL/AGPL/SSPL) clean, `THIRD_PARTY_NOTICES.md` regenerated
and unchanged. `make vuln`: no vulnerabilities. No code lifted this phase; the Phase 1 lifts from
shunt are unchanged and listed in `docs/deps.md`. No code from MinIO, Garage, warp, or elbencho.
YAML remains `go.yaml.in/yaml` (v4 direct, v2 transitively through `prometheus/common`).
