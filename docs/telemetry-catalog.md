# Telemetry catalog

Every metric tollbooth exports is listed here before it is implemented; a test fails on a metric
that is not in the catalog, and a cardinality test asserts each label set is exactly the one
written here (CLAUDE.md). Prefix: `tollbooth_`. Labels never carry an access key, IP, bucket, or
object key: that is showback's job and would blow up the time-series database.

Bounded label values: `operation` (the 113 names in docs/OPERATIONS_TABLE.md), `op_class`
(`LIST`, `READ`, `WRITE`, `DELETE`, `BUCKET`, `UNKNOWN`), `status_class` (`2xx`, `3xx`, `4xx`,
`5xx`, `throttled`, `proxy_error`), `result` (`forwarded`, `throttled`, `shaped`, `error`), `rule`
(configured rule ids; bounded by the config), `endpoint` (configured backend endpoints), `kind`,
`direction`, `side`.

`shaped` as a `result` value and `limiter_shaped_wait_seconds` are reserved by the specification
for the Phase 6 hold-and-forward mode; in v1 they are catalogued, registered with zero
observations, and never incremented, so a dashboard built now does not change shape later.

| Name | Type | Labels | Unit | Buckets | Description | Phase |
| --- | --- | --- | --- | --- | --- | --- |
| `tollbooth_requests_total` | counter | `operation`, `op_class`, `status_class`, `result` | requests | — | Requests completed, by operation, response status class, and what the proxy did (`forwarded`, `throttled`, `shaped` (reserved), `error` = proxy-originated error). Pre-resolved handles (DESIGN §9.3) | 1 |
| `tollbooth_request_duration_seconds` | histogram | `op_class` | seconds | 100 µs … 600 s, log-spaced, 20 buckets | Client-observed duration, first header byte in to last body byte out | 1 |
| `tollbooth_proxy_overhead_seconds` | histogram | `op_class` | seconds | 10 µs … 100 ms, log-spaced, 16 buckets | Handler entry to the moment `RoundTrip` is called (identity, classify, limiter, build) | 1 |
| `tollbooth_backend_ttfb_seconds` | histogram | `op_class` | seconds | 1 ms … 600 s, log-spaced, 20 buckets | `RoundTrip` call to response headers received | 1 |
| `tollbooth_bytes_total` | counter | `direction` ∈ {`in`, `out`} | bytes | — | Body bytes read from clients (`in`) and written to clients (`out`); headers excluded | 1 |
| `tollbooth_inflight_requests` | gauge | — | requests | — | Requests between handler entry and completion | 1 |
| `tollbooth_connections_open` | gauge | `side` ∈ {`client`, `backend`} | connections | — | Open TCP connections on each side | 1 |
| `tollbooth_backend_up` | gauge | `endpoint` | — | — | 1 when the endpoint passed its last health check, else 0 | 1 |
| `tollbooth_backend_errors_total` | counter | `endpoint`, `kind` ∈ {`dial`, `tls`, `timeout`, `reset`, `other`} | errors | — | Backend request failures by endpoint and kind | 1 |
| `tollbooth_backend_retries_total` | counter | — | retries | — | Requests moved to another endpoint after a connect or TLS error before any request byte was sent | 1 |
| `tollbooth_config_reload_total` | counter | `result` ∈ {`ok`, `error`} | reloads | — | Reload attempts via `SIGHUP` or `POST /admin/reload` | 1 |
| `tollbooth_limiter_decision_seconds` | histogram | — | seconds | 100 ns … 1 ms, log-spaced, 14 buckets | Time for the whole rule pass (match plus GCRA for every matching rule) | 2 |
| `tollbooth_limiter_keys` | gauge | — | keys | — | Live GCRA keys across all rules | 2 |
| `tollbooth_limiter_keys_overflow_total` | counter | — | requests | — | Requests allowed without state because `limiter.max_keys` was reached (fail open) | 2 |
| `tollbooth_limiter_denied_total` | counter | `rule` | requests | — | Requests denied, by the rule that denied them | 2 |
| `tollbooth_limiter_shaped_wait_seconds` | histogram | `rule` | seconds | 1 ms … 60 s, log-spaced, 16 buckets | Reserved for Phase 6 shaping; registered, never observed in v1 | 6 (catalogued now) |
| `tollbooth_limiter_errors_total` | counter | — | errors | — | Panics or internal errors inside the limiter, each failed open | 2 |
| `tollbooth_showback_tuples` | gauge | — | tuples | — | Distinct tuples in the open hour | 3 |
| `tollbooth_showback_flush_duration_seconds` | histogram | — | seconds | 1 ms … 60 s, log-spaced, 16 buckets | Time to snapshot or seal one hour file (encode, fsync, rename) | 3 |
| `tollbooth_showback_flush_errors_total` | counter | — | errors | — | Snapshot or seal attempts that failed (disk full, read-only, I/O error); counting continued in memory | 3 |
| `tollbooth_showback_collapsed_total` | counter | — | requests | — | Requests folded into the `__overflow__` row because `showback.max_tuples_per_hour` was reached (the specification also calls this `showback_overflow_total`; this is the one name) | 3 |
| `tollbooth_showback_clock_skew_total` | counter | — | requests | — | Requests whose completion hour was earlier than the open hour (wall clock stepped back); counted in the open hour | 3 |

Go runtime and process metrics come from the client library's default collectors and are not
listed individually.

## Logging fields

`log/slog` JSON. Fields that may appear: `msg`, `level`, `time`, `node_id`, `request_id` (only on
proxy-originated responses), `operation`, `op_class`, `status`, `rule` and `retry_after` (throttle
decisions, at `debug`, one in `logging.throttle_sample`), `endpoint`, `err`, `config`,
`generation`, `rules`, `duration_ms` (reload results, at `info`). `bucket` and `source_ip` appear
only at `debug` (CLAUDE.md); access keys and object keys never appear in logs at any level (D-042):
`rules top` and showback carry them. `TestThrottleLogSampled` asserts the debug line's fields.

## Showback columns

The third observation surface. `docs/SHOWBACK_SCHEMA.md` is its catalog; every request is a row
increment there, including the ones the proxy denied or could not forward.
