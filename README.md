# tollbooth

A transparent inline reverse proxy for S3 that adds exactly two things: inline rate limiting
keyed by access key, bucket, prefix, and source address, and hourly showback (API-call and byte
counts) for SLA reporting and chargeback. Every request and response passes through
byte-for-byte; the proxy holds no credentials, re-signs nothing, and fails open.

Status: **Phase 0 (design)**. No product code yet. See `docs/STATUS.md`.

- `docs/SPEC.md` — the development specification (what to build, decisions already made)
- `docs/DESIGN.md` — architecture, data structures, hot-path walkthrough with allocation budget
- `docs/CONFIG.md` — the configuration schema and defaults
- `docs/adr/` — architecture decision records
- `docs/reports/` — gate reports

Apache-2.0. Code adapted from [shunt](https://github.com/blakegolliher/shunt) keeps its headers
and is listed in `THIRD_PARTY_NOTICES.md`.
