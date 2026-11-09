# S3 Rate-Limit & Showback Proxy — Development Prompt for Opus

Nov 8, 2026 · @Blake Golliher

## How to use this prompt

Paste everything from **Mission and scope** onward into Opus as the opening message of the build session, or commit it as `docs/SPEC.md` and open the session with "Read docs/SPEC.md and begin Phase 0." Three roles: Blake owns the product and arbitrates; Fable owns the design, writes the prompts, and assesses every gate report; Opus implements.

Rules of engagement for Opus:

- Work phase by phase (see **Phased delivery plan**). At each gate, write the gate report (see **Gate report format**) and stop. Do not begin the next phase until told to.
- **Decisions already made** are fixed. To challenge one, write an ADR with the alternative and the evidence, then stop; do not implement the alternative.
- **Open decisions** each get an ADR in Phase 0 with a recommendation. Stop and ask after writing them.
- Ambiguity elsewhere: if a wrong guess is cheap to redo, take the simplest reading that satisfies the text, record it in `docs/DECISIONS.md`, and continue. If a wrong guess is expensive (storage schema, wire behavior, public config keys), ask first.
- Never weaken, skip, or mark-as-flaky a test to pass a gate. A failing test is a finding; report it.
- Report numbers, not adjectives. "Fast" is not a result; "p99 412 µs at 48k req/s on 16 vCPU, run 3 of 3" is.
- Commits: Conventional Commits, small and single-purpose, no AI attribution trailers of any kind (no Co-Authored-By, Generated-with, or session trailers).

Context Opus holds throughout: the author is a field CTO at a storage vendor whose customers (GPU neoclouds and enterprises) run very large S3 workloads. The proxy must work in front of any S3-compatible backend but will be validated against VAST clusters, with MinIO and Garage as local stand-ins. The author's earlier Go S3 proxy, [shunt](https://github.com/blakegolliher/shunt) (Apache-2.0), already solves passthrough streaming, request classification, and raw-URI handling in its passthrough mode. shunt is not to be modified for this project; its packages are copied in under the procedure in Lift from shunt (see **Licensing**). Protocol-level fluency is expected; do not explain S3 basics back to the author.

## Mission and scope

Working name: `tollbooth` (a placeholder: a tollbooth meters traffic and counts it; rename freely, spend no time on it).

`tollbooth` is a transparent inline HTTP(S) reverse proxy between S3 clients and an S3-compatible backend. Every request and response passes through byte-for-byte. The proxy adds exactly two capabilities:

1. **Rate limiting**, enforced inline on every S3 operation, keyed by any combination of `access_key`, `bucket`, `bucket + prefix`, and `source_ip`, scoped by S3 operation or operation class.
2. **Showback**: hourly counts of API calls (plus bytes in and out) by the same dimensions, durable, additive across proxy nodes, and queryable. This is the input to SLA reporting and chargeback by API call; it is not a real-time reporting engine.

Priorities, in order. When two conflict, the higher one wins:

1. **Transparency**: a client cannot tell the proxy exists except by being throttled.
2. **Availability**: the proxy is never the reason storage is unreachable. Fail open.
3. **Latency**: microseconds of added overhead, not milliseconds.
4. **Accuracy**: limits and counts are correct within stated, tested bounds.
5. Features.

Out of scope. Do not build these, and do not leave hooks for them that cost latency on the hot path:

- Object storage, caching, request re-signing, credential storage, and authentication or authorization decisions. The backend does all of these.
- Inspecting or modifying request or response bodies, ever.
- Real-time analytics or dashboards. Showback is hourly aggregation with a query interface.
- Load balancing or migration as a product (shunt covers that). Only the minimal multi-endpoint support that resilience needs.
- Bandwidth (bytes/s) limits in v1. The rule schema must leave room for them.
- A web UI. Operators get a CLI and a terminal UI with full parity (see Operator surface); configuration is a file plus an admin API.

## Decisions already made

These are fixed. Opus implements them as written; a disagreement becomes an ADR, not a different implementation.

| Decision | Choice | Why |
| --- | --- | --- |
| Language | Go, current stable toolchain, pinned in `go.mod` | Matches shunt (Apache-2.0), whose passthrough pipeline and request parsing are lifted; `net/http` is production-grade for streaming proxies; fastest path for LLM-driven development. Rust is the fallback only if Phase 5 misses the p99 targets, decided by ADR at Gate 5. |
| Dependencies | Standard library first. Allowed: `log/slog` (stdlib), `prometheus/client_golang`, `go.yaml.in/yaml` (the archived `gopkg.in/yaml.v3` must not appear in the binary; `go version -m` is the check, since the module graph is not controllable), `pires/go-proxyproto` (Apache-2.0) for the PROXY protocol, `hdrhistogram-go` in `bench/` only, and `charmbracelet/bubbletea`, `bubbles`, `lipgloss` (MIT) imported only from `cmd/tollbooth/tui`. No database driver. Anything else needs a one-line justification in `docs/deps.md` | Every dependency is attack surface, build friction, and a license to track. |
| Proxy core | Hand-written handler modeled on shunt's passthrough pipeline: one handler, no middleware framework, `Host` preserved, bodies streamed with `io.CopyBuffer` through a pooled fixed-size buffer, per-chunk idle deadlines via `http.ResponseController` (shunt's `internal/proxy/copy.go`), and a body cut short aborts the client connection (`panic(http.ErrAbortHandler)`, shunt ADR-0003) so a truncated object is never delivered as complete | Proven against VAST, Garage, and versitygw in shunt and fuzzed there. `httputil.ReverseProxy` would be a second integration with its own `Host` and raw-URI quirks to re-learn. |
| Signature handling | Pure passthrough. Never re-sign, never touch a signed header, never buffer a body. The proxy holds no credentials | Re-signing adds latency and makes the proxy a credential store; neither is wanted here. shunt re-signs because it changes backends; `tollbooth` does not. |
| Throttle response | HTTP 503 with the S3 `SlowDown` XML error body and a `Retry-After` header. Per-rule option: HTTP 429 `TooManyRequests`. Reject only; no hold-and-forward (shaping) mode in v1 | 503 `SlowDown` is how S3 itself signals throttling, so every AWS SDK and CLI retries it with backoff out of the box, and that backoff is the burst smoothing. A hold mode needs a per-rule queue, timers, and an interaction with client timeouts that can double load when the client gives up and retries; it is Phase 6 if the field asks for it. |
| Rule evaluation | One pass over the matching rules in config order, consuming a token from each; the first denial throttles the request. Tokens already taken are not returned | A denied request still counts against the rules that allowed it, which is what S3 does. This removes a two-phase check, its check-then-consume race, and any rollback code. |
| Failure posture | Fail open, always: any limiter or showback fault forwards the request and raises a metric. No per-rule or global fail-closed knob | A rate limiter that takes storage offline is worse than no rate limiter, and the only limiter-internal fault that happens in practice is the key cap. The recover guard re-panics `http.ErrAbortHandler`, so fail-open never completes a truncated body. |
| Limiter algorithm | GCRA per key: one `int64` theoretical arrival time, tolerance `(burst − 1) × T` so exactly `burst` requests pass from a full bucket, 256 shards each guarded by a mutex (a decision is one lock, one map lookup, one compare-and-set), struct keys of existing substrings (no concatenation, no hashing of keys), monotonic clock. A key whose TAT is at or before now has a full bucket and is evicted by the sweeper without loss | Exact token-bucket semantics in 8 bytes per key with no timers and no refill goroutine. Striped locks, not lock-free: Go maps need a lock, and the decision budget has room for one. |
| Multi-node limits | Node-local, with a `cluster.node_count` divisor the operator sets. No cluster coordination, and no schema provisions for one, in v1 | A remote round-trip per decision (the aws-samples pattern with DynamoDB) breaks the latency budget and adds a dependency to fail on. Designing for a future mode now is speculation. |
| Showback store | Flat files: the current hour in memory, snapshotted whole every `flush_interval` by atomic rename; sealed hours as NDJSON under `usage/YYYY/MM/DD/`; counts are additive so a cross-node merge is a sum. No database | No dependency, no schema migrations, nothing to corrupt; a billing system or DuckDB reads NDJSON directly. Parquet export is a Phase 6 option. |
| Showback counting | Every request is counted once, at completion, in memory, throttled and unforwardable ones included; bytes are wire bytes actually read and written; unknown operations pass through and count as `Unknown` | Chargeback by API call does not care about `aws-chunked` framing overhead, and rejecting an unknown operation would break the next S3 API addition a customer adopts. |
| Time | UTC everywhere. Hour bucket = `floor(unix_seconds / 3600)`. Monotonic clock for limiter math, wall clock only for bucketing | Chargeback needs unambiguous hours; limiter correctness must survive NTP steps. |
| Addressing | Both path-style and virtual-hosted-style; vhost needs a configured list of base domains | VAST and MinIO deployments use both; SDK defaults vary. |
| Source IP | TCP peer address by default; PROXY protocol v1/v2 optional on the listener via `go-proxyproto`; `X-Forwarded-For` honored only from configured trusted CIDRs | XFF is forgeable; a tenant must not be able to pick its own rate-limit bucket. A hand-written PROXY parser is another fuzz target for no gain. |
| TLS | Listener: optional termination from cert and key files, hot-reloadable, TLS 1.2 minimum. Backend: `http` or `https` with a configurable CA bundle and a lab-only `insecure_skip_verify` | Plaintext inside the data center is a per-site decision (same as shunt). |
| Configuration | One YAML file, validated as a whole, published as an immutable value behind one `atomic.Pointer` on `SIGHUP` or `POST /admin/reload`. Invalid config keeps the running one. `node_id` and `showback.dir` are required and `showback.dir` must be a persistent path. Nothing in tollbooth ever writes the operator's config file; rules change by editing it and reloading | Rules change at runtime; a typo must never drop traffic. shunt carries the same rule (its config package: it must never rewrite the operator's config), and it keeps a human editor's comments and ordering intact. |
| Deployment | Single static binary; systemd unit; distroless container image. N identical instances behind an L4 load balancer or DNS round-robin; no shared state required to start | Same two targets as shunt (bare metal and RKE2/k8s). |
| Logging and metrics | `log/slog` JSON; Prometheus `/metrics` on the admin port; access log off by default (showback is the record of record). No tracing in v1 | High-volume access logs compete with the data path for I/O. OpenTelemetry is Phase 6. |
| Project license | Apache-2.0 | Matches shunt and the author's public tooling. |

