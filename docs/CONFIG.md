# Configuration

One YAML file. It is validated as a whole, published as an immutable value behind one
`atomic.Pointer` on `SIGHUP` or `POST /admin/reload`, and an invalid file keeps the running config.
Unknown keys are errors. The only thing that writes this file is an explicit save in `tollbooth tui`
(the guided form on the Limits screen and the editor on the Config screen; ADR-0006, ADR-0007); the
CLI never does. `tollbooth check-config <file>` validates without starting, reads every rule back as
a sentence, and exits 1 on any error.

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
| `listen.max_inflight` | int | `4096` | In-flight request cap. Excess requests get `503 SlowDown` and count as `proxy_error`. Together with `proxy.copy_buffer_bytes` it bounds copy-buffer memory (defaults: 4,096 × 128 KiB = 512 MiB). |

## `backend`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `backend.scheme` | `http` or `https` | **required** | Scheme for every endpoint. |
| `backend.endpoints` | list of `host:port` | **required**, at least one | Backend endpoints, round-robin. No scheme, no path. `check-config` warns when an endpoint's host is not among `base_domains` (the vhost deployment constraint, docs/OPERATIONS.md). Example: `[vast-vip-1:443, vast-vip-2:443]`. A name is looked up on every new connection, so a cluster that rotates its DNS answers spreads connections; the last answer is remembered and used when the resolver does not answer (ADR-0008). An address is dialed as it is. |
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
| `proxy.copy_buffer_bytes` | bytes | `131072` | Size of each pooled body-copy buffer (128 KiB). Streaming, not buffering. |
| `proxy.drain_timeout` | duration | `30s` | On `SIGTERM`: stop accepting, wait up to this long for in-flight requests, flush showback, exit 0. |

## `cluster`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `cluster.node_count` | int 1 … 1024 | `1` | Number of identical proxy nodes behind the load balancer. Each node enforces `rate / node_count` and `burst / node_count` (rounded up; a burst never rounds below 1). No coordination. The upper bound keeps the per-node emission interval inside the limiter's arithmetic (D-037). |

## `limits`

The rules (ADR-0007). A rule answers four questions in S3's own words: which requests, how many,
counted how, and what the client gets over the limit.

```yaml
limits:
  # "We have too many ListObjectsV2 calls coming in; limit them to 20 a second."
  - name: list-storm
    operation: ListObjectsV2
    limit_per_second: 20
    per: source_ip
```

`tollbooth check-config` reads that back as: *At most 20 ListObjectsV2 calls per second from each
source IP. Over the limit, the client gets 503 SlowDown.* The same sentence is in `rules list`, on
the TUI's Limits screen, and in `GET /admin/limits`.

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `limits` | list of rule | `[]` | No matching rule means unlimited. A request must pass every rule that matches it. Rules are tried in file order and the pass ends at the first one that denies: the rules above it have counted the request, the rules below it have not. |

### Rule

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `name` | string | **required**, unique | What the rule is called in the console, the metrics (`limiter_matched_total{rule}`, `limiter_denied_total{rule}`), and the logs. Never shown to clients. Letters, digits, and `_ . -`, at most 64. |
| `operation` | one value or a list | every operation | **Which requests.** S3 API operation names as in `docs/OPERATIONS_TABLE.md` (`ListObjectsV2`, `GetObject`), in any letter case; a trailing star names a family (`Get*`, 33 operations; `List*`, 11). Omitted, the rule covers every request, including one the classifier cannot name. A typo is answered with the closest name. The class words older configs used (`READ`, `WRITE`, `LIST`, …) are not accepted. |
| `access_key` | one value or a list | any | Exact access key IDs. `""` matches anonymous requests. |
| `bucket` | one value or a list | any | Exact bucket names. `""` matches service-level operations. |
| `prefix` | one value or a list | any | Key prefixes relative to the bucket; longest match wins. For list operations the request's `prefix` query parameter is matched instead (ADR-0001). |
| `source_ip` | one address or CIDR, or a list | any | Client addresses. |
| `limit_per_second`, `limit_per_minute`, `limit_per_hour` | number above 0 | exactly one of these or `blocked` is **required** | **How many.** The number is in the key's own unit; decimals are allowed (`limit_per_second: 0.5` is one request every two seconds). In requests per second it must lie between `0.000001` and `1000000000` (D-037). |
| `blocked` | bool | `false` | `blocked: true` refuses every matching request with `Retry-After: 1`, in place of a limit. |
| `burst` | int ≥ 1 | the limit's own number, rounded up | How many may arrive at once before the rate applies. Two settings have names. **In a burst** (the default, no `burst` key): `limit_per_hour: 20` lets 20 through at once and then refills one every three minutes, so a client that arrives with a full allowance can make up to twice the number in its first hour. **Evenly spaced** (`burst: 1`, strict): one request at a time and the next after an even interval, one every three minutes for 20 an hour, so the number is never exceeded in any period; the sentence then says "evenly spaced (one every 3 minutes)". Any other number is that many at once. The refill time `burst ÷ rate` must not exceed `1000000000` seconds (D-037). Not allowed on a blocked rule. |
| `per` | `total`, or one or several of `access_key`, `bucket`, `prefix`, `source_ip` | **required** for a limit | **Counted how.** `total` is one budget shared by every request the rule matches; a dimension gives each of its values a budget of its own; a list gives each combination one (`[access_key, bucket]`). `prefix` needs a `prefix` list and means the matched one. On a blocked rule `per` is optional and only decides how `rules top` tells the refusals apart. |
| `respond` | `503` or `429` | `503` | **What the client gets over the limit.** `503` is the S3 `SlowDown` body, what S3 itself answers; `429` is `TooManyRequests`. Both carry `Retry-After`. |
| `note` | string | none | Why the rule exists; one line, at most 200 characters, shown beside the rule in the console. |

