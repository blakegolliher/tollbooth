# STATUS

Current phase: **0 (Design) passed; Phase 1 cleared to start.** Fable's second review (2026-11-08)
graded Gate 0 **pass** after reading the report, all decisions, DESIGN.md, the op-table
reconciliation, the ADRs, CONFIG.md, the lint and CI config, the backend script, and the deps
ledger. Five corrections (none blocking) are folded into the design below; the Phase 0 PR from
`phase-0` into `main` is the phase-close step. Phase 1 begins on branch `phase-1` once the PR is
merged.

## Phases

| Phase | Deliverables | Gate | State |
| --- | --- | --- | --- |
| 0 Design | `docs/DESIGN.md`; ADR-0001, ADR-0002 (open decisions) and ADR-0003 (TUI in one binary); shunt's op table checked against the API reference (`docs/OPERATIONS_TABLE.md`); repo scaffold, `Makefile`, CI skeleton, `THIRD_PARTY_NOTICES.md`; `docs/CONFIG.md` | Fable approves the design and ADRs; every requirement maps to a `DESIGN.md` section or an explicit deferral | **pass, 2026-11-08** (first review: pass with fixes; fixes applied; second review: pass); tagged `phase-0` |
| 1 Passthrough | transparent proxy lifted from shunt + TLS, PROXY protocol, trusted XFF; identity; classifier; backend pool; admin health/ready/metrics; null and recording backends | byte-equivalence green; compat matrix 100% on Garage and versitygw; fuzz 1 CPU-hour clean; `-race` clean | cleared to start |
| 2 Rate limiting | rules, matchers, GCRA, single pass, both modes, `Expect: 100-continue` measured, hot reload, limiter metrics, `/admin/limits`, `rules *` verbs | limiter suite green (fake clock); 10 µs p99; reload under load zero failures; SDKs complete work under 1 req/s | not started |
| 3 Showback | aggregator, snapshot and seal, NDJSON, `usage` verbs, `/admin/usage`, dimensions and cap, restart, multi-node sum | reconciliation exact; rollover tests; kill between snapshots loses ≤ `flush_interval`; verify detects a removed hour | not started |
| 4 Operator surface | remaining verbs, shared admin client, `/admin/status`, `/admin/limits/test`, `/admin/backends`, `tollbooth tui` | parity test; CLI goldens ×3 formats; `teatest` goldens; runbook tasks from the TUI and CLI alone | not started |
| 5 Hardening and v0.1.0 | bench runs with profiles, tuning, 24 h soak, caps, `OPERATIONS.md`, systemd, container, `CHANGELOG.md`, tag | all performance targets met or ADR'd; soak flat | not started |
| 6 Backlog | shaping, cluster limits, bandwidth, Parquet, OpenTelemetry, connection caps, rule editing, drain, web UI | each starts with an ADR | not scheduled |

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
