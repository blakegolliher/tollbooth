# Gate 1 report — Passthrough

Date: 2026-11-09. Branch `phase-1`, feature PRs merged in order: #2 (`internal/s3id`), #4
(`internal/config`), #5 (`internal/proxy`, `internal/admin`, `bench/nullbackend`), #6
(`cmd/tollbooth`); open at the time of writing: #7 (`test/s3diff`), #8 (client compatibility
matrix, this report). Every number below comes from a log under `run/` on the dev box or from a
CI artifact named in section 7.

## 1. Scope delivered

| Planned item (docs/SPEC.md, Phase 1) | Status | PR |
| --- | --- | --- |
| Transparent proxy lifted from shunt's passthrough pipeline: `Host` preserved, raw request-target via `URL.Opaque` (or `Path`/`RawPath` for a leading `//`), inbound header map reused, no header added, pooled `io.CopyBuffer`, every chunk flushed, per-chunk idle deadlines, retry on connect error before any byte, `ErrAbortHandler` on a short body | done | #5 |
| TLS listener (1.2 minimum, http/1.1 only, certificate hot reload) | done | #5 |
| PROXY protocol v1/v2 (required when enabled) | done | #5 |
| Trusted `X-Forwarded-For` (rightmost untrusted entry, trusted peers only) | done | #2, #5 |
| Identity extraction: access key (SigV4 header/presigned, SigV2 header/presigned), bucket and key in both styles, list prefix (ADR-0001), source IP, flags | done | #2 |
| Classifier from shunt's op table, extended to 113 operations with classes; `docs/OPERATIONS_TABLE.md` generated with a drift test | done | #2 |
| Backend pool with active (`HEAD /`, any status healthy) and passive health, round-robin skipping down endpoints | done | #5 |
| Admin `/healthz`, `/readyz`, `/metrics` with request metrics; pre-resolved handles; catalog and cardinality tests | done | #5 |
| Null backend and recording backend in `bench/` | done (one package, `bench/nullbackend`, with `Record`) | #5 |
| Config schema, loader, whole-file validation, defaults, `check-config`, reload compatibility | done | #4, #6 |
| `tollbooth serve`, `check-config`, `version`; SIGHUP reload; SIGTERM drain | done | #6 |
| Byte-equivalence harness (`test/s3diff`): SDK matrix, every signing mode, record mode | done | #7 |
| Client compatibility matrix and `docs/COMPAT.md` | done | #8 |
| CI: lint, unit with race and coverage floor, 60 s fuzz, govulncheck, go-licenses, CodeQL, Semgrep, integration against Garage and versitygw with cached pinned binaries; nightly 1 h fuzz and bench regression skeleton | done | #5–#8 |

## 2. Decisions and assumptions

No ADR was written this phase; nothing in "Decisions already made" was challenged. `docs/DECISIONS.md`
gained D-026 through D-035:

- D-026 raw query keys matched byte-wise (percent-encoded key names not recognized, as in S3).
- D-027 `docs/OPERATIONS_TABLE.md` generated; Phase 0 check moved to `docs/reports/`.
- D-028 non-origin-form request-targets reduced to origin-form with `FlagAbsolute`.
- D-029 `backend.*` and `proxy.copy_buffer_bytes` are restart-only.
- D-030 every no-response backend failure is `503 ServiceUnavailable` + `proxy_error`.
- D-031 the idle watchdog is armed only while body bytes are expected.
- D-032 record mode diffs requests as well as responses, in-process, on every PR.
- D-033 the test tool signs (SDK signer for SigV4 forms; own framings for chunked modes and SigV2); the proxy never does.
- D-034 known gap: a backend that answers and closes before reading the request body can make Go's transport surface the write error; 503 results; `Expect: 100-continue` avoids it. Pinned by `TestBackendAnswersBeforeReadingBody`.
- D-035 a client's explicit `Content-Length: 0` on a bodiless request is forwarded (aws cli v2 signs it; Go's transport would drop it). Found by the Garage client matrix; pinned by `TestExplicitZeroContentLengthIsPreserved`.

