# tollbooth

A transparent reverse proxy for S3 that adds exactly two things: inline rate limiting keyed by
access key, bucket, prefix, and source address, and hourly usage accounting (API calls and bytes)
for SLA reporting and chargeback. Every request and response passes through byte for byte; the
proxy holds no credentials, re-signs nothing, and fails open.

Clients keep their keys, their SDKs, and their code. They change one thing: the endpoint.

## Get started: limit one access key to 50 GetObject calls a second

One binary and one file. You need an S3 endpoint and the access key you want to slow down.

### 1. Get the binary

Linux amd64 and arm64, statically linked. Download the tarball for your machine and `SHA256SUMS`
from the [v0.1.0 release](https://github.com/blakegolliher/tollbooth/releases/tag/v0.1.0), then:

```
sha256sum -c SHA256SUMS --ignore-missing    # tollbooth-v0.1.0-linux-amd64.tar.gz: OK
tar -xzf tollbooth-v0.1.0-linux-amd64.tar.gz
cd tollbooth-v0.1.0-linux-amd64
./tollbooth version                         # tollbooth v0.1.0 (commit 5bf9ec6, ...)
```

Or build it: `make build` puts it in `bin/tollbooth` (Go 1.27.3; `GOTOOLCHAIN=auto` fetches it).

### 2. Write the limit

Save this as `tollbooth.yaml` (it is [`deploy/tollbooth.quickstart.yaml`](deploy/tollbooth.quickstart.yaml)).
Three values are yours to change: the S3 endpoint, the access key, and where usage files go.

```yaml
node_id: tollbooth-1                  # names this node in the usage files

listen:
  address: ":8800"                    # S3 clients connect here instead of to the cluster

backend:
  scheme: http                        # http or https
  endpoints: ["s3.example.internal:80"]   # <- your S3 endpoint, host:port

admin:
  address: "127.0.0.1:9900"           # the CLI, the console, and /metrics

showback:
  dir: ./tollbooth-usage              # <- hourly usage files go here

limits:
  - name: one-key-getobject
    operation: GetObject              # an S3 operation name
    access_key: AKIAEXAMPLEKEY000001  # <- the access key to limit
    limit_per_second: 50
    per: access_key                   # each access key listed gets a budget of its own
```

A limit answers four questions in S3's own words: which requests (`operation`, `access_key`), how
many (`limit_per_second`), counted for whom (`per`), and what the client gets over the limit (503
`SlowDown` unless you say `respond: 429`). Everything the rule does not name passes untouched: other
keys, and this key's PutObject, ListObjectsV2, and every other operation.

### 3. Check it

tollbooth reads every limit back as a sentence, so you check what it understood, not what you typed:

```
$ ./tollbooth check-config tollbooth.yaml
tollbooth.yaml: warning: base_domains is empty: virtual-hosted-style requests will be treated as path-style; set it if clients use bucket.<domain> addressing
tollbooth.yaml: ok (1 rules, 1 backend endpoints, 1 warnings)
  one-key-getobject: At most 50 GetObject calls per second for each access key. Only for requests by access key AKIAEXAMPLEKEY000001. Over the limit, the client gets 503 SlowDown.
```

The warning matters only if clients address buckets as `bucket.your-domain`; path-style clients
can ignore it. A typo is answered, not guessed at:
`unknown S3 operation "GetObjects"; did you mean GetObject?`

### 4. Run it, and point clients at it

```
./tollbooth serve --config tollbooth.yaml
```

Clients now use port 8800 on this host as their S3 endpoint. Nothing else about them changes:

```
aws --endpoint-url http://tollbooth-host:8800 s3 cp s3://photos/cat.jpg .
```

### 5. See it work

The outputs in this section and the next two are from one run of this exact config, v0.1.0, in
front of a test backend. Sent at once: 300 GetObject requests as the limited key (204 went
through, 96 were refused), 200 GetObject as another key and 100 PutObject as the limited key (all
300 went through).

```
$ ./tollbooth rules list
NAME               LIMIT          PER         OPERATION  ONLY_FOR                         RESPOND  KEYS  MATCHED  DENIED
one-key-getobject  50 per second  access_key  GetObject  access_key=AKIAEXAMPLEKEY000001  503      1     300      96

one-key-getobject  At most 50 GetObject calls per second for each access key. Only for requests by access key AKIAEXAMPLEKEY000001. Over the limit, the client gets 503 SlowDown.

1 rules, 1 live keys
```

`MATCHED` says the rule is seeing the traffic it was written for; `DENIED` is what it refused. Who
is being held right now:

```
$ ./tollbooth rules top
RULE               ACCESS_KEY            BUCKET  PREFIX  SOURCE_IP  DENIED  LAST
one-key-getobject  AKIAEXAMPLEKEY000001                             96      0s ago
```

A refused request gets what S3 itself sends when it throttles, so SDKs back off and retry on their
own (request id and date trimmed):

```
HTTP/1.1 503 Service Unavailable
Content-Length: 243
Content-Type: application/xml
Retry-After: 1
X-Amz-Retry-After: 5

<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SlowDown</Code><Message>Please reduce your request rate.</Message><Resource>/photos/cat.jpg</Resource><RequestId>…</RequestId><HostId>tollbooth</HostId></Error>
```

The same thing live, in the console (`./tollbooth tui --config tollbooth.yaml`, then `2`), here
under a steady mix of requests from two keys:

```
 1 Overview  2 Limits  3 Usage  4 Backends  5 Config  6 Graphs  tollbooth-1 · v0.1.0
  LIMIT             MEANS                                                      MATCHED/S THROTTLED/S
▸ one-key-getobject GetObject: 50 per second for each access key; key AKIAE…          45         3.0

  one-key-getobject: At most 50 GetObject calls per second for each access key. Only for requests
  by access key AKIAEXAMPLEKEY000001. Over the limit, the client gets 503 SlowDown.
  held in the last minute: key=AKIAEXAMPLEKEY000001 (222)

  BUSIEST OPERATIONS RIGHT NOW            REQ/S  THROTTLED/S  LIMITED BY
  GetObject                                  71          3.0  one-key-getobject (part)
  PutObject                                 7.5            0  no limit
```

To ask what would happen to a request without sending it:
`./tollbooth rules test --op GetObject --access-key AKIAEXAMPLEKEY000001 --bucket photos --key cat.jpg`.

### 6. Change it

Edit the number and reload. Nothing restarts, and requests in flight are not disturbed:

```
$ sed -i 's/limit_per_second: 50/limit_per_second: 200/' tollbooth.yaml
$ ./tollbooth config diff
KEY                                         RUNNING  FILE
limits[one-key-getobject].limit_per_second  50       200

1 keys differ; run `tollbooth config reload` to apply tollbooth.yaml
$ ./tollbooth config reload
FIELD        VALUE
ok           true
generation   2
rules        1
duration_ms  2.28
at           2026-11-11T00:12:02Z
```

Or do it without opening the file: in the console, `2`, then `enter` on the limit changes its
number and `n` adds a new one. The form lists the operations and access keys it sees in live
traffic, reads the rule back as a sentence while you type, and writes the same lines into the file.

### 7. Who used what

The other half of tollbooth. Every request is counted, by the hour, into plain files (the first
rows appear within a minute of the first request):

```
$ ./tollbooth usage query --group-by access_key,operation
ACCESS_KEY            OPERATION  REQUESTS  BYTES_IN  BYTES_OUT  PARTIAL
AKIAEXAMPLEKEY000001  GetObject  300       0         835584     true
AKIAEXAMPLEKEY000001  PutObject  100       100       0          true
AKIAOTHERKEY00000002  GetObject  200       0         819200     true

1 files, 3 groups, 600 requests, 100 bytes in, 1654784 bytes out; includes the open hour (partial)
```

Refused requests are counted too: the 300 GetObject from the limited key are all there, and its
bytes out are those of the 204 that went through. The files are NDJSON, one row per hour per
access key, bucket, prefix, operation, and status class (`docs/SHOWBACK_SCHEMA.md`).

### Good to know before you rely on it

- **A limit below a client's own concurrency makes some clients fail rather than slow down.** aws
  cli and boto3 in their default retry mode give up after a few attempts; with
  `AWS_RETRY_MODE=adaptive` they pace themselves, and in our tests they finished.
  `docs/OPERATIONS.md` §9 has the measurements and what to set.
- 50 a second also means up to 50 at once: the allowance refills evenly. For strictly one at a
  time, add `burst: 1`.
- tollbooth fails open. If its limiter has a fault, requests are forwarded, not refused.

## More limits

```yaml
limits:
  - name: list-storm                  # each client address may list 20 times a second
    operation: ListObjectsV2
    limit_per_second: 20
    per: source_ip
    respond: 429

  - name: uploads-per-bucket          # each bucket gets its own budget for uploads
    operation: [PutObject, UploadPart, CompleteMultipartUpload]
    limit_per_minute: 6000
    per: bucket

  - name: revoked-key                 # the stop button
    access_key: AKIAREVOKED000000001
    blocked: true
```

`docs/CONFIG.md` documents every key; `deploy/tollbooth.example.yaml` lists them all with their
defaults.

## Status

**v0.1.0.** Every feature of the specification is built and gated. Verified: 293 cases comparing
a request sent directly with the same request sent through the proxy, 0 differences, against each
of three backends, and 184 such cases with 0 differences against a VAST 5.5.3 cluster; six client
tools with 0 failures; a 24-hour soak with fault injection and 0 unexplained errors. **Not yet
verified:** four end-to-end performance targets stated for a 16 vCPU reference node.
[`CHANGELOG.md`](CHANGELOG.md) lists what is measured and what is not.

## Build from source, and the demo

```
make build                      # bin/tollbooth
make demo && source demo/env.sh # a local stack with four rules and a stand-in backend; demo/DEMO.md is the walkthrough
bin/tollbooth tui --config $TOLLBOOTH_DEMO_CONFIG
make release                    # dist/: static linux amd64 and arm64 tarballs, unit file, SHA256SUMS
```

## Documentation

- `docs/OPERATIONS.md` — the runbook: install with systemd or a container, configure, reload,
  upgrade, sizing, what each metric means
- `docs/CONFIG.md` — the configuration schema and defaults
- `docs/OPERATOR_SURFACE.md` — every CLI verb, console screen, and admin route
- `demo/` — the demo stack and walkthrough, including a run against a real cluster
- `docs/DESIGN.md` — architecture, data structures, the hot path and its allocation budget
- `docs/SPEC.md` — the development specification; `docs/adr/` — decision records;
  `docs/reports/` — gate reports; `docs/DECISIONS.md` — every ambiguity resolved without an ADR
- `bench/` — load generator, stand-in backend, soak, cross-check

Apache-2.0. Code adapted from [shunt](https://github.com/blakegolliher/shunt) keeps its headers
and is listed in `THIRD_PARTY_NOTICES.md`.