## Lift from shunt

shunt (`github.com/blakegolliher/shunt`, branch `master`, commit `9b36d09` at the time of writing, Apache-2.0) already contains roughly a third of this project. Its packages are all under `internal/`, so they are copied, not imported. shunt itself is not modified for this project; the reverse integration (tollbooth's packages into shunt) is a later project and does not shape this one.

| shunt source | Take | Adapt |
| --- | --- | --- |
| `internal/s3/op.go` with `TestOpNamesComplete`, `TestEveryRow`, `TestPrecedence` | The `Op` enum (95 operations) with classifier precedence and its tests | Regenerate from `ops.yaml` so the YAML stays the single source of truth; add the operation classes |
| `internal/s3` `Parse(r, Domains)` → `RequestInfo{Bucket, Key, Op, Style, Query}`, `NewDomains`, `ValidBucketName`, `testdata/fuzz/FuzzParse` | Bucket and key extraction for both addressing styles, with its fuzz corpus | Extend with `access_key` and `source_ip`. shunt's passthrough mode never parses the credential, so the access-key extractor is new |
| `internal/s3/errors.go` with `Render` and `Write` | The S3 error table (`SlowDown`, `ServiceUnavailable`, …) and XML rendering | Add `TooManyRequests` and the `Retry-After` header |
| `internal/sigv4` `RawPath`, `RawQuery`, `Scope` | Raw-URI preservation and `Credential=` scope parsing | Parse only. tollbooth never calls `CanonicalRequest`, `Sign`, or verifies anything |
| `internal/proxy/handler.go`, passthrough path: `Host` preserved, pooled `io.CopyBuffer`, `roundTrip` retry on connect error before any body byte, `ErrAbortHandler` on short read | The streaming data path and its two hardest-won rules | Strip every resign-mode branch, admission gate, directory lookup, and cross-cluster path; a few hundred lines should remain |
| `internal/upstream` (`Cluster`, endpoints, `IsConnectError`, health checks) | Backend pool | Keep as is |
| `internal/runtimecfg` (`Publisher`, `Bundle`) | Nothing | shunt's bundle-and-drain machinery exists because its bundles own cluster connections. tollbooth's config is an immutable value behind one `atomic.Pointer`, and limiter state lives inside the limiter keyed by rule id, so a pointer swap is the whole mechanism |
| `internal/telemetry` (`Metrics`, `StatusClass`, `OperationClass`, request IDs, the `Access` record) | Metrics scaffolding and a per-request record that already carries client, bucket, op, status, bytes in and out, duration | `Access` becomes the showback aggregator's input; drop the migration and window counters |
| `internal/listener` | TLS listener | Add PROXY protocol and trusted `X-Forwarded-For`, which shunt's listener doc defers |
| `test/s3diff`, `test/e2e`, `test/bench` | Byte-equivalence diff harness against Garage and versitygw, e2e scenarios, HDR latency harness | Starting point for Test plan categories 5, 6, 9, and 10 |
| `CLAUDE.md`, `docs/adr/`, `docs/deps.md`, `docs/telemetry-catalog.md`, `docs/STATUS.md`, `docs/reuse.md`, `.github/workflows/ci.yml` | The working framework: rules, ADR form, dependency ledger, metric catalog, phase status, lift procedure, CI | Copy the shape, rewrite the content for tollbooth |

Lift procedure, from shunt's `docs/reuse.md`, applied to every copied file including ones from the author's own repo:

1. Copy the file into the target package and keep the original license header verbatim.
2. Directly under the header add `// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, <date>: <one line on what changed>.` Apache-2.0 §4(b) requires the modification notice.
3. Append shunt's `LICENSE` (and `NOTICE`, if one exists at the commit copied) to `THIRD_PARTY_NOTICES.md`.
4. Add a line to `docs/deps.md`: source repo, path, commit SHA, license, why the stdlib was not enough.
5. The G4 audit at every gate checks all of the above.

## Open decisions for Phase 0

Each gets an ADR in `docs/adr/` (context, options, recommendation, consequences). Write all of them, then stop and ask. The recommendation column is Fable's starting position, not a constraint.

| # | Question | Options | Fable's recommendation |
| --- | --- | --- | --- |
| 1 | What does a prefix rule match for list operations? | (a) only object operations, by key; (b) also `ListObjectsV2`/`ListObjects`/`ListObjectVersions`/`ListMultipartUploads` by their `prefix` query parameter, longest configured prefix that is a prefix of the request's prefix | (b). A list storm on `ckpt/` should count against `ckpt/`. |
| 2 | Load generator for the bench harness | (a) in-repo Go generator on `aws-sdk-go-v2` with HDR histograms; (b) MinIO `warp` only; (c) both, with `warp` as the cross-check | (c). The in-repo generator gives microsecond-resolution overhead measurement; `warp` is an independent sanity check (run as an external binary only; see Licensing). |

Closed during the skeptical review and moved into **Decisions already made**, so Phase 0 does not spend time on them: cluster coordination (node-local, no hedging), shaping (reject only; Phase 6), bytes accounting (wire bytes only), showback durability (whole-hour snapshot, no WAL), cardinality overflow (one `__overflow__` row, no staged collapse), and unknown operations (pass through and count).

## Request identity

Every request yields one immutable `RequestIdentity`, computed from headers and the request line only, before a single body byte is read. The limiter and showback both key off it.

| Field | Type | Source and rules |
| --- | --- | --- |
| `access_key` | string, `""` if anonymous | SigV4 header: `Authorization: AWS4-HMAC-SHA256 Credential=<AKID>/<date>/<region>/s3/aws4_request, SignedHeaders=..., Signature=...` → `<AKID>`. SigV4 presigned: query `X-Amz-Credential=<AKID>/...` (percent-decode once). SigV2 header: `Authorization: AWS <AKID>:<sig>`. SigV2 presigned: query `AWSAccessKeyId=<AKID>`. Temporary keys (`ASIA...`) are just keys. |
| `bucket` | string, `""` for service-level ops | Virtual-hosted: `Host` (port stripped) ends with `.<base domain>` for a configured base domain → the bucket is everything before that suffix, dots included. Otherwise path-style: first path segment. A bare IP, the proxy's own name, or no match → path-style. |
| `key` | string | Path after the bucket, percent-decoded exactly once per RFC 3986 (`+` is a literal plus in a path, not a space). Keys may contain `%`, `//`, spaces, and arbitrary Unicode; none of these may be normalized. |
| `source_ip` | `netip.Addr` | TCP peer by default; PROXY protocol header when enabled on the listener; rightmost untrusted `X-Forwarded-For` entry only when the peer is in `trusted_proxies` CIDRs. |
| `operation`, `op_class` | enums | From the classifier (next section). |
| `addressing` | enum | `path` or `vhost`, for diagnostics only. |

Invariants, each enforced by a test:

- The data listener's handler is the proxy handler itself, never an `http.ServeMux`: the mux cleans `//` and `..` out of paths and issues redirects, which would rewrite object keys. The request-target is taken from `r.RequestURI` (shunt's `sigv4.RawPath` and `RawQuery`) and placed in `URL.Opaque` on the outbound request so it reaches the backend byte-identical; Go's URL parsing will normalize `%2F` into `/` if you let it, and that breaks the signature.
- The outbound request is built the way shunt's `handler.go` builds it: the inbound header map is reused (not cloned), hop-by-hop headers (`Connection`, `Keep-Alive`, `Proxy-*`, `TE`, `Transfer-Encoding`, `Upgrade`) are stripped per RFC 7230, `ContentLength` is copied, `r.Trailer` is forwarded, and `Host` is the client's `Host`, exactly as sent, in both addressing styles. It is almost always in `SignedHeaders`.
- The proxy adds no headers to the request. No `X-Forwarded-For`, no `X-Forwarded-Proto`, no `Via`. The backend sees the request as the client sent it; client identity for audit lives in showback, not in the request.
- `Content-Length` and `Transfer-Encoding` semantics pass through unchanged; `aws-chunked` bodies, including their trailing checksums, stream as opaque bytes.
- The limiter decision is made before the body is read.
- With `Expect: 100-continue`, the proxy answers a throttled request before touching the body, so Go's server never sends `100 Continue`. Clients wait at most their expect-continue timeout (1 s in Go's transport and in the AWS SDKs) before sending the body anyway, so the 503 must land inside that window; at microseconds of decision time it does, and the test asserts it. Go then closes the connection, because one with an unread body cannot be reused, so under a throttle storm clients reconnect with a TLS handshake each time. Expected cost, not a bug; Phase 2 measures it against aws cli, boto3, and s5cmd and records the result.
- Response headers (`ETag`, `x-amz-request-id`, `x-amz-id-2`, `x-amz-version-id`, checksum headers) pass through unchanged. The proxy adds nothing to responses.
- Object keys and access keys are never written to logs above `debug` level.

