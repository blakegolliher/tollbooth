# Operations

The runbook for tollbooth: a transparent S3 reverse proxy that adds inline rate limiting and
hourly showback and nothing else. It assumes an operator who has not read the code. Config keys are
in `docs/CONFIG.md`, the showback file format in `docs/SHOWBACK_SCHEMA.md`, every metric in
`docs/telemetry-catalog.md`, and the operator surface (every admin route, CLI verb, and TUI screen)
in `docs/OPERATOR_SURFACE.md`.

## 1. Install

Artifacts: `make release` produces `dist/tollbooth-<version>-linux-{amd64,arm64}.tar.gz` and
`SHA256SUMS`; each tarball holds the static binary, `tollbooth.service`, `tollbooth.example.yaml`,
the `Dockerfile`, the licenses, this file, `CONFIG.md`, and `SHOWBACK_SCHEMA.md`. The container
image (`make image`, `deploy/Dockerfile`) is the same binary on a distroless base.

Host install (systemd):

```
tar -xzf tollbooth-<version>-linux-amd64.tar.gz
sudo install -m 0755 tollbooth-<version>-linux-amd64/tollbooth /usr/local/bin/tollbooth
sudo useradd --system --home /var/lib/tollbooth --shell /usr/sbin/nologin tollbooth
sudo install -d -o tollbooth -g tollbooth -m 0750 /etc/tollbooth /var/lib/tollbooth
sudo install -m 0640 -g tollbooth tollbooth-<version>-linux-amd64/tollbooth.example.yaml /etc/tollbooth/tollbooth.yaml
head -c 32 /dev/urandom | base64 | sudo install -m 0600 -o tollbooth /dev/stdin /etc/tollbooth/admin.token
sudo install -m 0644 tollbooth-<version>-linux-amd64/tollbooth.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now tollbooth
```

The unit runs as `tollbooth`, with `GOMEMLIMIT=1GiB`, `LimitNOFILE=65536`, `MemoryMax=1536M`, a
read-only system, and `ExecStartPre=tollbooth check-config`, so a bad file never starts the
service. To listen on :443 add `AmbientCapabilities=CAP_NET_BIND_SERVICE` to the unit.

Container: mount the config read-only at `/etc/tollbooth/tollbooth.yaml` and a **persistent**
volume at `/var/lib/tollbooth/showback`; publish 8443 and, only on a management network, 9900.

## 2. Configure

Edit `/etc/tollbooth/tollbooth.yaml` (the example file lists every key with its default). The
keys that must be right before the first start:

- `node_id`: stable across restarts (never a pod name): it names the showback files.
- `base_domains`: the virtual-hosted-style domains clients use (`bucket.<domain>`). Because the
  proxy preserves `Host`, **the backend must accept the proxy's hostnames as virtual-host domains**,
  or clients must use path-style addressing. `check-config` warns when a backend endpoint name is
  not among the base domains; it cannot verify the backend's side.
- `backend.scheme` and `backend.endpoints`: the S3 cluster; `https` with `backend.tls.ca_bundle`
  for a private CA.
- `listen.tls.cert` / `key` for TLS termination (TLS 1.2 minimum); the pair is re-read on reload.
- `showback.dir`: a persistent path (`tollbooth doctor` warns on tmpfs).
- `admin.token_file`: the bearer token every `/admin/*` route and every CLI verb needs.

Validate before starting or reloading: `tollbooth check-config /etc/tollbooth/tollbooth.yaml` exits
1 on any error with the key path, and prints warnings (empty `base_domains`, `insecure_skip_verify`,
retention 0, a copy-buffer budget above half of `GOMEMLIMIT`). Then `tollbooth doctor --config …`
checks the environment: file-descriptor limit against the in-flight budget, `GOMAXPROCS` and
`GOMEMLIMIT`, the TLS pair and its expiry, every backend by TCP connect and `HEAD /`, the kernel's
clock-sync flag, and the showback directory (writable, 2 GiB free, not tmpfs).

Rules: `rate_limits.rules`, evaluated in order; a request must pass every matching rule. See the
`Task: change a rule` section and `docs/CONFIG.md` for the schema. Size a rule for the tenant that
misbehaves, not the steady state, and read section 8 before setting a rate below a client's
concurrency.

