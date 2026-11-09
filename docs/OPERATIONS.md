# Operations

The runbook is written in Phase 5 (install, configure, reload, upgrade, sizing, every metric and
what to do when it moves, chargeback export). Phase 2 contributes the one section operators need
before then, because it changes how a rule is sized.

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
