# Configuration

One YAML file. It is validated as a whole, published as an immutable value behind one
`atomic.Pointer` on `SIGHUP` or `POST /admin/reload`, and an invalid file keeps the running config.
Unknown keys are errors. Nothing in tollbooth ever writes this file. `tollbooth check-config <file>`
validates without starting and exits 1 on any error.

Types: `duration` is a Go duration string (`10s`, `5m`, `300ms`). `bytes` is an integer count of
bytes. `cidr` is `a.b.c.d/n` or an IPv6 prefix; a bare address means `/32` or `/128`. Keys marked
**required** have no default. Every key here is canonical: the names below are the ones the
specification, the design, the metrics catalog, and the admin API use.

The example file `deploy/tollbooth.example.yaml` sets every key to its default.

## Top level

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `node_id` | string | **required** | Identifies this node in showback files and `/admin/status`. Must be stable across restarts; `check-config` refuses an empty value. A pod name is the wrong choice. Example: `tb-a01`. |
| `base_domains` | list of string | `[]` | Virtual-hosted-style base domains. A `Host` whose port-stripped name ends in `.<domain>` yields the bucket as everything before the suffix, dots included. Empty means path-style only. Example: `[s3.lab.example.com]`. |

## `listen` (data listener)

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `listen.address` | string | `:8443` | TCP listen address. |
| `listen.tls.cert` | path | `""` | PEM certificate chain. Empty `cert` and `key` means plaintext HTTP. Hot-reloaded on reload. |
| `listen.tls.key` | path | `""` | PEM private key. Must be set together with `cert`. |
| `listen.tls.min_version` | `1.2` or `1.3` | `1.2` | Minimum TLS version. Nothing below 1.2 is accepted. |
| `listen.proxy_protocol` | bool | `false` | Expect a PROXY protocol v1 or v2 header on every accepted connection (`go-proxyproto`). The header's source address becomes `source_ip`. A connection without a header is closed. |
| `listen.trusted_proxies` | list of cidr | `[]` | Peers from which `X-Forwarded-For` is honored: the rightmost entry that is not itself in this list becomes `source_ip`. From any other peer the header is ignored. |
| `listen.read_header_timeout` | duration | `10s` | `http.Server.ReadHeaderTimeout`. |
| `listen.idle_timeout` | duration | `120s` | Keep-alive idle timeout for client connections. |
| `listen.max_header_bytes` | bytes | `1048576` | `http.Server.MaxHeaderBytes` (1 MiB). |
| `listen.max_inflight` | int | `10000` | In-flight request cap. Excess requests get `503 SlowDown` and count as `proxy_error`. |

## `backend`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `backend.scheme` | `http` or `https` | **required** | Scheme for every endpoint. |
| `backend.endpoints` | list of `host:port` | **required**, at least one | Backend endpoints, round-robin. No scheme, no path. `check-config` warns when an endpoint's host is not among `base_domains` (the vhost deployment constraint, docs/OPERATIONS.md). Example: `[vast-vip-1:443, vast-vip-2:443]`. |
| `backend.tls.ca_bundle` | path | `""` | PEM CA bundle for `https`. Empty means the system roots. Rejected when `scheme` is `http`. |
| `backend.tls.insecure_skip_verify` | bool | `false` | Lab only. `serve` logs a warning at startup and `doctor` reports it. |
| `backend.dial_timeout` | duration | `2s` | TCP connect timeout per endpoint. |
| `backend.response_header_timeout` | duration | `300s` | Time allowed for the backend's response headers (a huge `ListObjectsV2` can legitimately take minutes). |
| `backend.idle_body_timeout` | duration | `60s` | Idle-body timeout in each direction: aborts when no bytes move for this long. Pushed forward on every copied chunk. There is no whole-request timeout. |
| `backend.max_idle_conns_per_host` | int | `1024` | Keep-alive pool size per endpoint. |
| `backend.health_check.interval` | duration | `5s` | Active check cadence: TCP connect plus `HEAD /`; any HTTP status, 403 included, means healthy. |
| `backend.health_check.timeout` | duration | `2s` | Per-check timeout. |

