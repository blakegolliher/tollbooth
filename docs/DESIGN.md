# tollbooth — design

Status: Phase 0 draft, 2026-11-08. The specification is `docs/SPEC.md`; this document says how
each requirement in it is met, with data structures, the hot-path walkthrough, and an allocation
budget. Section 14 maps every specification section to a section here or to an explicit deferral.
Where this document and the specification disagree, the specification wins and the disagreement is
a bug here.

Priorities, in order, applied whenever two requirements conflict: transparency, availability,
latency, accuracy, features.

## 1. What it is

A transparent inline HTTP(S) reverse proxy between S3 clients and one S3-compatible backend
(a pool of endpoints for one cluster). Every request and response passes through byte-for-byte.
It adds two capabilities: inline rate limiting keyed by `access_key`, `bucket`, `bucket + prefix`,
and `source_ip`, scoped by operation or operation class; and hourly showback of API calls and
bytes by the same dimensions, durable as flat files, additive across nodes, queryable from the
CLI and the admin API.

It is not an object store, cache, signer, authenticator, analytics engine, load balancer, or web UI
(docs/SPEC.md, Out of scope). It never reads or modifies a body, never re-signs, holds no
credentials, and fails open.

## 2. Repository layout and package boundaries

```
cmd/tollbooth/          main; subcommands serve, status, rules, config, backends, usage, check-config, doctor, tui, version
cmd/tollbooth/tui/      Bubble Tea models and views (the only place charmbracelet is imported; ADR-0003)
internal/s3id/          RequestIdentity + classifier (shunt's op table, lifted)
internal/limiter/       rules, compiled matchers, GCRA key store, sweeper, deny sampler
internal/showback/      aggregator, snapshot, seal, NDJSON reader, query, verify
internal/proxy/         data listener (TLS, PROXY protocol, trusted XFF), backend pool, passthrough handler
internal/config/        schema, loader, whole-file validation, atomic publish, features.go
internal/admin/         admin HTTP server, metrics registry, and the client the CLI and TUI share
bench/                  null backend, recording backend, load generator (ADR-0002), results/
test/                   s3diff, e2e, integration backends (Garage, versitygw as pinned binaries), lab runner
docs/                   this file, adr/, CONFIG.md, SHOWBACK_SCHEMA.md, OPERATIONS_TABLE.md, telemetry-catalog.md, …
deploy/                 systemd unit, Dockerfile, example config
```

Dependency direction: `cmd` → `admin`, `proxy`, `config`; `proxy` → `s3id`, `limiter`, `showback`,
`config`; `admin` → `limiter`, `showback`, `config` (read-only views). `s3id`, `limiter`, and
`showback` import only the standard library (plus `prometheus/client_golang` for the few metrics
they own). Nothing imports `cmd/tollbooth/tui`. No package defines an interface with one
implementation; the one planned seam (a showback store for Parquet) is Phase 6 and does not exist.

## 3. The request path

```
accept ─► parse headers (net/http) ─► identity ─► classify ─► limiter decision ─┬─► deny: 503/429 XML, done
                                                                                 └─► forward: build outbound ─► RoundTrip ─► relay headers ─► stream body ─► record (showback, metrics)
```

Six stages, one decision, and showback off the critical path of the *next* request: the record
step runs in the request's own goroutine after the last byte is written (it costs that request
≤ 2 µs and blocks nothing else). Every decision is made before a single body byte is read.

## 4. Data path (`internal/proxy`, `internal/s3id`)

### 4.1 Listener

- `net.Listen("tcp", listen.address)`; optional `tls.NewListener` with `MinVersion` from
  `listen.tls.min_version`, `NextProtos: ["http/1.1"]` (no h2 on either side), and
  `GetCertificate` reading an `atomic.Pointer[tls.Certificate]` that reload replaces, so
  certificates hot-reload without a new listener. Lifted shape: shunt `internal/listener`.
- `listen.proxy_protocol: true` wraps the listener in `proxyproto.Listener` (`pires/go-proxyproto`,
  Apache-2.0) with `ConnPolicy` set to *require* a header: a connection without one is closed. The
  wrapped `net.Conn`'s `RemoteAddr()` is the client's address, so `r.RemoteAddr` is already right
  and nothing downstream knows PROXY protocol exists.
- `http.Server{Handler: proxyHandler, ReadHeaderTimeout, IdleTimeout, MaxHeaderBytes}`. **The
  handler is the proxy handler itself, never an `http.ServeMux`**: the mux cleans `//` and `..` and
  issues redirects, which would rewrite object keys. Enforced by a test that sends `GET /b//k` and
  `GET /b/a/../k` through the handler and asserts the backend saw them verbatim.
- `http.Server.ConnState` increments and decrements `connections_open{side="client"}`.
- In-flight cap: an `atomic.Int64` incremented at handler entry; past `listen.max_inflight` the
  request is answered `503 SlowDown` (`Retry-After: 1`) and counted as `proxy_error`.

### 4.2 Identity (`s3id.Parse`)

One pure function over `r.Method`, `r.Host`, `r.RequestURI`, `r.Header`, `r.RemoteAddr`, and the
configured `Domains`, producing:

```go
type RequestIdentity struct {
    AccessKey  string      // "" when anonymous
    Bucket     string      // "" for service-level operations
    Key        string      // percent-decoded once; "" at bucket or service level
    ListPrefix string      // the `prefix` query parameter of a list operation, decoded once (ADR-0001)
    SourceIP   netip.Addr
    Op         Op
    Class      Class
    Flags      Flags       // VersionID, PartNumber, Range, XID present; method/shape for Unknown
    Addressing Addressing  // Path or VHost, diagnostics only
    Level      Level       // service, bucket, key
}
```

Rules, each pinned by a test (test plan category 2):

- **Request-target**: `rawPath, rawQuery := split(r.RequestURI, '?')` (shunt `sigv4.RawPath`
  and `RawQuery`, lifted; parse only). The bucket segment is taken raw (valid bucket names never
  need escaping; decoding would let `%2F` smuggle a `/` into a bucket name, found by shunt's
  `FuzzParse`). The key is `url.PathUnescape`d exactly once; `+` stays literal; `%`, `//`, spaces,
  and Unicode are preserved; a decode error leaves the raw bytes. `//key` with an empty bucket
  segment is service-level `Unknown` and is proxied as-is.
