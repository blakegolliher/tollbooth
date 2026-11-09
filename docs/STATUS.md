# STATUS

Current phase: **4 (Operator surface), gate report written** (`docs/reports/gate-4.md`), awaiting
Fable's review and Blake's decision. Open, stacked on `phase-4`: #20 (`p4/admin`: status and
backends routes, the remaining verbs, doctor, goldens, parity test) and #21 (`p4/tui`: the TUI,
runbook tasks, the report). Started 2026-11-09 on Blake's go after Gate 3 passed; `phase-3`
tagged, #19 brings it to `main`. Merge #20 first, then #21.

## Phases

| Phase | Deliverables | Gate | State |
| --- | --- | --- | --- |
| 0 Design | `docs/DESIGN.md`; ADR-0001, ADR-0002 (open decisions) and ADR-0003 (TUI in one binary); shunt's op table checked against the API reference (`docs/OPERATIONS_TABLE.md`); repo scaffold, `Makefile`, CI skeleton, `THIRD_PARTY_NOTICES.md`; `docs/CONFIG.md` | Fable approves the design and ADRs; every requirement maps to a `DESIGN.md` section or an explicit deferral | **pass, 2026-11-08** (first review: pass with fixes; fixes applied; second review: pass); tagged `phase-0` |
| 1 Passthrough | transparent proxy lifted from shunt + TLS, PROXY protocol, trusted XFF; identity; classifier; backend pool; admin health/ready/metrics; null and recording backends | byte-equivalence green; compat matrix 100% on Garage and versitygw; fuzz 1 CPU-hour clean; `-race` clean | **report written 2026-11-09**: byte-equivalence 0 diffs on Garage and versitygw; compat matrix 0 failures; fuzz 1 CPU-hour + 20 min clean; race clean; coverage 92.1% |
| 2 Rate limiting | rules, matchers, GCRA, single pass, both modes, `Expect: 100-continue` measured, hot reload, limiter metrics, `/admin/limits`, `rules *` verbs | limiter suite green (fake clock); 10 µs p99; reload under load zero failures; SDKs complete work under 1 req/s | **report written 2026-11-09**: limiter suite 25 tests green; p99 4.7 µs at 1,000,000 keys; 20 reloads under load, slowest 0.60 ms, 0 failed; SDK completion by retry mode (D-044) |
| 3 Showback | aggregator, snapshot and seal, NDJSON, `usage` verbs, `/admin/usage`, dimensions and cap, restart, multi-node sum | reconciliation exact; rollover tests; kill between snapshots loses ≤ `flush_interval`; verify detects a removed hour | **report written 2026-11-09**: reconciliation exact; 19 clock-injected tests green; Record 198 ns, 0 allocs; 257 B per tuple |
| 4 Operator surface | remaining verbs, shared admin client, `/admin/status`, `/admin/limits/test`, `/admin/backends`, `tollbooth tui` | parity test; CLI goldens ×3 formats; `teatest` goldens; runbook tasks from the TUI and CLI alone | **report written 2026-11-09**: parity test green; 38 CLI goldens ×3 formats; 30 TUI frames at 80×24 and 120×40; two runbook tasks |
| 5 Hardening and v0.1.0 | bench runs with profiles, tuning, 24 h soak, caps, `OPERATIONS.md`, systemd, container, `CHANGELOG.md`, tag | all performance targets met or ADR'd; soak flat | not started |
| 6 Backlog | shaping, cluster limits, bandwidth, Parquet, OpenTelemetry, connection caps, rule editing, drain, web UI | each starts with an ADR | not scheduled |

## Phase 4 record

Shipped: everything in the Phase 4 row: `GET /admin/status` (D-050) and `GET /admin/backends`;
`status`, `config show|reload|diff`, `backends list`, `doctor` (D-051); `tollbooth tui` with the
five screens (D-052, D-053); `TestOperatorSurfaceParity`; CLI goldens for every verb in three
formats; TUI golden frames at both sizes including the unreachable state; the two runbook tasks in
`docs/OPERATIONS.md`. Cut: nothing (the full runbook is Phase 5 as planned).

Findings: a context-driven stop could take showback's final snapshot before the last request's
completion, because the background context derived from serve's context; fixed (D-054), found by
the race suite under load. boto3 in adaptive retry mode landed 199 of 200 objects in one of three
CI throttle runs; the runbook says "completed every run measured but one".