Gate 0 corrections carried in: flush every chunk (D-006, `TestTrickleResponseReachesClient`),
copy-buffer budget 4,096 × 128 KiB (D-024), hour read inside `Record` (D-025, Phase 3 code),
`TestHandlerAllocations` on a no-op writer and fake transport (D-023), deny ring 1,024 (Phase 2),
MinIO and `mc` out (D-022).

## 3. Test results

Commands (full logs under `run/` on the dev box; CI runs linked in section 7):

```
make lint                              run/lint-cov.log        0 issues
make race                              run/race-lint.log       all packages ok; coverage 92.1% over internal/ and cmd/ (floor 85%)
go test -race ./internal/proxy         10.5 s (was 307 s before the in-flight cap test timed its clients out)
go run ./test/s3diff -record           run/s3diff-record.log   285 cases, 0 diffs, 0 gaps (requests and responses, both styles, every signing mode)
fuzz (2 workers, 10 min each)          run/fuzz-1h/fuzz.log    FuzzParse, FuzzScanQuery (s3id), FuzzParse (config): PASS; 1 CPU-hour total
fuzz FuzzClassify 10 min               run/fuzz-1h/fuzz-classify.log  PASS (a further 20 CPU-minutes)
make compat (CI integration job)       CI artifact compat-logs  Garage: s3diff 293 cases, 0 diffs, 28 backend gaps; clients 60 cells, 0 failures, 2 backend gaps
                                                                versitygw: s3diff 293 cases, 0 diffs, 8 backend gaps; clients 60 cells, 0 failures, 0 backend gaps
                                                                docs/COMPAT.md: 122 cells, 0 failures, 2 backend gaps (committed from the artifact)
```

Per test-plan category:

| # | Category | Result |
| --- | --- | --- |
| 1 | Classifier unit tests | pass: every row, precedence (56 cases incl. the 18 additions and folds), names and classes, generated table drift, zero-allocation |
| 2 | Identity unit tests | pass: 24 style/encoding cases, 13 access-key cases, 12 source-IP/XFF cases, flags and list prefix, PROXY protocol v1 and v2 end to end |
| 3 | Limiter unit tests | skipped: Phase 2 |
| 4 | Fuzzing | pass: 4 targets, 1 CPU-hour + 20 CPU-minutes on the dev box; 60 s per target in CI; corpus committed (`internal/s3id/testdata/fuzz/FuzzParse`, 3 entries) |
| 5 | Byte-equivalence | record mode 285 cases, 0 diffs; Garage 293 cases, 0 diffs, 28 backend gaps (SigV2 header and presigned, and the five signed-chunk-trailer modes, refused identically direct and via); versitygw 293 cases, 0 diffs, 8 backend gaps (SigV2 header and presigned) |
| 6 | Client compatibility matrix | pass: aws cli v2.39.3, boto3 1.43.126, aws-sdk-go-v2 (s3diff), s5cmd 2.3.0, rclone 1.76.0, s3cmd 2.4.0, elbencho 3.3-3 against Garage 2.5.0 and versitygw 1.11.0: 122 cells, **0 failures**, 2 backend gaps (Garage answers `GetObjectAttributes` 500 direct and via). Each operation ran direct and via; `docs/COMPAT.md` is the generated matrix |
| 7 | Throttling behavior | skipped: Phase 2 |
| 8 | Showback correctness | skipped: Phase 3 |
| 9 | Chaos and resilience | pass for the Phase 1 rows: all backends down, one backend down (retry before bytes), backend reset mid-body (Content-Length and chunked), stalled backend (idle timeout), slow headers not idle, slowloris, 1 MiB headers, in-flight cap under concurrency, invalid reload, restart-only reload, SIGTERM drain, handler panic fails open, early backend answer (D-034) |
| 10 | Performance regression | skipped: baseline committed at Gate 2; microbenchmarks exist for classifier, identity, metrics observation, handler |
| 11 | Static analysis | pass: golangci-lint (0 issues), go vet, govulncheck (none), go-licenses (deny list clean), CodeQL, Semgrep, all in CI |
| 12 | Operator surface | partial by design: route table and the three Phase 1 routes tested; parity test, CLI goldens, and `teatest` are Phase 4 |