- **Bucket**: `Host` with the port stripped (`net.SplitHostPort`, IPv6 literals included); if it
  ends in `.<base domain>` the bucket is the part before the suffix, dots included, and
  `Addressing = VHost`; a bare base domain, a bare IP, the proxy's own name, or no match means
  path-style and the bucket is the first path segment. Missing `Host` is path-style.
- **Access key**: `Authorization: AWS4-HMAC-SHA256 Credential=<AKID>/…` → the bytes between
  `Credential=` and the first `/`; `Authorization: AWS <AKID>:<sig>` → the bytes before `:`;
  query `X-Amz-Credential=<AKID>%2F…` or `<AKID>/…` → the bytes before `%2F` or `/`, so no decode
  is needed; query `AWSAccessKeyId=<AKID>`. Header wins over query when both appear. Temporary
  keys (`ASIA…`) are just keys. Every result is a substring of the header or request line.
- **Source IP**: `netip.ParseAddrPort(r.RemoteAddr)` (which already reflects PROXY protocol when
  enabled). If the peer is inside `listen.trusted_proxies`, walk `X-Forwarded-For` entries from
  the right; the first entry that is not itself a trusted proxy is the client. Any other peer's
  `X-Forwarded-For` is ignored. A tenant cannot pick its own rate-limit bucket.
- **Query scan**: one pass over `rawQuery` that sets bits for the subresource keys the classifier
  knows (`uploads`, `uploadId`, `tagging`, `acl`, `versioning`, `delete`, `attributes`, `restore`,
  `select`, `location`, `list-type`, `versions`, `session`, `annotation`, `annotationName`,
  `renameObject`, `encryption`, `metadataTable`, …), records the byte ranges of the few values
  that matter (`list-type`, `select-type`, `x-id`, `prefix`, `X-Amz-Credential`, `AWSAccessKeyId`,
  `id`), and allocates nothing. `url.ParseQuery` and its map are not used on the hot path.

### 4.3 Classifier

shunt's table (`internal/s3/classify.go` and `op.go` at `9b36d09`) lifted as Go code and
extended with the 18 operations the reference lists that shunt lacked
(`docs/OPERATIONS_TABLE.md` has the check). The row shape is shunt's: `(method, level, required
query keys, optional exact key=value, optional header) → Op`, walked in order, first match wins.
The matcher reads the query bitmask from 4.2 instead of a `url.Values` map; the rows, their
order, and shunt's `TestEveryRow` and `TestPrecedence` are otherwise unchanged. Each `Op` carries a
`Class` (`LIST`, `READ`, `WRITE`, `DELETE`, `BUCKET`, `UNKNOWN`). `OPTIONS` at any level is
`Preflight`. `x-id` is recorded as a flag and used by tests as an oracle; it is a tiebreaker
nowhere in v1 because no row needs one after the `ListDirectoryBuckets` fold.

`docs/OPERATIONS_TABLE.md` is regenerated from the Go table by a test that fails on drift, so the
Go table is the single source of truth. Coverage: the table test enumerates the enum; an
operation without a test row fails the build.

### 4.4 Outbound request

Built the way shunt's `preparePassthrough` builds it, minus everything shunt adds:

```go
out := &http.Request{
    Method: r.Method,
    URL:    &url.URL{Scheme: backend.scheme, Host: endpoint, Opaque: rawPath, RawQuery: rawQuery},
    Host:   r.Host,                 // exactly as sent, in both addressing styles; it is in SignedHeaders
    Header: r.Header,               // the inbound map, reused, not cloned
    Body:   inBody,                 // nil when r.ContentLength == 0 and no chunked body
    ContentLength: r.ContentLength, // the Transport writes Content-Length from this
    Trailer: r.Trailer,
    Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
}
```

- `URL.Opaque` carries the raw path so `%2F`, `+`, `//`, and every other byte reach the backend
  unchanged; `RawQuery` is appended verbatim by `URL.RequestURI()`. Go would otherwise normalize
  `%2F` into `/` and break the signature. One case needs a second mechanism: `URL.RequestURI()`
  turns an `Opaque` that begins with `//` into absolute-form (`scheme://…`). For a raw path that
  begins with `//` the handler instead sets `URL.Path = unescape(rawPath)` and
  `URL.RawPath = rawPath`, and `EscapedPath()` returns `rawPath` byte-for-byte whenever Go's
  `validEncoded` accepts it (every properly percent-encoded path does). Path-style `//key` has an
  empty bucket, classifies `Unknown`, and is forwarded for the backend to answer; virtual-hosted
  `//key` is the legal object key `/key` and is forwarded the same way. **The proxy never rejects
  what the backend might accept.** Its only self-generated `400 InvalidURI` is a request-target it
  cannot forward at all: a leading `//` combined with bytes Go would re-encode (raw non-ASCII or
  the few reserved characters `validEncoded` refuses), which no S3 client produces. Both `//`
  cases are s3diff cases (`docs/DECISIONS.md` D-004, revised at Gate 0).
- Hop-by-hop headers are deleted from the shared map before sending: `Connection`, `Keep-Alive`,
  `Proxy-Authenticate`, `Proxy-Authorization`, `Proxy-Connection`, `TE`, `Trailer`,
  `Transfer-Encoding`, `Upgrade`, plus any header named in `Connection`. `Expect` is kept: it
  must reach the backend for 100-continue chaining. Mutating `r.Header` is safe because nothing
  reads it afterwards.
- **No header is added.** No `Via`, no `X-Forwarded-*`, no request id (shunt adds all three; they
  are removed). `User-Agent` is set to the empty string when the client sent none so the Transport
  does not insert `Go-http-client/1.1`. The Transport is built with `DisableCompression: true` (no
  `Accept-Encoding: gzip`), `Proxy: nil` (environment proxies ignored), `ForceAttemptHTTP2: false`
  and an empty `TLSNextProto` (no h2), `MaxIdleConnsPerHost` from config, `DialContext` with
  `backend.dial_timeout`, `ResponseHeaderTimeout`, `ExpectContinueTimeout: 1s`, and
  `TLSClientConfig` from `backend.tls`.