## `proxy`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `proxy.copy_buffer_bytes` | bytes | `262144` | Size of each pooled body-copy buffer (256 KiB). Streaming, not buffering. |
| `proxy.drain_timeout` | duration | `30s` | On `SIGTERM`: stop accepting, wait up to this long for in-flight requests, flush showback, exit 0. |

## `cluster`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `cluster.node_count` | int ≥ 1 | `1` | Number of identical proxy nodes behind the load balancer. Each node enforces `rate / node_count` and `burst / node_count` (rounded up; a burst never rounds below 1). No coordination. |

## `rate_limits`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `rate_limits.default_on_limit` | `slowdown` or `too_many_requests` | `slowdown` | Response mode for rules that do not set `on_limit`. `slowdown` is HTTP 503 with the S3 `SlowDown` body; `too_many_requests` is HTTP 429 with `TooManyRequests`. Both carry `Retry-After`. |
| `rate_limits.rules` | list of rule | `[]` | Evaluated in this order. No matching rule means unlimited. |

### Rule

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `id` | string | **required**, unique | Identifier for metrics (`limiter_denied_total{rule}`), the admin API, and the limiter key. Never shown to clients. `[A-Za-z0-9_.-]{1,64}`. |
| `match.access_keys` | list of string | any | Exact access key IDs. `""` matches anonymous requests. |
| `match.buckets` | list of string | any | Exact bucket names. `""` matches service-level operations. |
| `match.prefixes` | list of string | any | Key prefixes relative to the bucket; longest match wins. For list operations the request's `prefix` query parameter is matched instead (ADR-0001). |
| `match.source_cidrs` | list of cidr | any | Source address ranges. |
| `match.ops` | list of string | any | Operation classes (`LIST`, `READ`, `WRITE`, `DELETE`, `BUCKET`, `UNKNOWN`) and/or operation names from `docs/OPERATIONS_TABLE.md`. Unknown names are errors. |
| `group_by` | list of `access_key`, `bucket`, `prefix`, `source_ip` | `[]` | One GCRA state per distinct tuple of these dimensions. Empty means one shared state for everything the rule matches. `prefix` requires `match.prefixes` and means the matched configured prefix. |
| `limit.rate` | number ≥ 0 | **required** | Requests per second. `0` blocks every matching request with `Retry-After: 1`. Fractional rates are allowed (`0.5` = one request every two seconds). |
| `limit.burst` | int ≥ 1 | `max(1, ceil(rate))` | Bucket capacity: exactly this many requests are admitted from a full bucket. Ignored when `rate` is `0`. |
| `on_limit` | `slowdown` or `too_many_requests` | `rate_limits.default_on_limit` | Response mode when this rule denies. |

Every listed match condition must hold (AND); an omitted condition matches anything. `limit.bandwidth` is reserved for Phase 6 and is rejected as an unknown key until then.

## `limiter`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `limiter.max_keys` | int | `2000000` | Cap on live GCRA keys across all rules. Past it, new keys fail open and `limiter_keys_overflow_total` increments. |
| `limiter.sweep_interval` | duration | `10s` | How often the sweeper walks the shards deleting keys whose bucket is full (`TAT ≤ now`) and keys of rules no longer configured. |