Coverage: 92.1% of statements over `internal/...` and `cmd/...` (`make race`, `-coverpkg`). Race: clean. Fuzz: 1 CPU-hour clean (dev box) plus 60 s per target on every CI run.

## 4. Performance

Not a Phase 1 gate. Numbers recorded for the Gate 2 baseline, dev box (2 vCPUs, AMD EPYC 7402P host, go1.27.3), `-benchmem`:

| Benchmark | Result |
| --- | --- |
| `BenchmarkClassify` (UploadPart) | 98 ns/op, 0 allocs |
| `BenchmarkClassifyGetObject` | 47 ns/op, 0 allocs |
| `BenchmarkParse` (identity, vhost PUT with SigV4 header) | 607 ns/op, 0 allocs |
| `BenchmarkParseGetObject100Rules` (path GET) | 310 ns/op, 0 allocs |
| `BenchmarkObserve` (metrics, pre-resolved) | 172 ns/op, 0 allocs |
| `BenchmarkSmallGET` (handler alone, no-op writer, fake transport, 4 KiB) | 5.1 µs/op, 7 allocs, 874 B |
| `TestHandlerAllocations` | **7.00** allocations per forwarded 4 KiB GetObject (budget 7, DESIGN §9) |
| `BenchmarkSmallGETEndToEnd` (client → proxy → null backend over loopback, 2 vCPUs) | 391 µs/op, 161 allocs (net/http client, server, and transport included; a 16-vCPU reference node is the Phase 5 measurement) |

## 5. Known gaps and risks

