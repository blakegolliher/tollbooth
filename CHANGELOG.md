# Changelog

All notable changes to tollbooth. The format follows Keep a Changelog; versions follow semver.
Decisions behind entries are in `docs/DECISIONS.md` (D-nnn) and `docs/adr/`.

## [Unreleased]

### Added

- Transparent S3 reverse proxy lifted from shunt's passthrough pipeline: raw request-target in
  `URL.Opaque`, `Host` preserved, inbound header map reused, hop-by-hop headers stripped, every
  response chunk flushed, no header added to requests or responses (Phase 1).
- Identity pass and classifier over 113 S3 operations, path and virtual-hosted addressing, SigV4
  and SigV2 in header and query forms, PROXY protocol and trusted `X-Forwarded-For` (Phase 1).
- Backend pool with round-robin, active health checks, passive marking, retry before any request
  byte; TLS listener with certificate reload (Phase 1).
- Rate limiting: GCRA per rule and key, rules compiled with precomputed matchers, one pass in
  config order, `503 SlowDown` and `429 TooManyRequests` with `Retry-After` and
  `x-amz-retry-after`, hot reload by one pointer swap, per-rule deny ring for `/admin/limits/top`,
  key cap that fails open (Phase 2; D-036 to D-045).
- Showback: one row per hour per tuple of configured dimensions, NDJSON snapshots every flush
  interval, exact seal at the hour boundary, restart recovery, overflow row, `usage query|top|verify`
  and `GET /admin/usage` (Phase 3; D-046 to D-049).
- Operator surface: `status`, `rules`, `config`, `backends`, `usage`, `doctor`, `check-config`,
  `version`, all with `--format table|json|csv`; `tollbooth tui` with five screens; admin API
  `/healthz`, `/readyz`, `/metrics`, `/admin/reload`, `/admin/config`, `/admin/limits`,
  `/admin/limits/top`, `/admin/limits/test`, `/admin/usage`, `/admin/status`, `/admin/backends`
  (Phases 2 to 4; D-050 to D-055).
- Prometheus metrics (21), catalogued in `docs/telemetry-catalog.md`; `log/slog` JSON logging;
  optional access log.
- Byte-equivalence harness (`test/s3diff`) with a recording backend, client compatibility matrix
  against Garage and versitygw (`docs/COMPAT.md`), client throttling rows (`docs/THROTTLE.md`),
  SDK e2e suite, load generator and `make bench-e2e`, nightly fuzz and benchmark regression.
- Deployment: systemd unit with resource caps and hardening, distroless container image,
  `make release` artifacts (Phase 5).
- TUI as the operator console (ADR-0006): a Graphs screen (`6`) with braille line charts of
  request and throttle rates, in-flight, backend TTFB, denials per rule, heap, RSS, and goroutines
  over the last `--history` polls; a Config editor (`e` on screen `5`) with NORMAL and INSERT modes,
  validation on every keystroke, `$EDITOR` hand-off, and an explicit, atomic, backed-up save
  (`ctrl+s`, `ctrl+r` saves and reloads). The TUI's save is the only write tollbooth makes to the
  config file; the CLI never writes it. A shared color palette across the screens, a bordered
  editor pane with a mode badge and an always-visible block cursor, and `tab`, `shift+tab`, and the
  arrow keys to move between screens (Phase 5).

### Known limitations

- aws cli v2 and boto3 in their default retry mode do not complete a sync at twice a rule's rate;
  adaptive retry mode does (D-044, `docs/OPERATIONS.md`). Hold-and-forward is a Phase 6 ADR.
- A backend that answers and closes before reading the whole request body can surface as a 503
  from the proxy (D-034); every SDK avoids it with `Expect: 100-continue` above its threshold.
- Performance targets are stated for a 16 vCPU reference node; see `docs/adr/0005-performance-targets-at-v0.1.0.md`
  for what has and has not been measured.

## [0.1.0] - unreleased

The first tagged release follows the Gate 5 decision (docs/reports/gate-5.md).