## `showback`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `showback.dir` | path | **required** | Node-local store: `current/` holds the open hour's snapshot, `usage/YYYY/MM/DD/` the sealed hours. Must be a persistent path; `check-config` refuses an empty value and `doctor` warns when it sits on tmpfs or an ephemeral volume. |
| `showback.flush_interval` | duration | `60s` | Snapshot cadence for the open hour. A crash loses at most this much. |
| `showback.retention_days` | int | `400` | Sealed files older than this are deleted by a daily sweep. `0` disables deletion. |
| `showback.dimensions` | list of `access_key`, `bucket`, `prefix`, `source_ip`, `operation`, `status_class` | `[access_key, bucket, prefix, operation, status_class]` | Columns that form a tuple. `operation` and `status_class` are always present; omitting them is an error. Add `source_ip` to track addresses. |
| `showback.max_tuples_per_hour` | int | `250000` | Tuple cap per open hour. Past it, new tuples fold into one `__overflow__` row per `(operation, status_class)` and `showback_collapsed_total` increments. |
| `showback.prefixes` | map of bucket → list of string | `{}` | Per-bucket showback prefixes, longest match; a key under none of them is recorded as `""` (bucket root). Arbitrary key depth is never recorded. Example: `{checkpoints: [ckpt/, logs/]}`. |

## `admin`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `admin.address` | string | `127.0.0.1:9900` | Admin listener: health, metrics, reload, limits, usage. Separate server, separate connection pool. Bind to localhost or a management interface. |
| `admin.token` | string | `""` | Optional bearer token required on every `/admin/*` route (`/healthz`, `/readyz`, `/metrics` stay open). Redacted in `GET /admin/config`. The CLI and TUI read it from here when `--token` and `TOLLBOOTH_TOKEN` are unset. |
| `admin.pprof` | bool | `false` | Serve `/debug/pprof/*` on the admin listener. |

## `logging`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `logging.level` | `debug`, `info`, `warn`, `error` | `info` | `log/slog` JSON to stdout. Object keys and access keys appear only at `debug`. |
| `logging.access` | bool | `false` | Per-request access log at `info`. A debugging aid, not an accounting source; showback is the record. |
| `logging.throttle_sample` | int ≥ 1 | `100` | Throttle decisions are logged at `debug` one in this many, so a throttle storm cannot flood the log. |

## `features` and `kill_switches`

Both live in `internal/config/features.go`. There are no feature flags in v1; the `features` map
exists so one is added in one place. Kill switches default to the normal behavior (`false`) and
document what breaks when flipped.

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `kill_switches.limiter_disable` | bool | `false` | Skip rule evaluation: every request is forwarded. Flipped, no rule is enforced and `limiter_denied_total` stops moving; use it to get past a limiter bug while serving. |
| `kill_switches.showback_disable` | bool | `false` | Stop counting and snapshotting. Flipped, the hours it covers are missing from the store and `usage verify` reports them as downtime; use it only to confirm a suspected showback cost or fault. |

## Validation rules `check-config` enforces

- Unknown keys anywhere are errors, with the key path named.
- `node_id`, `showback.dir`, `backend.scheme`, `backend.endpoints` are required.
- Rule ids are unique; `group_by: [prefix]` needs `match.prefixes`; `match.ops` names must exist; CIDRs parse; `on_limit` and `default_on_limit` are one of the two modes.
- `listen.tls.cert` and `listen.tls.key` are set together, the files exist, and the pair loads.
- `backend.tls.*` is rejected with `scheme: http`.
- `showback.dimensions` contains `operation` and `status_class`; `showback.prefixes` keys are valid bucket names and prefixes are non-empty.
- Warnings (exit 0, printed): a backend endpoint host not in `base_domains`; `insecure_skip_verify: true`; `showback.retention_days: 0`.

## Reload semantics

`SIGHUP` or `POST /admin/reload` parses and validates the whole file, builds the complete rule set
and matchers, and swaps one pointer. Requests in flight finish under the config they started with.
Limiter state is keyed by rule id and is never reset: a changed `rate` or `burst` applies from the
next decision; state of removed rules is swept. `listen.address`, `admin.address`, `node_id`, and
`showback.dir` cannot change on reload; a change to any of them is a reload error that keeps the
running config and says a restart is needed. TLS certificates are re-read. Reload must complete
in under 100 ms with zero failed requests under load (Phase 2 test).