Deployment constraint, learned in shunt (ADR-0001): because `Host` is preserved, the backend receives the proxy's hostname as the virtual-host domain. VAST, or any backend, must be configured to accept that hostname as a vhost domain, or clients must use path-style addressing. `tollbooth check-config` warns when a backend endpoint name differs from the configured base domains, and `docs/OPERATIONS.md` opens with this.

## S3 operation classifier

The classifier is a pure, table-driven function: `(method, has_bucket, has_key, query keys, selected headers) → Operation`. The table is shunt's `internal/s3/op.go`, lifted as Go code, and that Go table is the single source of truth: `docs/OPERATIONS_TABLE.md` is generated from it by a test that fails when the two drift. No YAML schema and no code generator. Opus checks the table against the [Amazon S3 API reference](https://docs.aws.amazon.com/AmazonS3/latest/API/API_Operations_Amazon_Simple_Storage_Service.html), adds anything missing, and records the reference's version date in the file header.

Matching precedence, highest first: (1) a subresource query key (`uploads`, `uploadId`, `tagging`, `acl`, `versioning`, `delete`, `attributes`, `restore`, `select`, `location`, `list-type`, `versions`, `session`, and the rest); (2) a distinguishing header (`x-amz-copy-source`); (3) request shape (service, bucket, or key). `partNumber` is a qualifier, not a subresource: with `uploadId` it makes `UploadPart`, alone on a GET or HEAD it leaves the operation as `GetObject`/`HeadObject` and is recorded as a flag, as is `versionId`. The AWS SDK v2 family sends an `x-id=<Operation>` query parameter; use it as a tiebreaker and a test oracle, never as the sole source.

The rows below are the ones that matter most for limits and chargeback; shunt's table already has nearly all of them.

| Method | Shape | Qualifier | Operation | Class |
| --- | --- | --- | --- | --- |
| GET | service | — | ListBuckets | LIST |
| GET | bucket | — | ListObjects (v1) | LIST |
| GET | bucket | `list-type=2` | ListObjectsV2 | LIST |
| GET | bucket | `versions` | ListObjectVersions | LIST |
| GET | bucket | `uploads` | ListMultipartUploads | LIST |
| GET | key | `uploadId=` | ListParts | LIST |
| GET | key | — (`versionId`, `partNumber`, `Range` are flags) | GetObject | READ |
| HEAD | key | — | HeadObject | READ |
| GET | key | `attributes` | GetObjectAttributes | READ |
| GET | key | `tagging` / `acl` / `retention` / `legal-hold` / `torrent` | GetObjectTagging / Acl / Retention / LegalHold / Torrent | READ |
| POST | key | `select&select-type=2` | SelectObjectContent | READ |
| PUT | key | — | PutObject | WRITE |
| PUT | key | header `x-amz-copy-source`, no `uploadId` | CopyObject | WRITE |
| POST | bucket | `multipart/form-data` body, no subresource | PostObject (browser form upload) | WRITE |
| POST | key | `uploads` | CreateMultipartUpload ("NewMultipartPut") | WRITE |
| PUT | key | `uploadId=` + `partNumber=` | UploadPart | WRITE |
| PUT | key | `uploadId=` + `partNumber=` + `x-amz-copy-source` | UploadPartCopy | WRITE |
| POST | key | `uploadId=` | CompleteMultipartUpload | WRITE |
| PUT | key | `tagging` / `acl` / `retention` / `legal-hold` | PutObjectTagging / Acl / Retention / LegalHold | WRITE |
| POST | key | `restore` | RestoreObject | WRITE |
| PUT | key | `renameObject` (directory buckets) | RenameObject | WRITE |
| DELETE | key | — | DeleteObject | DELETE |
| POST | bucket | `delete` | DeleteObjects | DELETE |
| DELETE | key | `uploadId=` | AbortMultipartUpload | DELETE |
| DELETE | key | `tagging` | DeleteObjectTagging | DELETE |
| HEAD | bucket | — | HeadBucket | BUCKET |
| PUT | bucket | — | CreateBucket | BUCKET |
| DELETE | bucket | — | DeleteBucket | BUCKET |
| GET | bucket | `location` | GetBucketLocation | BUCKET |
| GET | bucket | `session` | CreateSession (S3 Express) | BUCKET |
| GET / PUT / DELETE | bucket | any other subresource (`acl`, `cors`, `encryption`, `lifecycle`, `logging`, `notification`, `policy`, `policyStatus`, `replication`, `requestPayment`, `tagging`, `versioning`, `website`, `object-lock`, `publicAccessBlock`, `ownershipControls`, `inventory`, `metrics`, `analytics`, `intelligent-tiering`, `metadataTable`, ...) | Get / Put / Delete Bucket\<Subresource> | BUCKET |
| any | any | nothing above matches | Unknown (method and shape recorded) | UNKNOWN |

Classes exist so a rule can say "2,000 WRITE/s" without listing fourteen operations, and so showback can roll up. They mirror how S3 itself partitions its own limits (PUT/COPY/POST/DELETE versus GET/HEAD per prefix). A rule may name classes, individual operations, or both.

## Rate limiting

A rule has three parts: **match** (which requests it applies to), **group\_by** (how many independent token buckets it creates), and **limit** (rate and burst). All matching rules must allow a request; if any denies, the request is throttled.

```yaml
rate_limits:
  default_on_limit: slowdown    # slowdown (503 SlowDown) | too_many_requests (429)
  rules:
    - id: tenant-a-writes
      match:                    # every listed condition must hold (AND); omitted = any
        access_keys: [AKIAEXAMPLE1, AKIAEXAMPLE2]
        buckets: [training-data, checkpoints]
        prefixes: [ckpt/, logs/]        # relative to the bucket; longest match wins
        source_cidrs: [10.20.0.0/16]
        ops: [WRITE]                    # classes and/or operation names
      group_by: [access_key, bucket]    # one GCRA state per distinct tuple
      limit: { rate: 2000, burst: 4000 }   # requests per second, bucket capacity
    - id: list-storm-per-client
      match: { ops: [LIST] }
      group_by: [source_ip]
      limit: { rate: 50, burst: 200 }
      on_limit: too_many_requests
    - id: frozen-key
      match: { access_keys: [AKIAREVOKED] }
      limit: { rate: 0 }                  # rate 0 blocks outright
```

Semantics:

