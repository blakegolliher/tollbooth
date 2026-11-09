# tollbooth

A transparent inline reverse proxy for S3 that adds exactly two things: inline rate limiting
keyed by access key, bucket, prefix, and source address, and hourly showback (API-call and byte
counts) for SLA reporting and chargeback. Every request and response passes through
byte-for-byte; the proxy holds no credentials, re-signs nothing, and fails open.

Status: **Phase 5 (hardening, v0.1.0)**: every feature of the specification is built and gated;
the reference-node performance run and the tag are what remain (`docs/STATUS.md`,
`docs/adr/0005-performance-targets-at-v0.1.0.md`).

Quick start:

```
make build                      # bin/tollbooth (Go 1.27.3; GOTOOLCHAIN=auto fetches it)
make demo && source demo/env.sh # a local stack on :28000 with four rules; demo/DEMO.md is the walkthrough
bin/tollbooth tui               # the operator screens against it
make release                    # dist/: static linux amd64 and arm64 tarballs, unit file, SHA256SUMS
```

Deploying in front of a real S3 endpoint: `docs/OPERATIONS.md` (install, configure, reload,
upgrade, sizing, what each metric means, two operator tasks) and `demo/tollbooth.poc.yaml`.

- `docs/SPEC.md` — the development specification (what to build, decisions already made)
- `docs/DESIGN.md` — architecture, data structures, hot-path walkthrough with allocation budget
- `docs/CONFIG.md` — the configuration schema and defaults
- `docs/adr/` — architecture decision records
- `docs/OPERATIONS.md` — the runbook; `docs/OPERATOR_SURFACE.md` — every CLI verb, TUI screen, and admin route
- `docs/reports/` — gate reports; `docs/DECISIONS.md` — every ambiguity resolved without an ADR
- `demo/` — the demo stack and walkthrough; `bench/` — load generator, null backend, soak, cross-check

Apache-2.0. Code adapted from [shunt](https://github.com/blakegolliher/shunt) keeps its headers
and is listed in `THIRD_PARTY_NOTICES.md`.
