# Operations

The full runbook is written in Phase 5 (install, configure, upgrade without dropping connections,
sizing, every metric and what to do when it moves). Two tasks are documented now because Gate 4
asks an operator who has never seen the code to complete them from the TUI and the CLI alone, and
one section from Phase 2 because it changes how a rule is sized.

## Task: change a rule

Nothing in tollbooth writes the config file. A rule change is an edit, a reload, and a check.

1. Edit `tollbooth.yaml` (the `rate_limits.rules` list; keys and semantics in `docs/CONFIG.md`).
2. Validate the file before touching the running proxy: `tollbooth check-config tollbooth.yaml`
   prints every problem with its key path and exits 1 on any. In the TUI, screen `5` (Config), key
   `v` runs the same check on the file and shows the verdict on the bottom line.
3. See what will change: `tollbooth config diff --config tollbooth.yaml` lists every key that differs
   between the running config and the file (the Config screen shows the same table under "Running
   config versus …").
4. Reload: `tollbooth config reload` (exit 1 and the error if the file was rejected; the running
   config is kept), or press `r` on the Overview or Config screen (the bottom line says
   "reloaded: generation N" or "reload REJECTED: …"). `SIGHUP` does the same. Reload swaps one
   pointer; limiter state is keyed by rule id and is never reset, so a changed rate applies from the
   next decision and the keys of removed rules are swept.
5. Verify: `tollbooth rules list` shows the rules in force with live keys and denials;
   `tollbooth rules test --op PutObject --access-key AKIA… --bucket b --key k` answers "which rules
   match and what would each do" without consuming a token (screen `2`, key `t` is the same form);
   `tollbooth rules top` (screen `2`, `enter` on a rule) shows who is being denied right now.
   `tollbooth status` shows the config generation and the last reload result.

The endpoint and token come from `--admin` and `--token`, then `TOLLBOOTH_ADMIN` and
`TOLLBOOTH_TOKEN`, then the config file's `admin.address` and `admin.token_file`.

## Task: export chargeback data

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

`tollbooth doctor --config tollbooth.yaml` checks the environment these tasks depend on (ulimits,
`GOMEMLIMIT`, the TLS pair, every backend, the clock, and `showback.dir`'s filesystem and headroom).

## Throttling and client retry modes

tollbooth throttles; it does not queue (hold-and-forward is Phase 6, ADR-0004). A denied request
gets `503 SlowDown` (or `429 TooManyRequests`) with `Retry-After` in seconds and
`x-amz-retry-after` in milliseconds, and the client's own retries decide whether the work
completes. Measured with the real tools against a rule whose rate is half the client's
concurrency and whose burst equals it (`docs/THROTTLE.md`, test plan category 7):

| Client | Default retry mode | Adaptive retry mode |
| --- | --- | --- |
| aws cli v2 | **fails**: 80 of 200 objects landed, exit 1 | completes |
| boto3 | **fails**: 190 of 200 landed | completes |
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
   `AWS_MAX_ATTEMPTS`. Adaptive mode paces itself to the rule and completed every run measured.
3. Wait for hold-and-forward (Phase 6), which queues a denied request up to a bound instead of
   answering 503.

Expect a herd: N clients denied together receive the same `Retry-After` and retry together, so
N − 1 are denied again. Adaptive clients spread out on their own; default-mode clients do not.

What to watch: `tollbooth_limiter_denied_total{rule}` (who is being throttled),
`GET /admin/limits/top` or `tollbooth rules top` (which keys), and the client's own error output
(`SlowDown` after max attempts means lever 1 or 2 above).