- **Key of a bucket** = `rule.id` + the values of the `group_by` dimensions. `prefix` as a `group_by` dimension means the matched configured prefix, not the raw object key. An empty `group_by` means one shared bucket for everything the rule matches.
- **Evaluation** is one pass over the matching rules in config order, consuming a token from each. The first denial ends the pass, throttles the request, and decides the response mode (that rule's `on_limit`); tokens already taken from earlier rules stay taken. A denied request therefore still counts against every rule that would have allowed it, which is how S3 behaves and removes any need for a check-then-consume phase or a rollback.
- **No matching rule** = unlimited. A request matched by five rules must pass all five.
- **Match cost** must be O(number of rules) with precomputed matchers: exact-set lookups for keys and buckets, a per-bucket sorted prefix list with longest-match, a `netip` prefix table for CIDRs, a bitmask for ops. Budget: 10 µs p99 with 100 rules loaded.
- **Rate 0** blocks. Rates are per node unless `cluster.node_count` is set, in which case each node enforces `rate / node_count` and `burst / node_count`.
- **Failure** inside the limiter (a panic, the key cap) always fails open and increments a metric. There is no fail-closed knob: the only limiter-internal fault that can happen in practice is the key cap, and refusing storage because a map is full is the wrong trade.

GCRA, per key: emission interval `T = 1/rate` seconds; tolerance `tau = (burst − 1) × T`, so exactly `burst` requests are admitted from a full bucket (with `burst × T` it would be `burst + 1`, and the tests check this); state `TAT` (theoretical arrival time, int64 monotonic nanoseconds). Allow if `now ≥ TAT − tau`, then `TAT = max(TAT, now) + T`. Deny otherwise, with `wait = (TAT − tau) − now` for `Retry-After`. `rate: 0` is a separate branch that always denies with `Retry-After: 1`; there is no infinite `T`. Keys are struct values `{rule, a, b, c string}` holding the identity's existing substrings, so a lookup allocates nothing and two different tenants can never collide the way a hashed key could; on first insert the strings are `strings.Clone`d so request buffers are not pinned. Keys live in 256 shards (`map[key]int64`, one mutex each, shard chosen by key hash); a decision is one lock, one lookup, one compare-and-set. A background sweeper deletes any key whose `TAT ≤ now`: that bucket is already full, so re-creating it on the next request is indistinguishable from having kept it, which makes eviction exact rather than a TTL guess. Live keys are capped at `limiter.max_keys` (default 2,000,000); past the cap, new keys fail open and `tollbooth_limiter_keys_overflow_total` increments, so a flood of unique keys cannot exhaust memory.

Enforcement modes:

- `slowdown`: HTTP 503, `Content-Type: application/xml`, body `<Error><Code>SlowDown</Code><Message>Please reduce your request rate.</Message><Resource>/bucket/key</Resource><RequestId>…</RequestId><HostId>…</HostId></Error>`, headers `Retry-After: <ceil(wait) seconds, min 1>`, `x-amz-retry-after: <ceil(wait) milliseconds>` (the exact wait, which aws-sdk-go-v2 honors; added at Phase 2, D-043), and `x-amz-request-id`. Every AWS SDK retries this with exponential backoff.
- `too_many_requests`: HTTP 429, same body shape with code `TooManyRequests`, same headers. For clients whose retry policy is keyed on 429.
- There is no hold-and-forward mode in v1. If a customer needs smoothing beyond SDK backoff, that is a Phase 6 ADR with the queue bounds and client-timeout analysis it deserves.
- Rule identifiers never appear in client-facing responses. Tenants should not learn each other's policy names, and there is no knob to change that.

Hot reload: build the complete new rule set, validate it, and swap it with one atomic pointer store. Limiter state is keyed by rule id and is never reset on reload: a `TAT` is a timestamp, so a changed rate or burst simply applies from the next decision, and the state of removed rules is swept. Requests in flight during the swap finish under whichever rule set they started with. Reload must complete in under 100 ms with no failed requests under load, and that is a test.

## Showback

Showback produces one row per hour per distinct dimension tuple, holding counts that are additive across nodes and across time. Every request that reaches the proxy is counted once, in memory, at completion, including the ones the proxy throttled or could not forward. What reaches disk is bounded by the snapshot interval described below, and that bound is the only durability promise.

| Column | Type | Meaning |
| --- | --- | --- |
| `hour_utc` | int64 | `floor(completion_unix / 3600)`. A request is attributed to the hour it completed in, because counting happens once, at completion. Attributing by start time would leave a long GET with nowhere to land once its start hour is sealed. |
| `node_id` | string | Required in config; `check-config` refuses a missing value. A hostname default would be a pod name in Kubernetes, different on every restart, which breaks both cross-node merging and restart recovery. |
| `access_key` | string | From identity; `""` for anonymous. |
| `bucket` | string | `""` for service-level operations. |
| `prefix` | string | The matched showback prefix for the bucket, or `""` (bucket root). Showback prefixes are configured per bucket (`showback.prefixes: {bucket: [a/, b/c/]}`), longest match. Arbitrary key depth is never recorded. |
| `source_ip` | string | Text form of the identity's address. Present only when `source_ip` is in `showback.dimensions`. |
| `operation` | string | Classifier output, including `Unknown`. |
| `status_class` | string | `2xx`, `3xx`, `4xx`, `5xx` from the backend; `throttled` (proxy denied); `proxy_error` (backend unreachable or proxy fault). SLA math needs these separated. |
| `requests` | int64 | Count. Each `UploadPart` is one request; multipart uploads are never reassembled into logical objects. |
| `bytes_in` | int64 | Request body bytes actually read from the client. An aborted upload counts what arrived. |
| `bytes_out` | int64 | Response body bytes actually written to the client. A client that disconnected mid-download is charged what was sent. |

Headers are excluded from byte counts. Latency is not a showback column; it belongs in Prometheus histograms.

Hot path: on request completion the proxy increments the tuple's counters in an aggregator built like the limiter's key store: struct keys of existing substrings, 256 mutex-guarded shards, counters as plain `int64` fields updated under the shard lock (held for nanoseconds; no atomics, no lock-free structures). The request goroutine never waits on storage. Budget: 2 µs p99 per request, and turning showback on must cost under 2% of peak request rate versus off.

Snapshot and seal: the whole in-memory hour is written every `showback.flush_interval` (default 60 s) to a temp file and renamed over `current/<hour>-<node_id>.ndjson`, so a crash loses at most one interval and a restart reads that file back into memory and continues. There is no delta merge and nothing to make idempotent: the snapshot is the state. Because attribution is by completion time, the moment the clock crosses an hour boundary the old hour has no more writers, so it is sealed at once: final snapshot, rename to `usage/YYYY/MM/DD/HH-<node_id>.ndjson`. No grace period, no reopen path. An hour with no requests still produces a file with a zero-row header, so a missing file means the node was not running, which `verify` reports as downtime rather than as a gap in the data. A wall-clock step backward never reopens or overwrites a sealed file.

Storage: one directory per node (`showback.dir`, required, on a persistent path; `check-config` refuses a missing value and `doctor` warns when it sits on tmpfs or an ephemeral volume), no database. Each file is NDJSON, one row per line, with a first line of metadata (`node_id`, `hour_utc`, row count, `sum(requests)`, `partial` true or false) so a consumer can verify completeness without a sidecar. Files are additive across nodes, DuckDB and any warehouse read NDJSON directly, and that is how a customer's billing system will consume them. Retention `showback.retention_days` deletes old files.

Cardinality: `showback.dimensions` (default `[access_key, bucket, prefix, operation, status_class]`; add `source_ip` to track it) and `showback.max_tuples_per_hour` (default 250,000; roughly 200 bytes each in memory). When the cap is hit, every further new tuple folds into one row per `(hour, operation, status_class)` with the other dimensions set to `__overflow__`, and `tollbooth_showback_overflow_total` increments; one log line per hour, not per request.

Query interface:

- CLI: `tollbooth usage query --from 2026-11-08T00:00Z --to 2026-11-09T00:00Z --group-by access_key,bucket --where bucket=checkpoints --format table|csv|json` over a directory or glob of exports from one or many nodes. It does filter, group, and sum, and nothing else; anything fancier is `duckdb -c "select ... from read_ndjson('usage/**/*.ndjson')"`, and the docs say so. The open hour is included and marked `partial` in both the file header and the query output.
- Admin API: `GET /admin/usage?from=&to=&group_by=&where=&format=` over this node's directory only, same code path.
- `tollbooth usage verify <dir>` recomputes each file's header and reports, per node, hours that are missing (node not running), partial, or whose header does not match the rows.

Reconciliation is the acceptance test: a load generator with a known request mix runs through the proxy, and after the hour closes `sum(requests)` and the byte totals in the store must equal the generator's own counts exactly for completed requests, with throttled requests landing under `status_class = throttled`.

## Data path and resilience

The request path is: accept → parse headers → identity → classify → limiter decision → reject, shape, or forward → stream the body to the backend → stream the response to the client → record showback on completion. Every decision is made before the body is read; the backend sees the request as the client sent it.

&#91;embedded content: request path · 6 stages, 1 decision, showback off the hot path\]

The limiter decision sits between the classifier and the forward step; a denied request never reaches the body-streaming stage, and showback is written on completion without blocking the next request.

Timeouts and limits, all configurable, defaults in parentheses: `ReadHeaderTimeout` (10 s); `IdleTimeout` on client keep-alives (120 s); no whole-request timeout, because a 5 TiB GET is legitimate, but an idle-body timeout in each direction when no bytes move (60 s), done the way shunt's `internal/proxy/copy.go` does it: `http.ResponseController.SetReadDeadline` and `SetWriteDeadline` pushed forward on every copied chunk; backend dial (2 s); backend response-header timeout (300 s, because a `ListObjectsV2` over a hundred-million-object bucket can legitimately take that long); `MaxHeaderBytes` (1 MiB); max in-flight requests (4,096, excess gets 503 `SlowDown` counted as `proxy_error`; with the 128 KiB copy buffer this bounds copy memory at 512 MiB, Gate 0 correction); backend keep-alive pool `MaxIdleConnsPerHost` (1,024). Response headers are written and flushed the moment the backend's arrive; the body is one `io.CopyBuffer` loop through the pooled buffer. Nothing is buffered.

Backend pool: a list of endpoints with round-robin selection, active health checks (TCP connect plus `HEAD /`, where any HTTP status including 403 means healthy), and passive marking on dial errors. A request is retried on another endpoint only when the failure was a dial or TLS handshake error before any request byte was sent. Never retry after bytes have gone out, regardless of HTTP method. HTTP/1.1 only, on both sides, like shunt's listener.

| Failure | Behavior |
| --- | --- |
| Limiter panic or internal error | Recover, fail open, forward, `tollbooth_limiter_errors_total` increments. The recover guard re-panics `http.ErrAbortHandler`: swallowing it would return normally and, on a chunked response, write the terminating chunk, which delivers a truncated body as complete. |
| Showback store unavailable (disk full, I/O error) | Keep aggregating in memory up to the tuple cap; log once per minute; never block a request. Counts are flushed when the store returns. |
| Config reload invalid | Keep the running config; log the error; `GET /admin/config` shows the last reload error; `tollbooth_config_reload_total{result="error"}` increments. |
| All backends unreachable | HTTP 503 with S3 `ServiceUnavailable` XML and `x-amz-request-id`; counted as `proxy_error`. SDKs retry this. |
| Client disconnects mid-upload | Cancel the backend request; count `bytes_in_wire` as read. |
| Backend resets mid-download | Close the client connection (the response is already partially written); count `bytes_out` as sent; `status_class = 5xx`. |
| `SIGTERM` | Stop accepting; drain in-flight up to `drain_timeout` (30 s); flush showback; exit 0. |
| Wall-clock step backward | Limiter is unaffected (monotonic clock). Showback never reopens or overwrites a closed hour; late rows go to the current hour with `tollbooth_showback_clock_skew_total` incremented. |
| Flood of unique keys or IPs | Limiter key cap and showback tuple cap hold memory; overflow fails open and collapses, respectively, with metrics. |
| Slowloris clients | `ReadHeaderTimeout` and the in-flight cap bound the damage; per-source connection cap is optional. |

Security posture: runs as an unprivileged user; stores no credentials; admin port binds to localhost or a management interface and takes an optional bearer token; TLS 1.2 minimum; no request or response body is ever read into memory beyond the streaming buffer.

## Performance targets and benchmark harness

Targets are measured on a reference node (16 vCPU, 64 GiB, 25 GbE or faster) against the null backend described below, so the proxy is the only thing under test. "Added latency" means proxied minus direct, same client, same backend, interleaved runs.

| Measure | Target | Stretch |
| --- | --- | --- |
| Added latency, 4 KiB `GetObject`, 100 rules loaded, plaintext | p50 ≤ 150 µs, p99 ≤ 1 ms, p99.9 ≤ 5 ms | p99 ≤ 500 µs |
| Small-op throughput per node, plaintext, 100 rules, showback on | ≥ 50,000 req/s | ≥ 100,000 req/s |
| Small-op throughput per node, TLS terminated | ≥ 30,000 req/s | ≥ 60,000 req/s |
| Large-object streaming, 32 concurrent 64 MiB GETs | ≥ 80% of NIC line rate; CPU per GB reported | ≥ 90% |
| Limiter decision, 100 rules, 1,000,000 live keys | ≤ 10 µs p99 | ≤ 3 µs p99 |
| Showback increment | ≤ 2 µs p99; ≤ 2% throughput cost on versus off | ≤ 1% |
| Memory | ≤ 1 GiB RSS with `GOMEMLIMIT=1GiB` at 1,000,000 limiter keys and 250,000 showback tuples (the default cap). The measured per-tuple cost (expect 350 to 450 bytes, not 200) goes into the sizing section of `docs/OPERATIONS.md` | ≤ 512 MiB |
| Config reload under full load | ≤ 100 ms, zero failed requests |  |
| Startup to ready | ≤ 1 s |  |

Harness, in `bench/`:

- **Null backend**: an in-repo Go HTTP server that speaks enough S3 to answer every operation with a canned response of configurable size and injected latency, records the exact bytes it received (for the byte-equivalence tests), and can be told to fail, stall, or reset.
- **Load generator**: in-repo, two modes. A raw `net/http` client firing presigned URLs for the latency rows, because SDK signing and middleware add roughly 100 µs of variance that would drown a 150 µs target; and an `aws-sdk-go-v2` mode for realistic operation mixes and the throttle path. HDR histograms, configurable operation mix, key distribution (how many access keys, buckets, prefixes, source addresses via multiple bind IPs), and a `direct` versus `proxied` switch. MinIO `warp` runs as an external cross-check (Open decision 2).
- **Method**: pin CPU sets for proxy, generator, and backend; 30 s warm-up; three runs; report the median run with all three attached. Record `go version`, kernel, NIC, `GOMAXPROCS`, `GOGC`, `GOMEMLIMIT`, and the exact config file.
- **Profiles**: CPU and allocation pprof captured during the peak run, flame graphs saved under `bench/results/<date>/`. A gate report without profiles is incomplete.
- **Regression**: a short benchmark set (classifier, matcher, GCRA, aggregator increment) runs in CI on a fixed runner with a ±10% threshold against the committed baseline, with `-benchmem` so an allocation on the decision path fails the build.

## Observability and admin API

The admin listener (default `127.0.0.1:9900`) is separate from the data listener and never shares its connection pool.

| Endpoint | Purpose |
| --- | --- |
| `GET /healthz` | Process is up. |
| `GET /readyz` | Config loaded and at least one backend healthy. |
| `GET /metrics` | Prometheus exposition. |
| `POST /admin/reload` | Same as `SIGHUP`; returns the validation result. |
| `GET /admin/config` | Effective config with secrets redacted, plus the last reload result and timestamp. |
| `GET /admin/limits` | Rules in force, live key count per rule, denied and shaped counts since start. |
| `GET /admin/limits/top?n=20` | Top denied keys in the last 60 s, for "who is getting throttled right now". |
| `GET /admin/usage?...` | Showback query over this node's store (see Showback). |
| `GET /debug/pprof/*` | Only when `admin.pprof: true`. |

Prometheus metrics (prefix `tollbooth_`). Labels never carry an access key, IP, bucket, or object key; that is showback's job and would blow up the time-series database.

- `requests_total{operation, op_class, status_class, result}` where `result` ∈ `forwarded | throttled | shaped | error`
- `request_duration_seconds{op_class}` (client-observed), `proxy_overhead_seconds{op_class}` (accept to first byte sent to backend), `backend_ttfb_seconds{op_class}`
- `bytes_total{direction}`
- `limiter_decision_seconds`, `limiter_keys`, `limiter_keys_overflow_total`, `limiter_denied_total{rule}`, `limiter_shaped_wait_seconds{rule}`, `limiter_errors_total`
- `showback_tuples`, `showback_flush_duration_seconds`, `showback_flush_errors_total`, `showback_collapsed_total`, `showback_clock_skew_total`
- `backend_up{endpoint}`, `backend_errors_total{endpoint, kind}`, `backend_retries_total`
- `config_reload_total{result}`, `inflight_requests`, `connections_open{side}`
- Go runtime and process metrics from the client library defaults

Logging: `log/slog` JSON to stdout. One startup line with version, config path, listeners, backends, rule count. Reload results at `info`. Throttle decisions at `debug`, sampled (1 in N, configurable) so a throttle storm cannot flood the log. Access logging exists (`logging.access: true`) but is off by default and documented as a debugging aid, not an accounting source.

No tracing in v1. OpenTelemetry is a Phase 6 item; metrics and the showback record cover what operators need until then.

## Operator surface: CLI and TUI

tollbooth has no web UI. Every operator function is reachable two ways, the CLI (`tollbooth <verb>`) and a terminal UI (`tollbooth tui`), and both go through one admin API client (`internal/adminclient`), so parity is structural rather than a checklist. Anything the TUI can do has a verb; anything a verb can do has a key.

Decisions:

- One binary. `tollbooth tui` is a subcommand. Bubble Tea is imported only from `cmd/tollbooth/tui`; a `depguard` rule in `golangci-lint` forbids charmbracelet imports anywhere else. Same reasoning as shunt ADR-0009 (the AWS SDK in the binary, kept off the data path); record it as an ADR.
- Framework: `charmbracelet/bubbletea`, `bubbles`, and `lipgloss`, all MIT. Each gets a `docs/deps.md` line and its copyright notice in `THIRD_PARTY_NOTICES.md`. No second TUI library.
- Neither the CLI nor the TUI writes the config file. Rules change by editing the file and running `tollbooth config reload` (or pressing `r` in the TUI); the Limits screen offers read-only views and a dry-run test, not an editor. This is shunt's rule (its config package must never rewrite the operator's config) and it removes atomic rewrites, comment preservation, and a race with a human editing the same file.
- The TUI is read-only and off the data path: it polls the admin API (default every 2 s, `--interval`), works over SSH at 80×24, is keyboard-only, honors `NO_COLOR` and `TERM=dumb`, and when the admin API is unreachable shows the last state under a staleness banner rather than exiting.
- Every CLI verb supports `--format table|json|csv`; `json` is the admin API response unwrapped, so scripts never screen-scrape. Failures exit non-zero with one line on stderr.
- Admin endpoint and bearer token come from `--admin` and `--token`, then `TOLLBOOTH_ADMIN` and `TOLLBOOTH_TOKEN`, then the config file.

