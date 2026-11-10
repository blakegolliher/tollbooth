# tollbooth

A transparent inline reverse proxy for S3 that adds exactly two things: inline rate limiting
keyed by access key, bucket, prefix, and source address, and hourly showback (API-call and byte
counts) for SLA reporting and chargeback. Every request and response passes through
byte-for-byte; the proxy holds no credentials, re-signs nothing, and fails open.

A limit is written in S3's own words, and the tool reads it back:

```yaml
limits:
  - name: list-storm
    operation: ListObjectsV2
    limit_per_second: 20
    per: source_ip
```

```
$ tollbooth check-config tollbooth.yaml
  list-storm: At most 20 ListObjectsV2 calls per second from each source IP. Over the limit, the client gets 503 SlowDown.
```

Or without opening the file: `tollbooth tui --config tollbooth.yaml`, screen `2`, `n` is a guided
form that lists the operations and access keys in live traffic and writes the same four lines.

Status: **v0.1.0**. Every feature of the specification is built and gated. Four end-to-end
performance targets are stated for a 16 vCPU reference node and are **not yet verified** there;
`CHANGELOG.md` lists what is measured and what is not (`docs/STATUS.md`,
`docs/adr/0005-performance-targets-at-v0.1.0.md`).

Quick start:

```
make build                      # bin/tollbooth (Go 1.27.3; GOTOOLCHAIN=auto fetches it)
make demo && source demo/env.sh # a local stack on :28000 with four rules; demo/DEMO.md is the walkthrough
bin/tollbooth tui --config $TOLLBOOTH_DEMO_CONFIG   # the operator console: 2 is Limits, n adds one
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