The filters (`operation`, `access_key`, `bucket`, `prefix`, `source_ip`) must all hold; an omitted
one matches anything. The four filter nouns are also the values of `per` and the showback column
names. Keys the earlier schema used (`rate_limits`, `id`, `match`, `ops`, `group_by`, `limit`,
`on_limit`) are unknown keys, and the error says what to write instead. A bandwidth limit is Phase 6
work and has no key yet.

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
| `showback.dimensions` | list of `access_key`, `bucket`, `prefix`, `source_ip`, `operation`, `status_class` | `[access_key, bucket, prefix, operation, status_class]` | Columns that form a tuple. `operation` and `status_class` are always present; omitting them is an error. Add `source_ip` to track addresses. Restart-only (D-046). |
| `showback.max_tuples_per_hour` | int | `250000` | Tuple cap per open hour. Past it, new tuples fold into one `__overflow__` row per `(operation, status_class)` and `showback_collapsed_total` increments. |
| `showback.prefixes` | map of bucket → list of string | `{}` | Per-bucket showback prefixes, longest match; a key under none of them is recorded as `""` (bucket root). For the four bucket listings the request's `prefix` parameter is matched (ADR-0001). Arbitrary key depth is never recorded. Example: `{checkpoints: [ckpt/, logs/]}`. Restart-only (D-046). |

## `admin`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `admin.address` | string | `127.0.0.1:9900` | Admin listener: health, metrics, reload, limits, usage. Separate server, separate connection pool. Bind to localhost or a management interface. |
| `admin.token_file` | path | `""` | **Preferred.** File holding the bearer token required on every `/admin/*` route (`/healthz`, `/readyz`, `/metrics` stay open); trailing whitespace trimmed; re-read on reload. The CLI and TUI read it from here when `--token` and `TOLLBOOTH_TOKEN` are unset. |
| `admin.token` | string | `""` | Inline alternative to `token_file` for labs. Mutually exclusive with `token_file`. Redacted to `"<redacted>"` in `GET /admin/config` and `tollbooth config show` (asserted by `TestConfigShowRedactsToken`). |
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
- Rule names are unique; every `operation` entry resolves to at least one S3 operation; `source_ip` values parse; exactly one of `limit_per_second`, `limit_per_minute`, `limit_per_hour`, or `blocked: true` is set; a limit is above 0 and, per second, between `1e-6` and `1e9`; `per` is set for a limit, `total` stands alone, and `per: prefix` has a `prefix` list; `burst ÷ rate` is at most `1e9` seconds; `respond` is `503` or `429`; and `cluster.node_count` is between 1 and 1024.
- `listen.tls.cert` and `listen.tls.key` are set together, the files exist, and the pair loads.
- `admin.token` and `admin.token_file` are not both set; a `token_file` must exist and be non-empty.
- `backend.tls.*` is rejected with `scheme: http`.
- `showback.dimensions` contains `operation` and `status_class`; `showback.prefixes` keys are valid bucket names and prefixes are non-empty.
- Warnings (exit 0, printed): a backend endpoint host not in `base_domains`; `insecure_skip_verify: true`; `showback.retention_days: 0`; `listen.max_inflight × proxy.copy_buffer_bytes` greater than half of `GOMEMLIMIT` when that variable is set in the checking environment (D-024).

## Reload semantics

`SIGHUP` or `POST /admin/reload` parses and validates the whole file, builds the complete rule set
and matchers, and swaps one pointer. The route answers 200 with the result (`ok`, `generation`,
`rules`, `duration_ms`) when the new config is published and 422 with the validation error when the
file is rejected (D-040); `GET /admin/config` repeats the last result. Requests in flight finish under the config they started with.
Limiter state is keyed by rule name and is never reset: a changed limit or `burst` applies from the
next decision; state of removed rules is swept. `listen.address`, `listen.proxy_protocol`,
`admin.address`, `node_id`, `showback.dir`, `showback.dimensions`, `showback.prefixes` (the row
shape and prefix tables are fixed for the process, D-046), `proxy.copy_buffer_bytes`, and every
`backend.*` key cannot change on reload (the listener, the copy-buffer pool, and the backend transport are built
once, D-029); a change to any of them is a reload error that keeps the running config and says a
restart is needed. TLS certificates are re-read. Reload must complete
in under 100 ms with zero failed requests under load (Phase 2 test).