CLI verbs:

| Verb | Does | Backed by |
| --- | --- | --- |
| `serve` | Run the proxy |  |
| `check-config [file]` | Validate a config file, including a warning when a backend endpoint name is not among the base domains (the vhost constraint); exit 1 on any error, unknown keys included | local |
| `doctor` | Environment checks: ulimits, `GOMAXPROCS` and `GOMEMLIMIT`, TLS material, backend TCP and HTTP reachability, clock sync, disk headroom for the showback directory. It does not guess whether a backend accepts the base domains; that is a configuration fact the operator asserts | local and backends |
| `status` | One-screen summary: uptime, node id, in-flight, req/s, throttled/s, backend health, config generation, last reload result | `GET /admin/status` |
| `rules list`, `rules show <id>` | Rules in force with live key counts and denied counters | `GET /admin/limits` |
| `rules test --access-key K --bucket B --key P --ip A --op OP` | Dry run: which rules match, the bucket key each resolves to, and the decision now, without consuming tokens | `POST /admin/limits/test` |
| `rules top [--n 20] [--window 60s]` | Top denied keys right now | `GET /admin/limits/top` |
| `config show`, `config reload`, `config diff` | Effective config (redacted) with the last reload result; trigger a reload; diff the running config against the file on disk | `GET /admin/config`, `POST /admin/reload` |
| `backends list` | Health, in-flight, error counters, TTFB per endpoint | `GET /admin/backends` |
| `usage query --from --to --group-by --where` | Showback filter, group, and sum over a directory or glob of exports | files |
| \`usage top --from --to --by access\_key | bucket | source\_ip --n 20\` |
| `usage verify <dir>` | Recompute file headers and report missing hours per node | files |
| `version` | Version, commit, build date, Go version |  |

TUI screens (`tollbooth tui`), one key each, `?` for help on any screen:

| Screen | Key | Shows | Actions |
| --- | --- | --- | --- |
| Overview | `1` | The `status` summary; req/s and throttled/s sparklines built from the last five minutes of polls; backend health; config generation and last reload result | `r` reload, `q` quit |
| Limits | `2` | Rules table: id, match summary, `group_by`, rate and burst, mode, live keys, denied/s. Detail pane: top denied keys for the selected rule | `enter` detail, `t` test-request form (the `rules test` fields), `/` filter |
| Usage | `3` | Hour-range picker, group-by toggles, sortable table of rows with requests and bytes, hourly bars across the range for the selected row | `g` cycle group-by, `s` sort column, `/` filter, `x` export the current view as CSV |
| Backends | `4` | Endpoints, health, in-flight, error counters, TTFB | none; read-only |
| Config | `5` | Effective config (redacted), diff against the file on disk, last reload error | `r` reload, `v` validate the file on disk |

Admin API additions for this surface: `GET /admin/status`, `POST /admin/limits/test`, `GET /admin/backends`. All three are reads of state the proxy already holds; the operator surface adds no behavior to the data path.

Parity and tests: `docs/OPERATOR_SURFACE.md` is a hand-written matrix of admin route × CLI verb × TUI screen, and a test over the admin route table fails if any route is missing from it. Bubble Tea models are tested with `charmbracelet/x/exp/teatest` (MIT) golden frames at 80×24 and 120×40, including the admin-unreachable state. Every CLI verb has golden tests in all three formats.

## Phased delivery plan

Six phases, each ending at a gate that Fable reviews before the next begins. One branch per phase, one PR per feature inside it. Passthrough correctness comes before any feature, because every feature is worthless if a signature breaks.

&#91;embedded content: roadmap · 6 phases, 6 gates\]

Phase 6 is a backlog, not a scheduled phase; each item in it starts with its own ADR when Blake pulls it in.

| Phase | Deliverables | Gate criteria |
| --- | --- | --- |
| **0 Design** (no product code) | `docs/DESIGN.md` (architecture, data structures, hot-path walkthrough with allocation budget); one ADR per open decision; shunt's op table checked against the API reference; repo scaffold, `Makefile`, CI skeleton, `THIRD_PARTY_NOTICES.md`; `docs/CONFIG.md` with the full config schema and defaults (the key names used throughout this document are canonical) | Fable approves the design and ADRs; every requirement in this document maps to a section of `DESIGN.md` or an explicit "deferred to Phase N". |
| **1 Passthrough** | Transparent proxy lifted from shunt's passthrough pipeline (see **Lift from shunt**), plus TLS, PROXY protocol, trusted XFF; identity extraction; classifier from shunt's op table; backend pool with health checks; admin `/healthz` `/readyz` `/metrics` with request metrics; null backend and recording backend in `bench/` | Byte-equivalence suite green; client compatibility matrix 100% green against Garage and versitygw; fuzz 1 CPU-hour clean; `go test -race` clean. |
| **2 Rate limiting** | Rule schema and loader; matchers; GCRA; single-pass evaluation; `slowdown` and `too_many_requests`; `Expect: 100-continue` behavior measured; hot reload; limiter metrics and `/admin/limits`; `rules list`, `rules show`, `rules test`, `rules top` CLI verbs | Limiter correctness suite green (fake clock); 10 µs p99 decision microbenchmark met; reload under load with zero failures; SDK clients complete work under a 1 req/s rule via their own retries. |
| **3 Showback** | Aggregator; snapshot and seal; NDJSON files with headers; `tollbooth usage` CLI verbs and `/admin/usage`; dimensions and overflow cap; restart from snapshot; multi-node sum | Reconciliation test exact; deterministic-clock rollover tests green; kill between snapshots loses ≤ `flush_interval`; verify tool detects a removed hour. |
| **4 Operator surface** | Remaining CLI verbs (`status`, `config`, `backends`, `doctor`); the shared admin client; admin API additions (`/admin/status`, `/admin/limits/test`, `/admin/backends`); `tollbooth tui` with the five screens (see **Operator surface**) | Parity matrix test green (every admin route has a CLI verb and a TUI screen); CLI golden tests in all three output formats; `teatest` goldens; an operator who has never seen the code completes the runbook's rule-change (edit file, reload, verify in TUI) and chargeback-export tasks from the TUI and CLI alone. |
| **5 Hardening and v0.1.0** | Full bench runs with profiles; tuning; 24 h soak at 50% of peak with chaos injections; resource caps; `docs/OPERATIONS.md` runbook; systemd unit; container image; `CHANGELOG.md`; tagged release | All targets in **Performance targets** met or each miss has an ADR (including the Go-versus-Rust call); soak shows flat memory and zero unexplained errors. |
| **6 Backlog** (not scheduled) | Hold-and-forward (shaping) mode; cluster-coordinated limits; bandwidth limits; Parquet export; OpenTelemetry; per-source connection caps; rule editing from the CLI/TUI; backend drain; a web UI only if the TUI proves insufficient in the field | Each item starts with its own ADR. |

## Test plan

Every category below is required; none may be skipped or reduced to pass a gate. All Go tests run with `-race`. Coverage floor: 85% of statements overall, and every `Op` in the classifier table exercised by the classifier tests (the table test enumerates the enum, so a new operation without a test row fails the build).

| # | Category | What it must prove |
| --- | --- | --- |
| 1 | Classifier unit tests | Table-driven over every `Op` in the table × path and vhost addressing × with and without `versionId`/`partNumber` × SigV4 header, SigV4 query, SigV2 header, SigV2 query, anonymous. Golden file of expected `(operation, class, flags)`. `x-id` present and absent. |
| 2 | Identity unit tests | Percent-encoding edge cases (`%2F`, `%25`, `+`, spaces, Unicode, `//`, trailing `/`); `Host` with port, IPv6 literal, missing `Host`; bucket names with dots under vhost; XFF from trusted and untrusted peers; PROXY protocol v1 and v2 framing via `go-proxyproto`. |
| 3 | Limiter unit tests | GCRA with an injected clock: exactly `burst` requests admitted from a full bucket and the next denied; exact token count over 10 s equals `burst + rate × 10`; `rate: 0` always denies; two rules in config order, where a denial by the second still charges the first and the test asserts that charge; eviction of full buckets is lossless (evict, re-request, same decision); reload never resets state; 64 goroutines on one key never exceed the bound; the decision allocates zero bytes (`-benchmem`). |
| 4 | Fuzzing | `go test -fuzz` over the request parser and classifier (shunt's corpus as the seed): never panics, classification is deterministic, raw path round-trips byte-for-byte. One CPU-hour in nightly CI, 60 s on every PR. Corpus committed. |
| 5 | Byte-equivalence | Start from shunt's `test/s3diff`. The recording backend captures the full request bytes direct and via the proxy; the diff must be empty except for the allowed added headers. Cases: `aws-chunked` upload with trailing checksum, presigned GET and PUT, SigV2, `Expect: 100-continue`, `Range` GET, every multipart step, `DeleteObjects` with `Content-MD5`, every `x-amz-checksum-*` algorithm, keys with every edge case from category 2. Response bytes compared the same way. |
| 6 | Client compatibility matrix | aws cli v2, boto3, `aws-sdk-go-v2`, s5cmd, rclone, s3cmd, elbencho against Garage and versitygw in CI (pinned static binaries; MinIO and `mc` dropped at Gate 0 because MinIO archived its open-source server, D-022) and against a VAST cluster in the lab (manual, results committed). Operations: `cp`, `sync`, `ls`, `rm`, `mb`, `rb`, presign, multipart ≥ 5 parts, `--part-number` GET and HEAD, `GetObjectAttributes`, every checksum algorithm. Output: `docs/COMPAT.md` as a tool × operation matrix with pass/fail per backend. |
| 7 | Throttling behavior | Each SDK, with its default retry settings, completes a 200-object sync against a rule whose `rate` is half the client's concurrency and whose `burst` equals it, and showback shows the throttled count; the 503 body parses as an S3 error in every SDK; `Retry-After` is honored where the SDK supports it; a throttled `Expect: 100-continue` PUT sends zero body bytes (asserted on the recording backend, with the 503 landing inside the client's 1 s expect timeout) and the connection-close-and-reconnect cost is measured and recorded; 429 mode behaves the same. |
| 8 | Showback correctness | Injected clock. A request starting at 10:59:59.9 and ending at 11:00:00.1 lands in hour 10. Kill between snapshots loses ≤ `flush_interval`; restart loads the snapshot and continues with no double count. Reconciliation: generator counts equal file counts exactly for completed requests, throttled requests land under `throttled`, `bytes_out` for an aborted download equals bytes actually written. Sum of three nodes' files equals the generator total. Overflow folds into `__overflow__` as specified. `verify` detects a deleted hour file. |
| 9 | Chaos and resilience | All backends down; one backend down; backend TTFB 30 s; backend reset mid-body; showback directory full or read-only; invalid reload under load; `SIGTERM` under load drains; slowloris; 1 MiB headers; 10,000 new unique keys per second for 60 s (RSS stays under cap); clock step forward and back. Each has an expected behavior from the failure table and a test asserting it. |
| 10 | Performance regression | Microbenchmarks for classifier, matcher, GCRA, aggregator increment in CI with ±10% thresholds. Full load tests in Phase 5 per **Performance targets**. |
| 11 | Static analysis | `go vet`, `staticcheck`, `golangci-lint` (config committed, including the `depguard` rule), `govulncheck`, `gosec`, Semgrep, CodeQL, `go-licenses check`. All clean on every PR. |
| 12 | Operator surface | Parity: every registered admin route appears in `docs/OPERATOR_SURFACE.md` with a CLI verb and a TUI screen, asserted by a test over the route table. CLI golden tests for every verb in `table`, `json`, and `csv`. TUI models under `teatest` with golden frames at 80×24 and 120×40, including the admin-unreachable state. `depguard` fails the build if charmbracelet is imported anywhere but `cmd/tollbooth/tui`. |