## 3. Reload

`systemctl reload tollbooth` (SIGHUP), `tollbooth config reload`, or `POST /admin/reload`. The whole
file is parsed and validated, the rule set compiled, and one pointer swapped; requests in flight
finish under the config they started with. Limiter state is keyed by rule id and never reset: a
changed rate applies from the next decision, removed rules' keys are swept. TLS certificates are
re-read. A rejected file keeps the running config, is logged, and shows in `tollbooth config show`
and `GET /admin/config` as the last reload result; the exit code of `config reload` is 1.

Restart-only keys (a reload that changes them is refused): `listen.address`, `listen.proxy_protocol`,
`admin.address`, `node_id`, `showback.dir`, `showback.dimensions`, `showback.prefixes`,
`proxy.copy_buffer_bytes`, every `backend.*` key. Measured: 20 reloads under load, slowest 0.6 ms,
zero failed requests.

## 4. Upgrade without dropping connections

1. Install the new binary over `/usr/local/bin/tollbooth` (the running process keeps its own copy).
2. `tollbooth check-config` with the new binary against the current file: a new version may add
   validation.
3. `systemctl restart tollbooth`. On SIGTERM the proxy stops accepting, lets in-flight requests
   finish for up to `proxy.drain_timeout` (30 s default; `TimeoutStopSec=45` in the unit covers it
   plus the final showback snapshot), writes the snapshot, and exits 0. Clients see connection
   closes, not errors, and every SDK retries a connect failure.
4. Behind a load balancer, drain the node first (fail `/readyz` by stopping the health checks' target,
   or remove it from the pool) and bring nodes back one at a time; `cluster.node_count` stays the
   number of nodes in the pool.
5. Check `tollbooth status`: version, uptime, `ready`, backends up, config generation.

Showback across an upgrade: the snapshot taken on stop is the state; the new process reads it back
and continues the same hour with no double count (the file header's `partial: true` becomes `false`
when the hour seals). An upgrade that takes longer than `flush_interval` loses nothing that
completed before the stop.

## 5. Sizing

Memory, measured on the dev box (`internal/limiter` and `internal/showback` gated tests):

| Item | Per unit | At the default cap | Knob |
| --- | --- | --- | --- |
| Limiter key (one GCRA state per rule × group_by tuple) | 226 B | 1,000,000 keys ≈ 215 MiB | `limiter.max_keys` (2,000,000) |
| Showback tuple (one row per hour per dimension tuple) | 257 B | 250,000 tuples ≈ 61 MiB | `showback.max_tuples_per_hour` |
| Copy buffers | `proxy.copy_buffer_bytes` (128 KiB) × in-flight bodies | 4,096 × 128 KiB = 512 MiB worst case | `listen.max_inflight`, `proxy.copy_buffer_bytes` |
| Deny rings | 1,024 × ~100 B per rule that has denied | 100 rules ≈ 10 MiB | — |

Set `GOMEMLIMIT` to the sum plus headroom; the unit ships `1GiB`, which holds the default caps with
room. `check-config` warns when `max_inflight × copy_buffer_bytes` exceeds half of `GOMEMLIMIT`.
Past `limiter.max_keys` new keys **fail open** (unlimited for that key) and
`tollbooth_limiter_keys_overflow_total` counts them; past `showback.max_tuples_per_hour` new tuples
fold into one `__overflow__` row per operation and status class and `tollbooth_showback_collapsed_total`
counts them. Both are bounded, neither drops a request.

Disk: a sealed hour at the tuple cap is about 50 MiB per node; `showback.retention_days` (400)
bounds the directory at roughly `50 MiB × hours in retention` worst case, far less in practice.
File descriptors: about `2 × listen.max_inflight + backend.max_idle_conns_per_host × endpoints`;
`doctor` compares `RLIMIT_NOFILE` with that.

CPU: the proxy is I/O bound. On the profiled dev-box run 44% of its CPU was socket reads and writes,
the limiter decision 1.25%, the identity pass 0.75%. Give it cores in proportion to bandwidth, not
rules. The limiter decision is 335 ns to 1 µs with 100 rules loaded and under 5 µs p99 at
1,000,000 live keys; the showback increment 200 ns.

