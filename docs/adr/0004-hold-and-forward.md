# ADR-0004: Hold-and-forward (shaping) mode

Status: **stub, Phase 6**. Opened at Gate 2 (2026-11-09) so the field evidence is on record; the
options, queue bounds, and client-timeout analysis are written when the item is pulled in.

## Context

v1 throttles only: a denied request is answered `503 SlowDown` or `429 TooManyRequests` with
`Retry-After` and `x-amz-retry-after` (D-043), and the client's own retries do the smoothing.
Phase 2 measured what that means for the clients in the field (`docs/THROTTLE.md`, D-044):

- Against a rule whose rate is half the client's concurrency and whose burst equals it, **aws cli
  v2 lands 80 of 200 objects and boto3 190 of 200 in their default retry mode**. Both complete in
  adaptive mode (`AWS_RETRY_MODE=adaptive`); s5cmd, rclone, and s3cmd complete with their defaults.
- The Go SDK as shipped ignores both Retry-After headers and gives up after three random backoffs;
  its 2026 retry behavior honors `x-amz-retry-after` and completes deterministically.
- `Retry-After` names the instant the next single token is available, so N clients denied together
  retry together and N − 1 are denied again (herd retries).

In plain terms: **a rate limit set below a client's concurrency makes aws cli fail, not slow
down.** Operators have three levers today: set rates above client concurrency, have clients use
adaptive retry mode or raise `AWS_MAX_ATTEMPTS`, or wait for this mode. That is the evidence this
ADR exists to weigh; it is not to be relitigated from scratch.

## To be decided here

Per-rule queue depth and the memory it pins; the maximum hold before a request is denied anyway
(it must stay inside the client's own timeout, 1 s for `Expect: 100-continue` bodies); how a held
request is counted (`requests_total{result="shaped"}` and `limiter_shaped_wait_seconds{rule}`,
both defined in `docs/telemetry-catalog.md` and registered only with this mode, D-045); whether
the hold applies before or after the identity's body is read (before, or the memory bound is gone);
interaction with `max_inflight`; and the fail-open behavior when the queue is full.

## Consequences

None until accepted. Nothing in v1 is shaped; the `shaped` result value exists at zero because the
specification fixes the label set.