Test infrastructure lives in `test/integration/` with `backends.sh` (pinned static Garage and versitygw binaries) and a `make compat` target. Lab runs against VAST are scripted the same way with endpoint and credentials from environment variables, never committed.

## Engineering quality bar

Module path `github.com/blakegolliher/tollbooth` (rename with the project). Layout:

```
cmd/tollbooth/          main; subcommands serve, status, rules, config, backends, usage, check-config, doctor, tui, version
cmd/tollbooth/tui/      Bubble Tea models and views (the only place charmbracelet is imported)
internal/s3id/          identity + classifier (shunt's op table, lifted)
internal/limiter/       rules, matchers, GCRA
internal/showback/      aggregator, snapshot, seal, query, verify
internal/proxy/         listener, TLS, PROXY protocol, backend pool, passthrough handler (lifted from shunt)
internal/config/        schema, loader, validation, atomic publish
internal/admin/         admin HTTP server, metrics registry, and the client the CLI and TUI share
bench/                  null backend, recording backend, load generator, results/
test/                   s3diff, e2e, integration backends (Garage, versitygw), lab runner
docs/                   DESIGN.md, adr/, deps.md, telemetry-catalog.md, STATUS.md, CONFIG.md, OPERATIONS.md,
                        OPERATOR_SURFACE.md, COMPAT.md, SHOWBACK_SCHEMA.md, OPERATIONS_TABLE.md, reports/
deploy/                 systemd unit, Dockerfile, example configs
```