| Gap or risk | Proposed fix | Phase |
| --- | --- | --- |
| D-034: a backend that answers and closes before reading the request body can turn into a 503 for a client that does not send `Expect: 100-continue`. | A custom transport that prefers an early response over a write error, if the field hits it; every SDK uses `Expect` above its threshold. | 5 or 6 |
| GitHub's release CDN returned 504s in three CI runs; the backends and tools are now cached across runs and versitygw/s5cmd fall back to `go install` of the same tag. | Cache warm after one green run. | — |
| Header order is not compared in record mode (net/http parses both sides into maps); S3 signatures do not depend on it. | None needed; noted in D-032. | — |
| Garage 2.5.0 refuses SigV2 ("Unsupported authorization method") and the five signed-chunk-trailer modes ("Invalid payload signature"; versitygw accepts the same bytes, so the encoding is right); versitygw refuses SigV2. 28 and 8 GAP cells, each relayed byte-identically. | None; the VAST lab run (Phase 5) exercises them against a backend that supports them. | 5 |
| `GetObjectAttributes` returns 500 from Garage 2.5.0 direct and via. | GAP cell; Garage limitation. | — |
| The tools cache missed on every run until the first green one (the key is the scripts' hash and the save step runs only on success); it is saved now. | None. | — |
| boto3 presigned with the legacy S3 signature for a custom endpoint until `signature_version="s3v4"` was set; versitygw refused it both ways. Harness, not proxy; fixed. | None. | — |

## 6. Questions

1. **Record mode as the PR-time byte-equivalence oracle** (D-032): the request-side diff runs in-process on every PR, the real backends in the `integration` job. Acceptable as the standing arrangement? Recommended: **yes**.
2. **D-034**: accept as a documented gap for v1 (SDKs use `Expect: 100-continue` above their thresholds), with the custom-transport fix deferred? Recommended: **yes**.
3. **GAP cells** in `docs/COMPAT.md` (backend refuses an operation identically direct and via): count as green for the "100% green" gate criterion, since the proxy relayed the refusal byte-identically? Recommended: **yes**, with each gap named in the matrix.
4. **Phase 2 may begin** on branch `phase-2` once #7 and #8 are merged? Recommended: **yes**.

## 7. Reproduce

```
git checkout phase-1                       # after #7 and #8 merge
make lint && make race                     # lint, race, coverage floor
make s3diff-record                         # 285 cases, 0 diffs
make fuzz FUZZ_TIME=10m                    # 4 targets; 1 CPU-hour on 2 workers is 3 × 10 min
make compat                                # Garage and versitygw: s3diff + client matrix → docs/COMPAT.md (needs a host that can run the pinned binaries; CI does)
go test -run '^$' -bench . -benchmem ./internal/...
```

CI runs (repo blakegolliher/tollbooth): #2 run 42562546260, #4 run 42563822018, #5 run 42565687087, #6 run 42566167139 (unit, integration green; audit red on a notices sort locale fixed in #7), #7/#8 final run 42576399162 (all five jobs green; artifact `compat-logs` holds every log under `run/compat/` and `docs/COMPAT.md`). Fuzz and race logs: `run/fuzz-1h/`, `run/race-lint.log` on the dev box (sts-workshop).

## Guardrail passes

- **G1 Simplicity**: exported identifiers added this phase and their second callers: `s3id.Parse`/`RequestIdentity` (proxy, nullbackend, tests), `s3id.RawTarget` (proxy, tests), `s3id.NewDomains` (config, nullbackend), `s3id.ParseOp`/`ParseClass` (config), `s3id.Ops`/`NumOps`/`NumClasses` (admin), `config.Load`/`Parse` (cmd, tests, s3diff), `config.Config` methods `Identity`/`AdminToken`/`Redacted`/`Warnings`/`ReloadCompatible` (cmd), `admin.NewMetrics`/`Metrics` methods (proxy, cmd), `admin.Server`/`Routes` (cmd, Phase 4 parity test), `proxy.New`/`NewServer`/`NewPool`/`Listen`/`CertStore`/`NewAccessLogger` (cmd, s3diff record mode), `nullbackend.New`/`Server` (proxy tests, s3diff). `config.ValidBucketName` has one caller (validation) and is exported for the Phase 4 `rules test` verb; flagged. No interfaces added; `Pool.Transport` is the stdlib `http.RoundTripper` so tests can substitute a fake. Dependencies added: `go.yaml.in/yaml/v4`, `prometheus/client_golang`, `pires/go-proxyproto` (binary), `aws-sdk-go-v2` + `smithy-go` (test only); each has a `docs/deps.md` line. Body buffering: none (`grep -rn 'io.ReadAll\|bytes.Buffer' internal/` hits only test files and the error-body builder's pooled slice). Feature flags: none; kill switches: two, default off. LOC over the phase (main..p1/compat): 94 files, 10,653 insertions, 394 deletions; product code 4,054 lines, tests 3,315 lines (ratio 0.82, the first phase so no previous ratio), test and bench tooling 1,758 lines.
- **G2 Validation**: section 3. No benchmark baseline exists yet to compare against (Gate 2).
- **G3 Telemetry and docs**: every Phase 1 code path is observed: requests, durations, overhead, TTFB, bytes, in-flight, connections, backend up/errors/retries, config reloads; `TestEveryMetricIsInTheCatalog` and `TestLabelSetsMatchTheCatalog` pass. `docs/CONFIG.md` covers every key (`admin.token_file` added at Gate 0). `docs/STATUS.md` and `docs/CONTEXT.md` updated. Contradictions with the specification: none implemented differently; D-034 is a gap the specification's failure table does not list and should (section 5).
- **G4 Dependency and license audit**: `govulncheck` none; `go-licenses check` clean; `THIRD_PARTY_NOTICES.md` regenerated (12 linked modules, all Apache-2.0 or BSD-3); every lifted file carries the shunt header and a `Modified by` line and is in `docs/deps.md` (9 rows lifted, 1 planned for Phase 2: `test/bench/s3bench`); no code from MinIO, Garage, or warp; YAML is `go.yaml.in/yaml/v4` (`go version -m bin/tollbooth` shows no `gopkg.in`); the AWS SDK is not linked into the binary (`make build` checks).