- The body reader is shunt's `progressReader` (lifted from `copy.go`): it counts bytes, pushes the
  server read deadline forward by `backend.idle_body_timeout` on every read via
  `http.ResponseController.SetReadDeadline`, and kicks the idle watchdog. Nothing reads the body
  before `RoundTrip`, so a denied request never reads it, Go's server never sends `100 Continue`,
  and a throttled `Expect: 100-continue` PUT sends zero body bytes.

### 4.5 Backend pool and RoundTrip

`internal/proxy/backend.go` (shunt `internal/upstream`, lifted: `Cluster`, endpoints,
`IsConnectError`, health checks). Round-robin over `backend.endpoints` with an `atomic.Uint64`
counter; endpoints marked down by the active checker (TCP connect plus `HEAD /` every
`health_check.interval`; any HTTP status, 403 included, means healthy) or passively on a dial
error are skipped. `RoundTrip` retries on the next endpoint only when the failure was a dial or
TLS handshake error and `inBody.count() == 0`, i.e. before any request byte went out. Never after.
`backend_retries_total` counts retries. With every endpoint down the request gets
`503 ServiceUnavailable` with the S3 body and `x-amz-request-id`, counted `proxy_error`.

TTFB is measured as `time.Since(t0)` around `RoundTrip` (headers received); the `httptrace`
struct shunt allocates per request is not used.

### 4.6 Relay

- Response headers are copied into `w.Header()` (slices shared, not cloned; hop-by-hop dropped;
  nothing added). When the backend sent no `Content-Type`, `w.Header()["Content-Type"] = nil`
  disables net/http's sniffing so the client sees none. `w.WriteHeader(resp.StatusCode)` follows
  at once, and **every copied chunk is flushed** (`ResponseController.Flush` inside
  `deadlineWriter.Write`). net/http buffers 4 KiB; without the flush a trickle response, such as
  `CompleteMultipartUpload` on a large object (headers, then whitespace for minutes),
  `SelectObjectContent`, or a slow list stream, would reach the client only when the buffer filled
  or the response ended. For chunks ≥ 4 KiB the flush costs nothing (the write goes through
  anyway); for small chunks it is exactly the syscall transparency requires, and transparency
  outranks latency. `TestTrickleResponseReachesClient`: the backend sends headers, then one byte
  per second; the client sees the headers and the first byte within 1.5 s (`docs/DECISIONS.md`
  D-006, revised at Gate 0).
- Body: `io.CopyBuffer(deadlineWriter{w}, progressReader{resp.Body}, pooled buffer)`.
  `deadlineWriter` hides `ReaderFrom` so the pooled buffer is used, pushes the write deadline
  forward on every write, and counts bytes. The buffer comes from a `sync.Pool` of
  `proxy.copy_buffer_bytes` slices. A watchdog (`time.AfterFunc`, reset on every chunk, one timer
  per request) cancels the backend request when no bytes move for `idle_body_timeout`.
- **A body cut short is never delivered as complete** (shunt ADR-0003): if the copy ends in error
  and fewer bytes than the backend's `Content-Length` were written (or the response was chunked),
  the handler records the outcome and `panic(http.ErrAbortHandler)`, so net/http closes the client
  connection without a terminating chunk. A write error after a complete body returns normally.
- `ETag`, `x-amz-request-id`, `x-amz-id-2`, `x-amz-version-id`, checksum headers, everything:
  passed through unchanged. The proxy adds nothing to responses. The only responses it originates
  are its own errors (4.8).

### 4.7 Completion

One `finish` runs in a `defer` at the top of the handler and records exactly once: the
`requests_total` counter (pre-resolved, 9.3), duration and overhead histograms, byte counters,
and the showback increment with `status_class` ∈ `{2xx,3xx,4xx,5xx,throttled,proxy_error}`. The
recover guard sits above it: any panic other than `http.ErrAbortHandler` is recovered, counted
(`limiter_errors_total` if it came from the limiter, otherwise logged once per minute), and the
request is forwarded or, if the response has already started, abandoned the way 4.6 abandons it.
`http.ErrAbortHandler` is re-panicked so fail-open can never complete a truncated body.

### 4.8 Proxy-originated responses

Only four: `503 SlowDown` (throttle, in-flight cap), `429 TooManyRequests` (throttle in that mode),
`503 ServiceUnavailable` (no backend reachable), `400 InvalidURI` (unforwardable request-target).
Body is the S3 error XML from shunt's `internal/s3/errors.go` table (lifted, plus
`TooManyRequests`), with `Resource` set to the raw path (S3 itself echoes it), a freshly generated
`x-amz-request-id`, `Content-Type: application/xml`, `Content-Length`, and, on the throttle
responses, `Retry-After` (whole seconds, rounded up, at least 1) and `x-amz-retry-after`
(milliseconds, rounded up; the exact GCRA wait, which aws-sdk-go-v2 honors with its 2026 retry
behavior on, D-043). Rule
identifiers never appear. The request id is generated only on this path, so the forwarded path
allocates nothing for it.

### 4.9 Invariants and the tests that pin them

| Invariant | Test |
| --- | --- |
| Handler is not a mux; `//` and `..` survive | `TestRawPathSurvivesHandler` (recording backend) |
| `%2F`, `+`, `%25`, spaces, Unicode, trailing `/` reach the backend verbatim | `FuzzParse` round-trip, s3diff key cases |
| `Host` unchanged in both styles | s3diff vhost and path cases; `TestHostPreserved` |
| No header added to request or response | `TestNoAddedHeaders` diffs recording-backend capture and client capture against the originals |
| Hop-by-hop stripped | `TestHopByHop` |
| `Content-Length`, chunked, `aws-chunked` with trailers pass through | s3diff `aws-chunked` and trailer cases |
| Decision before body; throttled `Expect: 100-continue` sends zero body bytes inside 1 s | `TestExpectContinueThrottled` with the recording backend and a client with a 1 s expect timeout |
| Short body never complete | `TestBackendDiesMidGetContentLength`, `TestBackendDiesMidGetChunked` (asserts no `0\r\n\r\n`) |
| Trickle responses reach the client as they arrive | `TestTrickleResponseReachesClient` (headers then one byte per second; headers and first byte seen within 1.5 s) |
| Keys and access keys not logged above debug | `TestLogRedaction` over a captured `slog` handler |

## 5. Rate limiting (`internal/limiter`)

### 5.1 Compiled rules