Six Go packages plus the commands and the TUI, all under `internal/`. Nothing is designed for a hypothetical importer: when the shunt integration happens, moving a package to an importable path is a rename, and designing for it now would push option structs and injected dependencies into code that has one caller.

Practices:

- `Makefile` targets: `build`, `lint`, `test`, `race`, `fuzz`, `bench`, `compat`, `release`. CI (GitHub Actions) runs lint, unit with race, integration against Garage and versitygw, 60 s fuzz, `govulncheck`, `go-licenses`, CodeQL, Semgrep on every PR; nightly adds the one-hour fuzz and the regression benchmarks. CodeRabbit reviews PRs.
- No panics on the hot path; one recover guard at the top of the handler that fails open and counts. Errors are wrapped with context; no `log.Fatal` after startup.
- No allocation in the limiter decision or showback increment paths beyond what the profiler shows is unavoidable; `go test -bench` with `-benchmem` reports allocs/op and they are part of the regression thresholds.
- Every config key is documented in `docs/CONFIG.md` with type, default, and one example; `tollbooth check-config` validates a file without starting.
- `docs/OPERATIONS.md` is a runbook: install, configure, reload, upgrade without dropping connections, sizing guidance (keys and tuples to memory), what each metric means and what to do when it moves, how to run a chargeback export.
- Definition of done for a feature: code, tests in every applicable category, metrics, config docs, and an ADR if a decision was made. A PR missing any of these is not done.
- Versioning is semver; `tollbooth version` prints version, commit, build date, and Go version. `CHANGELOG.md` is kept by hand.
- Commits follow Conventional Commits, are small and single-purpose, and carry no AI attribution trailers.

Rules carried over from shunt's `CLAUDE.md`, which go into tollbooth's `CLAUDE.md` verbatim:

