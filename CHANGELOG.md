# Changelog

All notable changes to tollbooth. The format follows Keep a Changelog; versions follow semver.
Decisions behind entries are in `docs/DECISIONS.md` (D-nnn) and `docs/adr/`.

## [Unreleased]

## [0.1.0] - 2026-11-10

The first tagged release: a transparent S3 reverse proxy with inline rate limiting and hourly
showback, one binary, with a CLI and a terminal console. Gate 5 report: `docs/reports/gate-5.md`.

### Added

- Transparent S3 reverse proxy lifted from shunt's passthrough pipeline: raw request-target in
  `URL.Opaque`, `Host` preserved, inbound header map reused, hop-by-hop headers stripped, every
  response chunk flushed, no header added to requests or responses (Phase 1).
- Identity pass and classifier over 113 S3 operations, path and virtual-hosted addressing, SigV4
  and SigV2 in header and query forms, PROXY protocol and trusted `X-Forwarded-For` (Phase 1).
- Backend pool with round-robin, active health checks (an unsigned `HEAD /` every 5 s with a 5 s
  timeout; any HTTP status is healthy), passive marking, retry before any request byte; TLS
  listener with certificate reload. A backend given by name is looked up on every new connection
  and the answer remembered: when the resolver does not answer, the proxy connects and probes at
  the last known addresses, stays ready, and keeps serving (ADR-0008).
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
- Prometheus metrics (22), catalogued in `docs/telemetry-catalog.md`; `log/slog` JSON logging;
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
- Rules an operator can read (ADR-0007). The `limits` list is written in S3's own operation names
  (`operation: ListObjectsV2`, a trailing star for a family, a typo answered with the closest
  name), with the unit in the key (`limit_per_second`, `limit_per_minute`, `limit_per_hour`, or
  `blocked: true`), a required `per` (who gets a budget of their own, or `total`), `respond: 503`
  or `429`, and a `note`. A limit is kept in a burst by default (its own number may arrive at
  once) or evenly spaced with `burst: 1` (strict: one at a time), and the read-back says which.
  Every rule is read back as a sentence by `check-config`, `rules list`, `rules show`,
  `GET /admin/limits`, and the console. `tollbooth_limiter_matched_total{rule}`
  counts what each rule matches, beside what it denies. The TUI's Limits screen shows the rules
  in words with matched and throttled per second and the busiest S3 operations, flags a rule that
  matches nothing, and has a guided form (`n` new, `enter` change, `d` delete) with pickers for
  operations and for the access keys and buckets seen in traffic; it writes the rule into the
  config file with every other byte left alone. `config show` and `config diff` list each rule by
  name (Phase 5).

### Verified, and not yet verified

Measured on a 2 vCPU development machine (`docs/reports/gate-5.md` §3 and §4):

- Limiter decision: p99 4.7 µs with 100 rules and 1,000,000 live keys, 0 allocations.
- Showback increment: 198 ns, 0 allocations.
- Config reload under load: 0.6 ms, 0 failed requests.
- Startup to ready: 65 to 78 ms.
- Memory: 226 bytes per limiter key and 257 bytes per showback tuple (about 290 MiB at 1,000,000
  keys and 250,000 tuples).
- 24 h soak at 804 req/s with a fault injected every 10 minutes: 69.5 million requests, RSS 27 to
  37 MiB with no upward slope, 0 errors outside a fault window.
- Byte-equivalence: 293 cases with 0 differences against the recording backend, Garage 2.5.0, and
  versitygw 1.11.0; 184 cases with 0 differences against a VAST 5.5.3.0 cluster.
- Client tools (aws cli, boto3, s5cmd, rclone, s3cmd, elbencho): 122 cells with 0 failures against
  Garage and versitygw (`docs/COMPAT.md`); 48 cells with 0 failures against VAST 5.5.3.0, where 5
  cells fail the same way with and without the proxy (`docs/COMPAT-vast.md`).

**Not yet verified.** These targets are stated for a 16 vCPU reference node that this project has
not had; they are unmeasured there, and the numbers from the 2 vCPU machine are not evidence for
them (ADR-0005). The reference-node run is the gate for 0.1.1.

- Added latency per request (target: p50 at most 150 µs, p99 at most 1 ms).
- Small-object throughput over plaintext (target: at least 50,000 requests per second).
- Small-object throughput with TLS terminated (target: at least 30,000 requests per second).
- Large-object streaming (target: at least 80% of line rate).

Also not yet done: the cost of showback on versus off under load; against VAST, make-bucket and
remove-bucket, virtual-hosted addressing, and TLS to the cluster; a run of the container image
and of the systemd unit on a real host.

### Known limitations

- aws cli v2 and boto3 in their default retry mode do not complete a sync at twice a rule's rate;
  adaptive retry mode does (D-044, `docs/OPERATIONS.md`). Hold-and-forward is a Phase 6 ADR.
- A backend that answers and closes before reading the whole request body can surface as a 503
  from the proxy (D-034); every SDK avoids it with `Expect: 100-continue` above its threshold.
- Four end-to-end performance targets are unverified; see "Verified, and not yet verified" above
  and `docs/adr/0005-performance-targets-at-v0.1.0.md`.
- A proxy started while the resolver is not answering cannot locate a backend given by name and
  reports not ready until the resolver returns; one that was already running keeps serving
  (ADR-0008).
- A per-minute or per-hour limit is a budget, not a pace: once it is spent the client is told to
  wait up to the refill interval (`Retry-After: 180` for 20 an hour), which no SDK's retries
  outlast (`docs/OPERATIONS.md` §9).

### If you ran a build from before this release

- The rule schema changed. `rate_limits.rules` with `id`, `match`, `group_by`, `limit: {rate,
  burst}`, and `on_limit` is replaced by the `limits` list; a file in the old shape fails
  `check-config` with errors that name the new keys (`docs/CONFIG.md`).
- `backend.health_check.timeout` defaults to `5s`, was `2s` (D-064).