Reload compiles a `RuleSet` from the validated config and publishes it with one pointer store
inside the limiter (`Limiter.Apply`), immediately after the config pointer swap (D-036; the
config package cannot import the limiter, and a config value carrying compiled state would need
an interface with one implementation):

```go
type RuleSet struct {
    rules   []*Rule            // config order
    byID    map[string]*Rule   // for the sweeper and the admin API
    byClass [numClasses][]*Rule // candidates per operation class; a rule with ops=any is in every list
}
type Rule struct {
    ID        string             // cloned once at compile; part of every key
    idx       int                // position, for metrics and the deny sampler
    accessKeys map[string]struct{} // nil = any; "" is a legal member (anonymous)
    buckets    map[string]struct{} // nil = any; "" = service level
    prefixes   []string           // nil = any; sorted longest first; first HasPrefix wins
    cidrs      []netip.Prefix     // nil = any; linear Contains
    ops        [2]uint64          // bitset over Op (≤ 128 operations); all ones = any
    groupBy    uint8              // bits: access_key, bucket, prefix, source_ip
    T, tau     int64              // emission interval and tolerance in nanoseconds, after node_count
    blockAll   bool               // rate == 0
    mode       Mode               // slowdown | too_many_requests
    stats      *ruleStats         // live keys, denials since start, deny ring; owned by the id, survives reloads
    denied     prometheus.Counter // pre-resolved limiter_denied_total{rule}
}
```

Bounds (D-037): a non-zero `rate` lies in [1e-6, 1e9], `burst / rate ≤ 1e9` s, and
`cluster.node_count ≤ 1024`, so `T ≤ ~1e15` ns and `tau ≤ 1e18` ns and no GCRA sum can overflow
`int64` in a 30-year process lifetime. Validation enforces them; `compile` re-checks and a reload
never publishes a half-built set.

Matching walks `byClass[identity.Class]` in config order and tests, in the cheapest order: ops
bit, bucket set, access-key set, CIDR list, prefix list. Each test is a map lookup, a bit test, a
`Contains`, or a `HasPrefix`; none allocates. The prefix input is `identity.Key` for object
operations and `identity.ListPrefix` for the four bucket-level list operations (ADR-0001). Budget:
10 µs p99 with 100 rules loaded, measured by `BenchmarkDecide100Rules` with `-benchmem` asserting
0 allocs/op (test plan categories 3 and 10).

### 5.2 Key and store

```go
type key struct {
    rule   string      // rule.id; a reload that keeps the id keeps the state
    ak     string      // "" unless access_key in group_by
    bucket string      // "" unless bucket in group_by
    prefix string      // the matched configured prefix, "" unless prefix in group_by
    ip     netip.Addr  // zero unless source_ip in group_by
}
```

Struct keys of existing substrings: no concatenation, no hashing of our own, no allocation on
lookup, and two tenants can never collide. On first insert the strings are `strings.Clone`d so a
request's header buffer is not pinned. The shard is `maphash.Comparable(seed, k) & 255`
(`hash/maphash`, Go ≥ 1.24, hashes strings by content, allocates nothing). Each shard is
`{mu sync.Mutex; m map[key]int64}`: the value is the theoretical arrival time in monotonic
nanoseconds. The specification writes `map[key]*int64`; storing the `int64` directly saves one
8-byte allocation per new key and costs nothing under the lock that is held anyway. Listed as a
question in the gate report.

A decision for one matching rule: lock the shard, look the key up (a miss means a full bucket,
`TAT = now`), apply 5.3, store, unlock. One lock, one lookup, one compare-and-set, as specified.
Live keys are counted in one `atomic.Int64`; at `limiter.max_keys` a miss does not insert, the
request is allowed for that rule, and `limiter_keys_overflow_total` increments. Fail open.

Measured 2026-11-09 on the dev box (`TOLLBOOTH_BIG=1 go test -run TestDecisionLatencyAtOneMillionKeys`):
**226 bytes per key** at 1,000,000 live keys (heap delta 215 MiB), against the ~190 B estimate in
§9.4; decision latency with 100 rules loaded p50 2.6 µs, p99 4.7 µs, p99.9 17.5 µs.

### 5.3 GCRA

Monotonic clock: `now = int64(time.Since(epoch))` where `epoch` is process start, taken once per
request and shared with the metrics. For a rule with `rate > 0`:

```
T   = 1e9 / rate_eff            nanoseconds; rate_eff = rate / node_count
tau = (burst_eff - 1) * T        burst_eff = max(1, ceil(burst / node_count))
tat = m[k] (miss → now)
if now >= tat - tau:  allow; m[k] = max(tat, now) + T
else:                 deny;  wait = (tat - tau) - now; Retry-After = max(1, ceil(wait / 1e9))
```

Exactly `burst_eff` requests pass from a full bucket and the next is denied; with `burst × T` it
would be `burst + 1`, and `TestExactlyBurst` checks this. `rate == 0` is a separate branch:
always deny, `Retry-After: 1`, no state, no infinite `T`. Fractional rates work because `T` is an
integer number of nanoseconds (`rate: 0.5` → `T = 2e9`).