- One binary, `tollbooth`, with subcommands `serve`, `status`, `rules`, `config`, `backends`, `usage`, `check-config`, `doctor`, `tui`, `version`. The TUI is a subcommand, not a second binary (see **Operator surface**).
- Standard library first. Every dependency has one line in `docs/deps.md` saying why the stdlib was not enough.
- No interface with a single implementation. If Parquet export arrives in Phase 6, a store seam is introduced then, not now.
- No middleware framework, no DI container, no plugin system. One handler, one pipeline, explicit calls.
- Any buffering of a request or response body requires an ADR and a benchmark. `io.CopyBuffer` through a pooled fixed-size buffer is streaming, not buffering.
- Every package has tests and at least one benchmark; `-race` in CI; a fuzz target for every parser (classifier, identity, config, NDJSON reader).
- Feature flags default off, live in one file, and each has a removal criterion. Kill switches default to the normal behavior and document what breaks when flipped.
- Config is validated at startup and by `check-config`; unknown keys are errors. Nothing in tollbooth writes the config file.
- Every metric is in `docs/telemetry-catalog.md` before it is implemented, and a cardinality test guards the label set.
- Nothing assumes a NIC, a CPU model, or a kernel feature beyond what `doctor` checks and a fallback covers.
- Never pipe a build, test, fuzz, or benchmark run through `head`, `tail`, `grep`, or any other filter. Redirect the full output to a file and read the file. shunt lost evidence three times this way.
- No per-object state, ever. Limiter keys and showback tuples are keyed by configured dimensions, never by object key.
- A phase is done when its gate passes, not when the code compiles.

## Licensing and attribution

The project is Apache-2.0. Opus checks the license of every outside artifact before using it and records it; a dependency with an unknown license is not added.

- `THIRD_PARTY_NOTICES.md` lists every module in `go.sum` that ends up in the binary: name, version, license, URL. `go-licenses check` runs in CI with a deny list for GPL, LGPL, AGPL, and SSPL. Only permissive licenses (Apache-2.0, MIT, BSD-2/3, ISC, MPL-2.0 at file scope) may be linked.
- Apache-2.0 dependencies: keep their `LICENSE` and `NOTICE` files in the notices; mark any modified file as changed; preserve copyright headers. MIT and BSD dependencies: preserve the copyright notice.
- Code adapted from [shunt](https://github.com/blakegolliher/shunt) (Apache-2.0, same author) still follows Apache-2.0 form: keep its header, mark the file as modified, note the origin in the notices.
- Copyleft tools are run, never linked or vendored: elbencho (GPL-3.0), MinIO server and `warp` (AGPL-3.0), Garage (AGPL-3.0). They appear only as external processes in `test/integration/` and `bench/`, pulled by Docker image or system package, never copied into the repo.
- [aws-samples/fine-grained-rate-limit-demo](https://github.com/aws-samples/fine-grained-rate-limit-demo) is MIT-0 (no attribution required). It informed the `UsagePlan` idea of rate plus burst and the lesson that a per-request round-trip to a shared store is the wrong shape for an inline proxy. No code is taken from it.
- AWS documentation is referenced by link; its text is not copied into the repo beyond operation and error-code names.

## Gate report format

At each gate Opus writes `docs/reports/gate-N.md` with exactly these headings, in this order, and then stops. Fable grades it; Blake decides. Nothing in the report is prose for its own sake.

1. **Scope delivered** — table: planned item, status (done / partial / deferred), PR or commit.
2. **Decisions and assumptions** — links to ADRs written this phase; every entry added to `docs/DECISIONS.md`.
3. **Test results** — exact commands run; pass, fail, and skip counts per category from the **Test plan**; coverage percentage; race and fuzz durations; link to `docs/COMPAT.md`. A skipped category is listed as skipped with the reason, never omitted.
4. **Performance** (Phases 2 to 5) — table of target versus measured per row of **Performance targets**, environment description, path to raw results and profiles.
5. **Known gaps and risks** — each with a proposed fix and the phase it belongs to.
6. **Questions** — numbered, each with Opus's recommended answer, so a reply can be "1 yes, 2 no, 3 option b".
7. **Reproduce** — the commands that regenerate every number above from a clean checkout.

Fable's rubric for each gate, scored pass / pass with fixes / fail: correctness against this specification; completeness of scope; test rigor (do the tests prove the invariants or merely exercise code paths); honesty of performance claims (methodology, variance, profiles present); code quality (hot-path allocations, error handling, naming, package boundaries); documentation (could an operator who has never seen the code deploy and run a chargeback export). A fail at any gate means the phase is reworked before the next one starts.

Before writing the gate report, Opus runs shunt's four guardrail passes, adapted, and pastes their real output into the report. Each is a separate prompt, run in order:

1. **G1 Simplicity review.** List every exported type, interface, and function added this phase and name its second caller; delete or unexport anything with only one, except the named seam. List every dependency added with its `docs/deps.md` line and license; remove any the stdlib could replace in under 100 lines. Search for any place a body is buffered (`bytes.Buffer`, `io.ReadAll`, `[]byte` copies); each needs an ADR or goes. Confirm every feature flag is default off with a removal criterion and every kill switch defaults to normal behavior. Report LOC added, LOC deleted, and test-to-code ratio; if the ratio dropped, explain why. Make the deletions, then show the diff summary.
2. **G2 Validation.** Run, in order, redirecting full output to files and pasting it unsummarized: `make lint`, `go vet ./...`, `go test -race ./...`, `make fuzz` (60 s per target), `test/s3diff` against Garage and versitygw, `make bench-compare` against the previous phase's baseline, this phase's e2e scenarios. For each failure, fix the code, not the test, unless the test is wrong, and show why before changing it. A benchmark regression over 5% blocks the phase.
3. **G3 Telemetry and docs.** For every new code path, name the metric, log field, or showback column that observes it; anything unobserved is instrumented from the catalog, and anything new gets a catalog entry first. Confirm the catalog and cardinality tests pass. Update `docs/CONFIG.md` for every key, `docs/STATUS.md`, and the ADRs. If anything this phase contradicted this specification, say so explicitly and propose the edit; never diverge silently.
4. **G4 Dependency and license audit.** Run `go-licenses` and `govulncheck`; regenerate `THIRD_PARTY_NOTICES.md`. For each dependency: license, and whether it is MIT/BSD (preserve notice), Apache-2.0 (preserve LICENSE and NOTICE, mark modified files), or copyleft (stop and report). Confirm every lifted file, including those from shunt, has its origin, commit, and license in `docs/deps.md`. Confirm no code was copied from MinIO, Garage, or warp. Confirm YAML is `go.yaml.in/yaml`, including transitively.

Phase close: update `docs/STATUS.md` with what shipped, what was cut and why, the bench numbers, and the gate result; write the PR description (summary, decisions with ADR links, how to verify, known gaps); tag the commit `phase-<n>`; then say what you would change in this specification based on what you learned.

## References

- [Amazon S3 API operations](https://docs.aws.amazon.com/AmazonS3/latest/API/API_Operations_Amazon_Simple_Storage_Service.html): the source for `ops.yaml`; the reference also lists S3 Control, Tables, Vectors, and Files operations, which are out of scope.
- [S3 error responses](https://docs.aws.amazon.com/AmazonS3/latest/API/ErrorResponses.html): `SlowDown` and `ServiceUnavailable` error body shape.
- [Best practices: optimizing Amazon S3 performance](https://docs.aws.amazon.com/AmazonS3/latest/userguide/optimizing-performance.html): S3's own published per-prefix rates of at least 3,500 PUT/COPY/POST/DELETE and 5,500 GET/HEAD requests per second, with 503 Slow Down during scaling; the model for operation classes.
- [Authenticating requests (SigV4) for S3](https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-authenticating-requests.html): `Authorization` header and presigned query formats the identity extractor parses.
- [Virtual hosting of buckets](https://docs.aws.amazon.com/AmazonS3/latest/userguide/VirtualHosting.html): path-style versus virtual-hosted-style addressing.
- [shunt](https://github.com/blakegolliher/shunt) (Apache-2.0): the author's prior Go S3 proxy and the source of the lifted packages. Its [ADRs](https://github.com/blakegolliher/shunt/tree/master/docs/adr), in particular 0001 (two auth modes and the passthrough vhost constraint), 0003 (a short body is never delivered as complete), and 0007 (every held buffer has a cap), and its [`CLAUDE.md`](https://github.com/blakegolliher/shunt/blob/master/CLAUDE.md) rules.
- [versitygw](https://github.com/versity/versitygw) (Apache-2.0): S3 gateway used as a byte-equivalence and compatibility backend alongside Garage.
- [aws-samples/fine-grained-rate-limit-demo](https://github.com/aws-samples/fine-grained-rate-limit-demo) (MIT-0): Python and DynamoDB token and leaky bucket demo with a `UsagePlan(rate_limit, burst_limit)` abstraction and sharded buckets.
- [Generic cell rate algorithm](https://en.wikipedia.org/wiki/Generic_cell_rate_algorithm): the limiter's math.
