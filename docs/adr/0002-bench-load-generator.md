# ADR-0002: Load generator for the bench harness

Status: **proposed** (Phase 0, 2026-11-08). Open decision 2 in docs/SPEC.md. Awaiting Fable and Blake.

## Context

The performance targets (docs/SPEC.md, Performance targets) are stated as *added* latency,
proxied minus direct, with a p50 of 150 µs and a p99 of 1 ms for a 4 KiB `GetObject`, plus
throughput, streaming, reload-under-load, and memory rows. Several of those numbers are smaller
than the measurement noise of an SDK-based client: aws-sdk-go-v2 signing plus middleware adds on
the order of 100 µs of variance per request, which would drown the p50 target. The harness also
has to drive the throttle path realistically (SDK retries on 503) and produce the exact request
counts the showback reconciliation test compares against.

## Options

**(a) In-repo Go generator only**, two modes: a raw `net/http` client firing presigned URLs
(microsecond-resolution overhead measurement, no signing on the hot path) and an aws-sdk-go-v2
mode for realistic operation mixes and the throttle path. HDR histograms via `hdrhistogram-go`
(MIT, `bench/` only). Knows the exact counts it sent, so it is the showback oracle.

- For: full control of the key distribution (access keys, buckets, prefixes, source addresses via
  multiple bind IPs), the `direct` versus `proxied` switch with interleaved runs, pinned CPU sets,
  and the counts reconciliation needs.
- Against: a generator written by the same hands as the proxy shares its blind spots.

**(b) MinIO `warp` only.** Mature, widely compared against, independent of this codebase.

- For: independence; operators recognize its output.
- Against: AGPL-3.0, so external binary only, never code; latency resolution and the operation
  mix are warp's, not ours; no access-key or source-address distribution; no exact per-run counts
  exposed in a form the reconciliation test can consume; cannot drive presigned URLs without
  signing in the client.

**(c) Both, warp as the cross-check.** The in-repo generator produces the gate numbers; warp runs
as an external binary (Docker image or system package) against the same proxy and backend and its
throughput and latency are reported alongside, with the delta explained.

## Recommendation

**(c).** The in-repo generator is required by the targets themselves (presigned raw mode for the
latency rows, exact counts for reconciliation, key distributions for the cardinality tests); warp
is cheap to run and catches the case where the generator and the proxy agree with each other and
disagree with the world. warp never enters the repository: `bench/warp/` holds a script and a
pinned image tag, nothing else (docs/SPEC.md, Licensing).

## Consequences

- `bench/loadgen/` (Phase 2): `-mode raw|sdk`, `-target direct|proxied`, `-ops <mix>`,
  `-keys/-buckets/-prefixes/-sources <n>`, `-bind <ip,...>`, `-duration`, `-warmup 30s`,
  `-runs 3`, HDR output as text and as JSON under `bench/results/<date>/`.
- `bench/warp/run.sh` (Phase 5): pinned `minio/warp` image tag, same operation mix where warp can
  express it, output saved next to the generator's.
- `docs/deps.md` gains `hdrhistogram-go` (MIT, bench only) and, in Phase 2,
  `aws-sdk-go-v2` + `smithy-go` (Apache-2.0, bench and test only; never linked into `bin/tollbooth`,
  checked by `go version -m` in `make build`).
- `make bench` runs the microbenchmarks; `make bench-e2e` (Phase 2) runs the generator; the gate
  report's Performance section cites both and the warp cross-check from Phase 5.