Evaluation is one pass over the matching rules in config order, consuming from each. The first
denial ends the pass, decides the mode from that rule, and the tokens already taken from earlier
rules stay taken (S3's behavior; no check-then-consume, no rollback). `TestDenialChargesEarlierRule`
asserts the charge. `rules test` uses the same function with `consume=false`, which computes the
decision without storing; in that mode it continues past the first denial so the operator sees every
matching rule's own verdict as well as the decision the real pass would reach (D-038). A panic
anywhere in the pass is recovered inside `Decide`: the request is allowed and
`limiter_errors_total` increments, so every caller fails open.

### 5.4 Sweeper and reload

A goroutine wakes every `limiter.sweep_interval`, takes each shard lock in turn, and deletes
every key whose `TAT ≤ now` (that bucket is full, so re-creating it on the next request is
indistinguishable from having kept it: eviction is exact, `TestEvictionLossless`) and every key
whose `rule` is not in the current `RuleSet`. It decrements the live count. Each shard lock is
held for the walk of that shard only; with 1,000,000 keys that is ~4,000 keys per shard. Measured
2026-11-09: a sweep that deletes all 1,000,000 keys takes 165 ms, so each shard lock is held about
0.65 ms, 256 times per sweep.

Reload swaps the config pointer, then `Limiter.Apply` compiles and stores the new `RuleSet`; the
store is untouched. A changed `rate` or `burst` applies from the next decision because a TAT is a
timestamp. Per-rule counters and the deny ring belong to the rule id (`ruleStats`) and move to the
new compiled rule; the stats and the `limiter_denied_total{rule}` child of a removed id are deleted.
Requests in flight finish with the `RuleSet` they loaded at entry. `TestReloadKeepsState` and the
Phase 2 reload-under-load test (≤ 100 ms, zero failed requests) pin this.

### 5.5 Deny sampler (`/admin/limits/top`)

Per rule, a fixed ring of 1,024 `{key, at int64}` entries written under a per-rule mutex on each
denial; the array is allocated on the rule's first denial. `GET /admin/limits/top?n=20&window=60s`
reads the rings, counts entries within the window by key, and returns the top n with the rule id
and the key's dimensions. The ring holds cloned strings only; a slot that already holds the same
key is reused without cloning, so a storm on few keys allocates nothing after the first lap
(`TestDecideAllocatesZero`). Allowed requests never touch it.

## 6. Showback (`internal/showback`)

### 6.1 Aggregator

```go
type tuple struct {
    ak, bucket, prefix string  // "" when the dimension is not configured
    ip                 netip.Addr
    op                 Op      // uint8
    status             Status  // uint8: 2xx 3xx 4xx 5xx throttled proxy_error
}
type row struct{ requests, bytesIn, bytesOut int64 }
type hourTable struct {
    hour   int64
    shards [256]struct {
        mu     sync.Mutex
        sealed bool
        m      map[tuple]*row
    }
    tuples atomic.Int64
}
```

`Record(id, status, bytesIn, bytesOut)`:

1. `h = time.Now().Unix() / 3600`, read **inside** `Record`, not from a timestamp taken earlier
   in the handler: a request that completed at 10:59:59.999 whose `Record` runs at 11:00:00.001
   must land in hour 11, the hour the sealer is working in, or it would be a spurious
   `showback_clock_skew_total`. Load `cur := agg.cur.Load()`.
2. If `h > cur.hour`: build a new `hourTable{hour: h}` and `CompareAndSwap(cur, new)`; on failure
   another request did it first, reload; on success hand the replaced table to the writer's
   closed list (D-049). If `h < cur.hour` (wall clock stepped back): count into `cur` and
   increment `showback_clock_skew_total`.
3. Compute the shard from `maphash.Comparable(seed, tuple)`, lock it, and re-check
   `shard.sealed`: if the sealer got here first, unlock and go to 1 with the new table. Otherwise
   look the tuple up; on a miss, if `tuples ≥ max_tuples_per_hour` substitute the `__overflow__`
   tuple for the configured dimensions and increment `showback_collapsed_total`, else allocate a
   `row` with cloned strings and insert. Increment the three counters. Unlock.

Nanoseconds under the lock, no atomics on the counters, no lock-free structures, no I/O, and the
request goroutine never waits on storage. Budget: ≤ 2 µs p99 and ≤ 2% of peak request rate on
versus off (`BenchmarkRecord`, Phase 3 throughput A/B).

### 6.2 Snapshot and seal

One writer goroutine, a ticker at `showback.flush_interval`:

- **Snapshot**: for the open table, take each shard lock in turn, copy its rows into a reusable
  slice, release; encode header plus rows as NDJSON to `current/<hour>-<node>.ndjson.tmp`,
  `fsync`, rename over `current/<hour>-<node>.ndjson`. The header's `partial` is `true`. A write
  or rename error increments `showback_flush_errors_total`, is logged once per minute, and the
  in-memory table keeps counting up to the tuple cap.
- **Seal**: when the wall clock's hour exceeds the open table's hour, the writer swaps in a new
  table if no request has yet (so an hour with no requests still gets a file), then takes every
  table on the closed list, oldest first, and for each shard of it: lock, set `sealed`, unlock.
  The writer wakes at every flush interval and half a second after each hour boundary (D-049). After that pass no `Record` can land in the old
  table (6.1 step 3 re-checks under the lock), so the old hour has no writers and no grace period
  is needed. Final encode with `partial: false`, rename into `usage/YYYY/MM/DD/HH-<node>.ndjson`,
  remove the `current/` file. Nothing reopens or overwrites a sealed file (a second seal of the
  same hour lands beside it as `.dup`, logged); a wall-clock step backward lands in the current
  hour (6.1 step 2). Measured 2026-11-09 on the dev box: `Record` 198 ns and 0 allocations for an
  existing tuple; a snapshot of 10,000 tuples (copy, sort, encode, fsync, rename) 24 ms.
- **Retention**: once a day delete `usage/` files older than `retention_days`.

### 6.3 Restart

At startup, `current/*-<node>.ndjson` is read. If its hour is the current hour, the rows are
loaded into the open table and counting continues with no double count (the snapshot *is* the
state). If its hour is earlier, the file is sealed as-is into `usage/` with `recovered: true`
(the node was down when the hour ended; up to `flush_interval` of it is lost) and the open table
starts empty. A snapshot from another node id in the same directory is left alone.

### 6.4 Query and verify

The NDJSON reader (`internal/showback/read.go`, with a fuzz target) parses one file into its
header and rows. `Query{From, To, GroupBy, Where}` streams every file whose hour is in range,
filters rows, groups by the requested dimensions, and sums the three counters; the open hour is
read from `current/` and flagged `partial`. It does filter, group, and sum and nothing else.
`Verify(dir)` recomputes each header, lists hours with no file per node as downtime, and reports
`partial`, `recovered`, and header-mismatch files. The CLI (`tollbooth usage …`) and
`GET /admin/usage` call the same functions.

## 7. Configuration, admin, observability

### 7.1 Configuration (`internal/config`)

`Load(path) (*Config, error)` parses with `go.yaml.in/yaml/v4` and `KnownFields(true)`, applies
defaults, validates the whole file (every rule in `docs/CONFIG.md`), compiles the `RuleSet`, the
`Domains`, the trusted-proxy table, and the showback prefix tables, and returns one immutable
value. `serve` stores it in an `atomic.Pointer[Config]`. `SIGHUP` and `POST /admin/reload` call
`Load` again and swap the pointer on success; on failure the running config stays, the error is
kept for `GET /admin/config`, and `config_reload_total{result="error"}` increments. Keys that
cannot change at runtime (`listen.address`, `admin.address`, `node_id`, `showback.dir`) are a
reload error. Nothing in tollbooth writes the file. `features.go` holds the (empty) feature set and
the two kill switches.

### 7.2 Admin server (`internal/admin`)

A second `http.Server` on `admin.address` with its own `ServeMux` (a mux is fine here: nothing
on this port is an object key) and its own connection handling, never sharing the data
listener's pool. Routes, each a read of state the proxy already holds:

| Route | Purpose |
| --- | --- |
| `GET /healthz` | Process is up. |
| `GET /readyz` | Config loaded and at least one backend healthy. |
| `GET /metrics` | Prometheus exposition. |
| `POST /admin/reload` | Same as `SIGHUP`; returns the result as JSON, 200 when published, 422 when the file was rejected (D-040). |
| `GET /admin/config` | Effective config with `admin.token` redacted, in the file's key names, with the generation, load time, and last reload result (D-040). |
| `GET /admin/status` | Uptime, node id, in-flight, req/s, throttled/s, backend health, config generation, last reload result. |
| `GET /admin/limits` | Rules in force, live key count per rule, denied counts since start. |
| `GET /admin/limits/top?n=` | Top denied keys in the last 60 s (5.5). |
| `POST /admin/limits/test` | Dry run for `rules test` (5.3, `consume=false`); served from Phase 2 (D-039). |
| `GET /admin/backends` | Endpoints, health, in-flight, error counters, TTFB. |
| `GET /admin/usage?from=&to=&group_by=&where=&format=` | Showback query over this node's store (6.4). |
| `GET /debug/pprof/*` | Only with `admin.pprof: true`. |

`/admin/*` requires `Authorization: Bearer <admin.token>` when a token is configured; the three
health and metrics routes never do. The route table is one Go slice, and the parity test (test plan
category 12) asserts every entry appears in `docs/OPERATOR_SURFACE.md` with a CLI verb and a TUI
screen. The admin client (`internal/admin/client.go`) is the one HTTP client the CLI and TUI use.

### 7.3 Metrics and logging

Every metric is in `docs/telemetry-catalog.md` before it is implemented; `TestEveryMetricIsInTheCatalog`
and a cardinality test over the label sets guard it. Labels never carry an access key, IP, bucket,
or object key. Logging is `log/slog` JSON to stdout: one startup line, reload results at `info`,
throttle decisions at `debug` sampled one in `logging.throttle_sample`, access log off by default.

## 8. Operator surface

CLI verbs, TUI screens, and the parity matrix are specified in docs/SPEC.md (Operator surface) and
tracked in `docs/OPERATOR_SURFACE.md`. Design points that matter here: one admin client; every
verb supports `--format table|json|csv` where `json` is the admin response unwrapped; neither the
CLI nor the TUI writes the config file; the TUI is read-only, polls, and shows the last state under
a staleness banner when the admin API is unreachable; Bubble Tea imports are fenced to
`cmd/tollbooth/tui` (ADR-0003).

## 9. Hot-path walkthrough with allocation budget

Counted per *forwarded* request, excluding what `net/http` allocates for its own server-side
request and client-side transport (measured separately as the baseline). All sizes are fixed;
nothing scales with body size.

| Stage | Work | Allocations (ours) | Time budget |
| --- | --- | --- | --- |
| 1. Accept and header parse | `net/http` | 0 | — (Go's; measured as baseline) |
| 2. Handler entry | `time.Now()`, in-flight increment, `defer finish`, `defer recover` | 0 | < 0.2 µs |
| 3. Identity | substrings of `Host`, `RequestURI`, `Authorization`; `ParseAddrPort`; query bit scan | 0 (+1 when the key contains `%`, +1 when a list `prefix` contains `%`) | < 1 µs |
| 4. Classify | table walk over the bitmask | 0 | < 0.3 µs |
| 5. Limiter | match over `byClass`; per matching rule: hash, lock, lookup, store | 0 steady state; first sight of a key: up to 3 `strings.Clone` + map insert | ≤ 10 µs p99 at 100 rules |
| 6. Outbound request | `&http.Request{}`, `&url.URL{}`, `progressReader` (only with a body) | 3 | < 1 µs |
| 7. RoundTrip | Transport (Go's); retry loop; `time.Now()` ×2 | 0 ours | backend |
| 8. Relay | header copy (slice sharing), `WriteHeader`, `deadlineWriter`, `progressReader`, `ResponseController`, pooled buffer get, watchdog timer | 4 (3 small structs + 1 `time.Timer`; the buffer is pooled) | < 1 µs plus copy at memory speed |
| 9. Finish | pre-resolved counters (9.3), two histogram observes, showback `Record` | 0 steady state; new tuple: 1 `row` + up to 3 clones | ≤ 2 µs p99 showback + ≤ 1 µs metrics |

Total ours: **≤ 7 small allocations per forwarded request**, 0 per body byte, 0 in the limiter
decision and the showback increment at steady state. The seven, by name:

| # | Allocation | Where | Why it cannot be avoided |
| --- | --- | --- | --- |
| 1 | `*http.Request` for the outbound request (`Request.WithContext` copies the stack literal) | §4.4 | `http.Transport.RoundTrip` takes a pointer it retains; the context field is unexported, so the copy is the only way to attach one |
| 2 | `*url.URL` for the outbound request | §4.4 | `Request.URL` is a pointer; the inbound URL cannot be reused because `Opaque`, `Scheme`, and `Host` differ |
| 3 | `*http.ResponseController` | §4.4, §4.6 | `http.NewResponseController` returns a pointer and the readers hold it |
| 4 | one `reqState` holding the watchdog, both body readers, and the response writer | §4.4, §4.6 | `Request.Body` and `io.CopyBuffer`'s arguments are interface values, so the readers must live on the heap; one struct instead of four |
| 5 | `*time.Timer` for the idle watchdog | §4.6 | `time.AfterFunc` allocates its timer; one per request |
| 6 | `*cancelCtx` from `context.WithCancel` | §4.4 | the watchdog cancels the backend request through its context |
| 7 | the `CancelFunc` closure from `context.WithCancel` | §4.4 | returned by the standard library with the context |

Measured on 2026-11-09 by `TestHandlerAllocations` (no-op writer, fake `RoundTripper`): **7.00** per forwarded 4 KiB `GetObject`, 0 per body byte.

Everything else is pooled (copy buffer), pre-resolved (metric handles, §9.3), or a struct value
that does not escape (`RequestIdentity`, the limiter decision, the outcome record). Enforcement:
`TestHandlerAllocations` in `internal/proxy` runs `testing.AllocsPerRun` over a warmed-up 4 KiB
`GetObject` with `httptest.NewRecorder` as the `ResponseWriter` and a fake `RoundTripper` that
returns a canned response, so no sockets or goroutines are involved, the `net/http` baseline is
near zero, and the seven are counted directly; it **fails above 7**. Differencing two noisy
end-to-end measurements was rejected as flaky (Gate 0). `BenchmarkHandlerGetObject` `-benchmem`
against the null backend records the absolute end-to-end number for the committed baseline. The limiter and showback
microbenchmarks assert exactly 0 allocs/op. If Gate 1 finds one of the seven avoidable (for
example by embedding the readers in one per-request struct), the table shrinks and the test's
bound follows it; it never grows without an ADR.

Denied path (throttle): stages 2–5, then the XML body is written from a pooled byte slice with
`Retry-After` and a request id (128 random bits from `math/rand/v2`, formatted on the stack).
Measured 2026-11-09 by `TestThrottledAllocations` (pinned at the measured value, D-041):
**6 allocations** in both build modes, the request-id, `Content-Length`, and `x-amz-retry-after`
strings and the three single-value header slices that vary (`Content-Length`, `X-Amz-Request-Id`,
`X-Amz-Retry-After`); `Content-Type` and small `Retry-After` values are shared slices; no body read,
nothing per body byte. A throttle storm
allocates less per request than a forwarded request and does none of its I/O.

### 9.3 Pre-resolved metric handles

`requests_total{operation, op_class, status_class, result}` would cost a label hash and a slice
per observation through `WithLabelValues`. At startup every child is resolved once with
`GetMetricWithLabelValues` into `[numOps][numStatus][numResult]prometheus.Counter` (113 × 6 × 4
≈ 2,700 counters, `op_class` is a function of `operation`), and the hot path indexes the array.
The same for `bytes_total{direction}` and the three histograms by `op_class`. Zero allocations,
and the cardinality is fixed by construction, which is what the cardinality test asserts.

### 9.4 Memory model

| Item | Bytes each (estimate) | At cap | Note |
| --- | --- | --- | --- |
| Limiter key + TAT in a Go map | ~96 B key/value + ~30 B map overhead + ~60 B cloned strings ≈ 190 B | 1,000,000 keys ≈ 190 MiB | measured at Gate 2; `docs/OPERATIONS.md` sizing |
| Showback tuple + row | ~80 B key + 8 B pointer + 48 B row (with allocator header) + ~30 B map + ~60 B strings ≈ 230 B | 250,000 tuples ≈ 55 MiB; the specification expects 350–450 B measured | measured at Gate 3 |
| Snapshot scratch | rows slice, reused | ~30 MiB transient | one writer goroutine |
| Copy buffers | `copy_buffer_bytes` × in-flight bodies | defaults 4,096 × 128 KiB = **512 MiB** worst case | `sync.Pool`; the defaults bound it at half the 1 GiB target; `check-config` warns when `max_inflight × copy_buffer_bytes` exceeds half of `GOMEMLIMIT` (D-024) |
| Deny rings | 1,024 × ~100 B per rule | 100 rules ≈ 10 MiB | only rules that deny fill theirs |

Target: ≤ 1 GiB RSS with `GOMEMLIMIT=1GiB` at 1,000,000 keys and 250,000 tuples. The copy-buffer
row is the one that can break it under a flood of concurrent large bodies, so its budget is
decided now rather than discovered in the Phase 5 soak: `listen.max_inflight` defaults to 4,096
and `proxy.copy_buffer_bytes` to 131,072, bounding it at 512 MiB; an operator who raises either
is warned by `check-config` when the product exceeds half of `GOMEMLIMIT`, and
`docs/OPERATIONS.md` gives the sizing rule. The soak confirms the sum, it does not set it.

## 10. Concurrency

- One goroutine per request (net/http). No worker pools, no channels on the hot path.
- Limiter: 256 shard mutexes; the sweeper takes them one at a time. No other lock.
- Showback: 256 shard mutexes per hour table; `atomic.Pointer` to the open table; the writer
  goroutine is the only one that encodes or touches disk; the sealer's per-shard `sealed` flag is
  the hand-off (6.2).
- Config: one `atomic.Pointer[Config]`; a request loads it once at entry.
- Backend health: one goroutine per endpoint on a ticker; health is an `atomic.Bool` per endpoint.
- Metrics: the client library's atomics.
- Shutdown: `SIGTERM` → `http.Server.Shutdown(ctx with drain_timeout)` on the data listener,
  showback final snapshot, admin server shutdown, exit 0.

## 11. Resilience

The failure table in docs/SPEC.md (Data path and resilience) is the contract; each row names the
mechanism here and the test (category 9) that asserts it.

| Failure | Mechanism | Test |
| --- | --- | --- |
| Limiter panic or internal error | recover guard (4.7), fail open, `limiter_errors_total` | `TestLimiterPanicFailsOpen` |
| Key cap | 5.2 overflow branch | `TestKeyCapFailsOpen`, 10,000 new keys/s for 60 s under `GOMEMLIMIT` |
| Showback store unavailable | 6.2 error path; counting continues; log once per minute | `TestFlushErrorKeepsCounting` (read-only dir) |
| Invalid reload | 7.1 | `TestInvalidReloadKeepsConfig` under load |
| All backends unreachable | 4.5 | `TestAllBackendsDown` |
| One backend down | 4.5 passive marking and retry-before-bytes | `TestOneBackendDown` |
| Backend TTFB 30 s | `response_header_timeout` 300 s default; request waits | `TestSlowBackendHeaders` |
| Backend resets mid-download | 4.6 abort | `TestBackendDiesMidGet*` |
| Client disconnects mid-upload | context cancel propagates to the backend request; `bytes_in` as read | `TestClientAbortsUpload` |
| `SIGTERM` under load | section 10 shutdown | `TestDrain` |
| Wall-clock step backward | monotonic limiter; 6.1 step 2 | `TestClockStepBack` (injected clock) |
| Flood of unique keys or IPs | 5.2 cap and 6.1 cap | `TestUniqueKeyFlood`, `TestTupleOverflow` |
| Slowloris, 1 MiB headers | `ReadHeaderTimeout`, `MaxHeaderBytes`, in-flight cap | `TestSlowloris`, `TestHugeHeaders` |

## 12. Testing strategy

Every category in docs/SPEC.md (Test plan) maps to a package and a phase:

| # | Category | Where | Phase |
| --- | --- | --- | --- |
| 1 | Classifier | `internal/s3id` golden table test over every `Op` × style × flags × auth form | 1 |
| 2 | Identity | `internal/s3id` | 1 |
| 3 | Limiter | `internal/limiter` with an injected clock | 2 |
| 4 | Fuzzing | `FuzzParse`, `FuzzClassify` (`internal/s3id`, shunt's corpus as seed), `FuzzConfig`, `FuzzNDJSON` | 1–3 |
| 5 | Byte-equivalence | `test/s3diff` + `bench/recordbackend` | 1 |
| 6 | Client compatibility | `test/integration` (Garage and versitygw run as pinned static binaries downloaded by `test/integration/backends.sh`, no container runtime; MinIO and `mc` dropped, D-022), `docs/COMPAT.md` | 1 |
| 7 | Throttling behavior | `test/e2e` (aws-sdk-go-v2 in-process, every PR: body parsing, 1 req/s completion by retry mode, the 200-object sync at half concurrency in standard and adaptive modes, the Expect reconnect cost) and `test/integration/throttle.sh` (aws cli, boto3, s5cmd, rclone, s3cmd against Garage in the CI `integration` job, `docs/THROTTLE.md`) | 2 |
| 8 | Showback correctness | `internal/showback` (injected clock: completion hour, rollover and seal, restart with no double count, kill between snapshots, recovered hour, overflow fold, clock step back, concurrent records across the seal, verify detects a removed hour, `FuzzRead`) + `TestShowbackReconciliation` and the on/off cost A/B in `test/e2e` | 3 |
| 9 | Chaos and resilience | `test/e2e` + null backend fault injection | 1–5 |
| 10 | Performance regression | `make bench-compare` against `test/bench/baseline.txt` (committed at Gate 2 from the fixed CI runner) in the nightly job; `make bench-e2e` (`bench/run.sh`, `bench/loadgen`) for the end-to-end rows | 2–5 |
| 11 | Static analysis | CI (`.golangci.yml`, `govulncheck`, `gosec`, Semgrep, CodeQL, `go-licenses`) | 0 skeleton, 1 on |
| 12 | Operator surface | `internal/admin` parity test, CLI goldens, `teatest` goldens | 4 |

Coverage floor 85% of statements; every `Op` exercised; `-race` everywhere.

## 13. Deferred (explicitly)

| Item | Phase | Why not now |
| --- | --- | --- |
| Hold-and-forward (shaping) | 6, by ADR | needs a per-rule queue, timers, and client-timeout analysis |
| Cluster-coordinated limits | 6, by ADR | a remote round-trip per decision breaks the latency budget |
| Bandwidth limits | 6, by ADR | `limit.bandwidth` key reserved; rejected as unknown until then |
| Parquet export | 6, by ADR | NDJSON is enough for DuckDB and billing systems; a store seam is introduced then |
| OpenTelemetry tracing | 6 | metrics and showback cover v1 needs |
| Per-source connection caps | 6 | `ReadHeaderTimeout` and the in-flight cap bound slowloris in v1 |
| Rule editing from CLI/TUI | 6 | nothing in tollbooth writes the config file |
| Backend drain | 6 | passive marking and retry-before-bytes suffice for v1 |
| Web UI | 6, only if the TUI proves insufficient | |
| Go versus Rust | decided by ADR at Gate 5 only if p99 targets are missed | |
| Nothing else from the specification is deferred. | | |

## 14. Requirement map

| docs/SPEC.md section | Here |
| --- | --- |
| Mission and scope; priorities; out of scope | §1 |
| Decisions already made (every row) | Language, deps: §2, `docs/deps.md`; proxy core: §4.4–4.7; signatures: §4.4; throttle response: §4.8; rule evaluation: §5.3; failure posture: §4.7, §11; GCRA: §5.3; multi-node: §5.3 `node_count`; showback store and counting: §6; time: §5.3, §6.1; addressing: §4.2; source IP: §4.1–4.2; TLS: §4.1, §4.4; configuration: §7.1; deployment: Phase 5 (`deploy/`); logging and metrics: §7.3; license: `LICENSE`, `NOTICE` |
| Lift from shunt | §4 (each lifted piece named), `docs/reuse.md`, `docs/deps.md` |
| Open decisions | ADR-0001, ADR-0002 |
| Request identity (fields, invariants, deployment constraint) | §4.2, §4.9; constraint in `docs/CONFIG.md` (`backend.endpoints` warning) and `docs/OPERATIONS.md` (Phase 5) |
| S3 operation classifier | §4.3, `docs/OPERATIONS_TABLE.md` |
| Rate limiting (schema, semantics, GCRA, modes, reload) | §5, `docs/CONFIG.md` |
| Showback (columns, hot path, snapshot and seal, storage, cardinality, query, reconciliation) | §6, `docs/SHOWBACK_SCHEMA.md` |
| Data path and resilience (timeouts, pool, failure table, security posture) | §4.1, §4.5, §11; security posture: §4.1 (TLS 1.2), §7.2 (token, localhost), unprivileged user in `deploy/` (Phase 5) |
| Performance targets and harness | §9, ADR-0002, `bench/README.md` |
| Observability and admin API | §7.2–7.3, `docs/telemetry-catalog.md` |
| Operator surface | §8, ADR-0003, `docs/OPERATOR_SURFACE.md` |
| Phased delivery plan | `docs/STATUS.md` |
| Test plan | §12 |
| Engineering quality bar | §2, `CLAUDE.md`, `Makefile`, `.golangci.yml`, `.github/workflows/` |
| Licensing and attribution | `LICENSE`, `NOTICE`, `THIRD_PARTY_NOTICES.md`, `docs/deps.md`, `scripts/third-party-notices.sh` |
| Gate report format | `docs/reports/gate-0.md` |
