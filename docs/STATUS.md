# STATUS

Current phase: **0 (Design)**, gate report written (`docs/reports/gate-0.md`), awaiting Fable's
review and Blake's decision on the open ADRs. No product code exists; the next phase starts only
when told to.

## Phases

| Phase | Deliverables | Gate | State |
| --- | --- | --- | --- |
| 0 Design | `docs/DESIGN.md`; ADR-0001, ADR-0002 (open decisions) and ADR-0003 (TUI in one binary); shunt's op table checked against the API reference (`docs/OPERATIONS_TABLE.md`); repo scaffold, `Makefile`, CI skeleton, `THIRD_PARTY_NOTICES.md`; `docs/CONFIG.md` | Fable approves the design and ADRs; every requirement maps to a `DESIGN.md` section or an explicit deferral | **report written 2026-11-08**, branch `phase-0` |
| 1 Passthrough | transparent proxy lifted from shunt + TLS, PROXY protocol, trusted XFF; identity; classifier; backend pool; admin health/ready/metrics; null and recording backends | byte-equivalence green; compat matrix 100% on Garage, versitygw, MinIO; fuzz 1 CPU-hour clean; `-race` clean | not started |
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
1.27.3 (verified). No Docker on the dev box; integration runs need `podman compose` or CI.

Gate result: pending.

## Open ADRs

- ADR-0001 prefix rules and list operations — recommendation (b)
- ADR-0002 bench load generator — recommendation (c)