## 6. What each metric means and what to do when it moves

Prefix `tollbooth_`; labels never carry a key, bucket, or address (showback does). Scrape
`GET /metrics` on the admin listener.

| Metric | Meaning | When it moves |
| --- | --- | --- |
| `requests_total{operation,op_class,status_class,result}` | every completed request | `result="throttled"` rising: a rule is biting; `rules top` says whose. `status_class="5xx"` with `result="forwarded"`: the backend's errors, not the proxy's. `result="error"` (`status_class="proxy_error"`): the proxy answered itself: backends unreachable, in-flight cap, or a fault; check `backend_up` and `inflight_requests` |
| `request_duration_seconds{op_class}`, `backend_ttfb_seconds{op_class}` | client-observed duration; backend time to first byte | a rise in both is the backend; a rise in duration alone is the client or the network out |
| `proxy_overhead_seconds{op_class}` | handler entry to the backend call (identity, classify, limiter, build) | should stay in tens of microseconds; a rise means CPU starvation on the node |
| `bytes_total{direction}` | body bytes in and out | capacity planning; compare with the NIC |
| `inflight_requests` | requests between entry and completion | near `listen.max_inflight`: raise it (and `GOMEMLIMIT`) or add nodes; the excess is answered `503 SlowDown` as `proxy_error` |
| `connections_open{side}` | open TCP connections | client side far above in-flight: clients are keeping many idle connections; backend side at `max_idle_conns_per_host`: the pool is saturated |
| `backend_up{endpoint}` | 1 when the last health check passed | 0 on an endpoint: `backends list` shows its last error; the pool routes around it and retries connect failures before any request byte |
| `backend_errors_total{endpoint,kind}` | dial, tls, timeout, reset, other | `dial`/`tls`: network or certificates; `timeout`: `backend.response_header_timeout` (300 s) expired, a slow list or a stuck backend; `reset`: the backend closed mid-request |
| `backend_retries_total` | requests moved to another endpoint before any byte was sent | flapping endpoints |
| `limiter_decision_seconds` | the whole rule pass | p99 above 10 µs: too many rules match every request (see `rules list`); 100 rules all matching one request cost 25 µs on the dev box |
| `limiter_keys`, `limiter_keys_overflow_total` | live GCRA keys; requests allowed without state at the cap | overflow rising: a flood of distinct `group_by` tuples (a `source_ip` rule under a scan); raise `max_keys` or narrow the rule; nothing is denied by the overflow |
| `limiter_denied_total{rule}` | denials per rule | the rule is doing its job or is misconfigured; `rules test` tells which |
| `limiter_errors_total` | panics inside the limiter, each failed open | any value is a bug report; the request was forwarded |
| `showback_tuples`, `showback_collapsed_total` | tuples in the open hour; requests folded into `__overflow__` | collapsed rising: add `max_tuples_per_hour` or drop a dimension (`source_ip` is the usual culprit) |
| `showback_flush_duration_seconds`, `showback_flush_errors_total` | snapshot or seal time; failures | errors: disk full, read-only, or permissions on `showback.dir`; counting continues in memory and the next successful flush writes everything; the log has one line per minute |
| `showback_clock_skew_total` | completions whose hour was earlier than the open hour | the wall clock stepped back; rows went to the open hour; check NTP (`doctor` reports the kernel's flag) |
| `config_reload_total{result}` | reloads by result | `error` rising: see `config show` for the message; the running config is unchanged |

Logs: JSON on stdout (`journalctl -u tollbooth`). One startup line, reload results at `info`,
throttle decisions at `debug` sampled one in `logging.throttle_sample`; access keys and object keys
never appear in logs at any level. `logging.access: true` is a debugging aid, not an accounting
source: showback is the record.

## 7. Task: change a rule

The CLI never writes the config file; the TUI's editor writes it only when you press save. A rule
change is an edit, a reload, and a check.

1. Edit `tollbooth.yaml` (the `rate_limits.rules` list; keys and semantics in `docs/CONFIG.md`),
   with your editor or in the TUI: screen `5` (Config), `e` opens the file; `i` inserts, `esc`
   returns to NORMAL mode; every keystroke is validated and the verdict (or the first error with
   its key and line) sits under the text; `ctrl+s` saves (atomically, with `tollbooth.yaml.<UTC
   time>.bak` beside it; refused if the file changed on disk since it was opened, `L` reloads the
   buffer), `ctrl+r` saves and reloads in one step, `E` opens `$EDITOR` on the buffer. Steps 2 to 4
   are then already done; go to step 5.
2. Validate the file before touching the running proxy: `tollbooth check-config tollbooth.yaml`
   prints every problem with its key path and exits 1 on any. In the TUI, screen `5` (Config), key
   `v` runs the same check on the file and shows the verdict on the bottom line.
3. See what will change: `tollbooth config diff --config tollbooth.yaml` lists every key that differs
   between the running config and the file (the Config screen shows the same table under "Running
   config versus …").
4. Reload: `tollbooth config reload` (exit 1 and the error if the file was rejected; the running
   config is kept), or press `r` on the Overview or Config screen (the bottom line says
   "reloaded: generation N" or "reload REJECTED: …"). `SIGHUP` does the same.
5. Verify: `tollbooth rules list` shows the rules in force with live keys and denials;
   `tollbooth rules test --op PutObject --access-key AKIA… --bucket b --key k` answers "which rules
   match and what would each do" without consuming a token (screen `2`, key `t` is the same form);
   `tollbooth rules top` (screen `2`, `enter` on a rule) shows who is being denied right now.
   `tollbooth status` shows the config generation and the last reload result.

The endpoint and token come from `--admin` and `--token`, then `TOLLBOOTH_ADMIN` and
`TOLLBOOTH_TOKEN`, then the config file's `admin.address` and `admin.token_file`.

## 8. Task: export chargeback data

Showback files are NDJSON under `showback.dir` (`docs/SHOWBACK_SCHEMA.md`): `usage/YYYY/MM/DD/HH-<node>.ndjson`
sealed hours, `current/<hour>-<node>.ndjson` the open hour (`partial: true`). Copy or mount every
node's directory into one place; files from many nodes sum.

- One month, one row per access key and bucket, as CSV for a billing system:
  `tollbooth usage query --from 2026-11-01T00:00:00Z --to 2026-12-01T00:00:00Z --group-by access_key,bucket --format csv /srv/showback/*/ > november.csv`
  (`--from` inclusive, `--to` exclusive, RFC 3339; paths are directories or globs; with none,
  `showback.dir` from `--config`). The open hour, if in range, is marked `partial`.
- Who used the most: `tollbooth usage top --by access_key --n 20 --from … --to …`.
- Check the files first: `tollbooth usage verify /srv/showback/*/` recomputes every header and
  lists, per node, missing hours (the node was not running), partial, recovered, and mismatched
  files; exit 1 on any problem. A missing hour is downtime, not lost data.
- From the TUI: screen `3` (Usage), `f` to set the range, `g` to cycle the group-by, `s` to sort,
  `/` to filter, `enter` for the hourly bars of a row, `x` writes the current view as
  `usage-<time>.csv` in the working directory. The TUI reads this node's directory only
  (`GET /admin/usage`); for a multi-node export use the CLI over the collected directories.
- Anything beyond filter, group, and sum is DuckDB:
  `duckdb -c "select access_key, bucket, sum(requests), sum(bytes_in), sum(bytes_out) from read_ndjson('/srv/showback/*/usage/**/*.ndjson', union_by_name=true) where operation is not null group by 1, 2"`.

## 9. Throttling and client retry modes

tollbooth throttles; it does not queue (hold-and-forward is Phase 6, ADR-0004). A denied request
gets `503 SlowDown` (or `429 TooManyRequests`) with `Retry-After` in seconds and
`x-amz-retry-after` in milliseconds, and the client's own retries decide whether the work
completes. Measured with the real tools against a rule whose rate is half the client's
concurrency and whose burst equals it (`docs/THROTTLE.md`, test plan category 7):

| Client | Default retry mode | Adaptive retry mode |
| --- | --- | --- |
| aws cli v2 | **fails**: 80 of 200 objects landed, exit 1 | completes |
| boto3 | **fails**: 190 of 200 landed | completes (199 of 200 in one of three CI runs) |
| aws-sdk-go-v2 | completes under 1 req/s sequentially most runs; a sync at 2× the rate loses objects (17 of 200); with `AWS_NEW_RETRIES_2026=true` it honors `x-amz-retry-after` and completes | completes |
| s5cmd, rclone, s3cmd | complete | — |

**A rate limit set below a client's concurrency makes aws cli fail, not slow down.** The default
standard retry mode gives up after three to five attempts with random backoff and ignores
`Retry-After`. Three levers, in order of preference:

1. Size rules above the clients' concurrency: `burst` at least the client's parallelism (aws cli
   `max_concurrent_requests`, default 10; s5cmd `--numworkers`, default 256) and `rate` at or above
   the sustained request rate you intend to allow. A rule is for the tenant that misbehaves, not for
   the steady state.
2. Tell clients that will be throttled to use adaptive retry mode (`AWS_RETRY_MODE=adaptive`, or
   `retry_mode = adaptive` in `~/.aws/config`; aws cli and every AWS SDK support it) or to raise
   `AWS_MAX_ATTEMPTS`. Adaptive mode paces itself to the rule and completed every run measured but
   one (boto3, 199 of 200 objects); it is the right default for clients that will be throttled, not
   a guarantee.
3. Wait for hold-and-forward (Phase 6), which queues a denied request up to a bound instead of
   answering 503.

Expect a herd: N clients denied together receive the same `Retry-After` and retry together, so
N − 1 are denied again. Adaptive clients spread out on their own; default-mode clients do not.

What to watch: `tollbooth_limiter_denied_total{rule}` (who is being throttled),
`GET /admin/limits/top` or `tollbooth rules top` (which keys), and the client's own error output
(`SlowDown` after max attempts means lever 1 or 2 above).

## 10. Failure behavior

The proxy fails open and never fails closed; there is no knob for the other way.

| Failure | Behavior | You see |
| --- | --- | --- |
| All backends unreachable | `503 ServiceUnavailable` with the S3 body; SDKs retry | `backend_up` all 0, `requests_total{result="error"}` |
| One backend down | routed around; a dial or TLS failure before any request byte is retried on another endpoint | `backend_up{endpoint}` 0, `backend_retries_total` |
| Backend slow to answer | the request waits up to `backend.response_header_timeout` (300 s, a long list is legitimate) | `backend_ttfb_seconds` tail |
| Backend resets mid-download | the client connection is closed (the response was partially written); bytes actually sent are charged | `backend_errors_total{kind="reset"}` |
| Client disconnects mid-upload | the backend request is cancelled; bytes read are charged | `bytes_total{direction="in"}` |
| Limiter fault | fail open, forward | `limiter_errors_total` |
| Key cap or tuple cap reached | fail open (unlimited new keys) or fold (overflow row) | `limiter_keys_overflow_total`, `showback_collapsed_total` |
| Showback directory unwritable | counting continues in memory; the next successful flush writes everything | `showback_flush_errors_total`, one log line per minute |
| Invalid reload | running config kept | `config_reload_total{result="error"}`, `config show` |
| Wall clock steps back | limiter unaffected (monotonic); late rows go to the open hour | `showback_clock_skew_total` |
| SIGTERM | drain up to `drain_timeout`, final snapshot, exit 0 | `tollbooth stopped` in the log |

Kill switches for a bug in the field: `kill_switches.limiter_disable` (every request forwarded, no
rule enforced) and `kill_switches.showback_disable` (no counting; the hours it covers are missing
from the store). Both default off and are reloadable.

## 11. Performance expectations at v0.1.0

Verified on the dev box (2 vCPUs): limiter decision p99 4.7 µs at 1,000,000 keys, showback
increment 200 ns, reload 0.6 ms under load, startup 65–78 ms, 226 B per key and 257 B per tuple.
**Not yet verified on the reference node** (16 vCPU, 25 GbE; ADR-0005): added latency per request,
small-object throughput plaintext and TLS, large-object line-rate streaming, and the showback on/off
cost. The dev-box profile shows the proxy I/O bound (44% of its CPU in socket syscalls, under 2% in
its own code), so expect it to track the kernel's forwarding cost on real hardware; measure with
`make bench-e2e` and `bench/crosscheck.sh` on your node before promising numbers.