Numbers (dev box): `doctor` 17 ms for 10 checks; forwarded path still 7 allocations with the
status counters and per-endpoint atomics on; the data path is otherwise untouched.

Gate result: pending.

## Phase 3 record

Shipped: everything in the Phase 3 row, plus `FuzzRead`, retention, the five showback metrics,
the drain's final snapshot, the on/off cost A/B, the gated 250,000-tuple measurement, D-046
through D-049. Cut: nothing (`doctor`'s tmpfs warning rides with `doctor` in Phase 4).

Findings: the specification's test-plan row 8 ("lands in hour 10") contradicts its own column
definition and the Gate 0 correction; the code and test follow the completion hour (D-047). The
first writer design lost an hour that opened and closed between two ticks; the aggregator now
hands every replaced table to the writer (D-049).

Numbers (dev box): `Record` 198 ns and 0 allocations; 257 bytes per tuple at 250,000; snapshot
of 250,000 tuples 1.09 s; reconciliation exact over 497 requests of a mixed generator; race
coverage 92.3%.

Gate result: **pass** (Blake, 2026-11-09: #18 merged, "open phase-3 to main and start phase 4").

## Phase 2 record

Shipped: everything in the Phase 2 row, plus `x-amz-retry-after` on throttle responses (D-043),
`/admin/limits/test` and `/admin/config` (D-039, D-040), the shared `admin.Client`, the bench
harness of ADR-0002 (`bench/loadgen`, `bench/run.sh`, `make bench-e2e`) and the first regression
baseline from the fixed CI runner, the e2e SDK throttling suite (`test/e2e`), the client-tool
throttling rows (`test/integration/throttle.sh`, `docs/THROTTLE.md`), the 1,000,000-key and 60 s
flood measurements, D-036 through D-044.

Cut: nothing. `config show/reload/diff` stay in Phase 4 as the specification places them.

Findings: aws cli v2 and boto3 in their default retry mode do not complete a 200-object sync at
twice the rule's rate (80/200 and 190/200); adaptive mode completes, as do s5cmd, rclone, and
s3cmd with defaults; the Go SDK as shipped ignores both Retry-After headers and honors
`x-amz-retry-after` only with `AWS_NEW_RETRIES_2026=true` (D-044). The throttled path allocated
differently under `-race` until request ids left `crypto/rand` (D-041). CodeQL flagged the access
key in the throttle debug line; it is gone from logs at every level (D-042).

Bench numbers (dev box, 2 vCPUs shared by generator, backend, and proxy; indicative): limiter
decision p99 4.7 µs at 1,000,000 keys with 100 rules; 226 B per key; 100-rule decision 980 ns
(335 ns on the CI runner); reload under load slowest 0.60 ms; startup to ready 65–78 ms; added
latency p50 2.1 ms and proxied throughput 2,194 req/s against direct 6,284 at concurrency 8, which
measure the box, not the proxy (44% of proxy CPU is socket syscalls, `limiter.Decide` 1.25%). The
reference-node run is Phase 5.

Gate result: **pass** (Fable, 2026-11-09). Answers: 1 yes (keep `x-amz-retry-after`); 2 yes, Fable
amends the specification's category 7 wording; 3 yes; 4 yes; 5 Phase 5; 6 yes once the stack is
in `phase-2`. Flags acted on: the reserved shaping histogram is deleted until shaping ships (D-045);
the field evidence for shaping is in ADR-0004's stub; the runbook states the client retry-mode
facts (`docs/OPERATIONS.md`). The stack had been merged top-down and collapsed into `p2/e2e`; #15
brings it into `phase-2`, #16 brings `phase-2` into `main`; `phase-1` and `phase-2` are tagged at
the close.

## Phase 1 record

Shipped: everything in the Phase 1 row, plus the record mode of `test/s3diff` (in-process request
and response diff on every PR), the chaos tests for the Phase 1 rows of the failure table, D-026
through D-035, `docs/CONTEXT.md`, `docs/COMPAT.md` (generated).

Cut: nothing. MinIO and `mc` were dropped at Gate 0 (D-022).

Findings fixed on the way: `net.SplitHostPort` and `netip.ParseAddrPort` allocate on their failure
paths (identity pass now zero-allocation); a trickle response needs a flush per chunk (Gate 0);
aws cli v2 signs `Content-Length: 0` on GET and DELETE and Go's transport drops it (D-035). Known
gap recorded, not fixed: D-034 (backend answers and closes before reading the body).

Bench numbers (dev box, 2 vCPUs, indicative): classifier 98 ns, identity 310–607 ns, metrics
observation 172 ns, handler alone 5.1 µs and 7 allocations per 4 KiB GetObject, all zero-allocation
on the decision paths. The reference-node run is Phase 5.

Gate result: **pass** (Blake, 2026-11-09: #7 and #8 merged and Phase 2 started; Fable's written
grading, if any, is not recorded here).

## Phase 0 record

Shipped: everything in the row above plus `docs/SHOWBACK_SCHEMA.md`, `docs/OPERATOR_SURFACE.md`,
`docs/telemetry-catalog.md`, `docs/deps.md`, `docs/reuse.md`, `docs/DECISIONS.md` (20 entries),
`CLAUDE.md`, `deploy/tollbooth.example.yaml`, `.golangci.yml` with the depguard rules, CI and
nightly workflows, `scripts/third-party-notices.sh`.

Cut: nothing.

Bench numbers: none (no code). Environment facts for later phases: Go 1.27.3 is current; the dev
box packages 1.25.3 with `GOTOOLCHAIN=local`, and the Makefile's `GOTOOLCHAIN=auto` downloads
1.27.3 (verified). No Docker on the dev box; integration backends are static binaries (`docs/CONTEXT.md`).

Gate result: **pass with fixes** (Fable, 2026-11-08). Answers: ADR-0001 (b) with a clarification;
ADR-0002 (c); `map[key]int64`; additive showback header with `schema_version`; `base_domains` and
`kill_switches.*` accepted, `admin.token_file` added as the preferred form; `Preflight` is an
anonymous `BUCKET` op; a `//` request-target is forwarded byte-identical, never 400'd; Go 1.27.3.

Fixes applied:

1. Op-table arithmetic: `docs/OPERATIONS_TABLE.md` now has a script-computed reconciliation of all
   116 AWS names (92 kept, 18 added, 5 folded, 1 excluded) plus the 3 non-AWS entries (`Unknown`,
   `Preflight`, `PostObject`) that make the 113, and a reference link per operation, with the
   request line read from each added operation's page.
2. No containers: `test/integration/backends.sh` downloads sha256-pinned static binaries for
   Garage v2.5.0, versitygw v1.11.0, and MinIO `RELEASE.2025-09-07T16-13-09Z` (MinIO is archived;
   `dl.min.io` is gone and the final tag has no assets; `docs/CONTEXT.md`). Makefile `backends`
   and `compat` targets and the CI `integration` job use it.
3. Decisions log: 23 entries, each with its Gate 0 disposition; the three new ones (D-021 static
   binaries, D-022 MinIO pin, D-023 exact allocation bound) are at the end. Ready to read.

Also: the seven allocations are named in `docs/DESIGN.md` §9 and `TestHandlerAllocations` will
enforce the bound exactly (D-023); `docs/SPEC.md` wording changed to `map[key]int64`;
`docs/CONTEXT.md` added.

Corrections from the second review, carried into the design (none blocked the start):

- D-006 revised: every copied chunk is flushed, so trickle responses reach the client;
  `TestTrickleResponseReachesClient` added to the invariants.
- D-024: copy-buffer budget decided now; `listen.max_inflight` 4,096 and
  `proxy.copy_buffer_bytes` 128 KiB bound it at 512 MiB; `check-config` warns past half of
  `GOMEMLIMIT`. The specification's 10,000 default is updated.
- D-025: the showback hour is read from the clock inside `Record`.
- D-023 refined: `TestHandlerAllocations` counts against `httptest.NewRecorder` and a fake
  `RoundTripper`, not by differencing end-to-end runs.
- D-019: deny ring 1,024 per rule.
- D-022 decided: MinIO out of CI, `mc` out of the compat matrix; Garage and versitygw in CI, VAST
  in the lab. The specification's rows are edited to match.

## ADRs

- ADR-0001 prefix rules and list operations — **accepted (b)**, with the no-prefix clarification
- ADR-0002 bench load generator — **accepted (c)**
- ADR-0003 TUI in one binary — accepted
