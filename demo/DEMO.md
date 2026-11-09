# tollbooth demo: the walkthrough

Twelve minutes, one machine, two terminals. Every command below was run as written against the demo
stack (`demo/run.sh start`) and the outputs are what it printed, trimmed. The audience sees one
thing per step: the proxy is invisible until a rule fires, a rule is one line of intent, a reload
is instant, the hour's usage is a file they can open.

Setup once, in every shell you will type in:

```
make demo                      # or demo/run.sh start: null backend :29000, tollbooth :28000, admin :29900
source demo/env.sh             # TOLLBOOTH_ADMIN, AWS_* for tenant A, s3/s3api/tb helpers, one attempt per command
```

Second terminal, left open the whole time:

```
tb tui --config $TOLLBOOTH_DEMO_CONFIG   # the screens; 1-6 switch, ? help, q quits
```

`demo/run.sh load 20` adds 20 req/s of background GET/PUT/HEAD across three keys and buckets so the
TUI's sparklines move; `demo/run.sh stop` ends everything and keeps `demo/state/` (the showback
files stay queryable); `demo/run.sh clean` removes it.

## 1. It is running, and it says so itself

```
$ tb doctor --config $TOLLBOOTH_DEMO_CONFIG
CHECK                    STATUS  DETAIL
config                   ok      …/demo/state/tollbooth.yaml loads: 4 rules, 1 backend endpoints, 1 warnings
ulimit nofile            ok      soft 524288, hard 524288; about 9280 needed at max_inflight 4096
GOMAXPROCS               ok      2 (NumCPU 2)
GOMEMLIMIT               warn    unset: the specification's memory target assumes GOMEMLIMIT=1GiB; …
tls                      ok      plaintext listener (no listen.tls)
backend 127.0.0.1:29000  ok      tcp 1ms, HEAD / 200 in 1ms
clock                    ok      kernel reports the clock synchronized (max error 45741 µs)
showback.dir             ok      …/demo/state/showback writable; 57.8 GiB free
admin                    ok      127.0.0.1:29900 accepts connections (tollbooth is running)
10 checks, 0 fail

$ tb status
node_id            demo-a01
state              ready
requests           0 total; 0.0/s (1s) 0.0/s (10s) 0.0/s (60s)
rules              4 in force; 0 live keys; 0 denied since start
showback           hour 2026-11-09T19:00:00Z; 0 tuples
backends           1 of 1 up

$ tb rules list
ID                     MATCH                       GROUP_BY           RATE       BURST  MODE               KEYS  DENIED
tenant-a-reads         key=DEMO-TENANT-A ops=READ  access_key,bucket  2          2      slowdown           0     0
list-storm-per-client  ops=LIST                    source_ip          1          1      too_many_requests  0     0
writes-per-bucket      ops=WRITE                   bucket             500        1000   slowdown           0     0
frozen-bucket          bucket=frozen                                  0 (block)  -      slowdown           0     0
```

Say: four rules, each one line of intent in a YAML file; nothing in tollbooth writes that file.

## 2. Transparent: the client cannot tell it is there

```
$ head -c 65536 /dev/urandom > /tmp/demo-64k.bin
$ s3 cp /tmp/demo-64k.bin s3://training-data/ckpt/step-0001.bin
upload: /tmp/demo-64k.bin to s3://training-data/ckpt/step-0001.bin
$ s3 cp s3://training-data/ckpt/step-0001.bin /tmp/demo-back.bin
download: s3://training-data/ckpt/step-0001.bin to /tmp/demo-back.bin
```

Headers direct versus proxied, byte for byte (the demo backend serves a canned 4 KiB object):

```
$ diff <(curl -sI http://127.0.0.1:29000/training-data/ckpt/step-0001.bin | sort) \
       <(curl -sI $TOLLBOOTH_S3/training-data/ckpt/step-0001.bin | sort) && echo identical
identical
```

Say: no header added, nothing rewritten, every response chunk flushed as it arrives. A client
pointed at the proxy behaves exactly as it did pointed at the cluster.

## 3. A rule fires: SlowDown with Retry-After, the SDK backs off

Tenant A is allowed 2 reads per second per bucket. Eight requests at once, one attempt each:

```
$ for i in 1 2 3 4 5 6 7 8; do s3api head-object --bucket training-data --key ckpt/step-0001.bin & done; wait
ok
ok
An error occurred (503) when calling the HeadObject operation (reached max retries: 0): Service Unavailable
An error occurred (503) when calling the HeadObject operation (reached max retries: 0): Service Unavailable
… (5 of 8 denied; HEAD carries no body, so the CLI shows the status)
```

The raw response, so the audience sees exactly what S3 would have said:

```
$ curl -s -D - -o /dev/null -H "Authorization: AWS4-HMAC-SHA256 Credential=DEMO-TENANT-A/20261109/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=0" \
    $TOLLBOOTH_S3/training-data/ckpt/step-0001.bin   # run it three times quickly
HTTP/1.1 503 Service Unavailable
Content-Type: application/xml
Retry-After: 1
X-Amz-Retry-After: 138
X-Amz-Request-Id: 090e2122a80dd63e1634f927db7bb55c

<Error><Code>SlowDown</Code><Message>Please reduce your request rate.</Message>
<Resource>/training-data/ckpt/step-0001.bin</Resource><RequestId>090e…</RequestId><HostId>tollbooth</HostId></Error>
```

Now the same eight with the SDK's own retries on (what every real client does):

```
$ unset AWS_MAX_ATTEMPTS AWS_RETRY_MODE
$ for i in 1 2 3 4 5 6 7 8; do s3api head-object --bucket training-data --key ckpt/step-0001.bin > /dev/null && printf 'ok '; done; echo
ok ok ok ok ok ok ok ok
$ export AWS_RETRY_MODE=standard AWS_MAX_ATTEMPTS=1
```

Say: nothing is dropped; the client slows to the rate it was given. The limiter decision costs
under 5 µs at a million live keys; it adds no latency a client can measure.

Who is being throttled, right now:

```
$ tb rules top
RULE            ACCESS_KEY     BUCKET         PREFIX  SOURCE_IP  DENIED  LAST
tenant-a-reads  DEMO-TENANT-A  training-data                     5       6s ago
```

What would happen to a request, without sending it:

```
$ tb rules test -access-key DEMO-TENANT-A -bucket training-data -key ckpt/step-0001.bin -op GetObject
GetObject READ (key level): denied by tenant-a-reads (slowdown, Retry-After 1s); 1 rules match
RULE            KEY                                     LIVE_KEYS  VERDICT  RETRY_AFTER  MODE
tenant-a-reads  key=DEMO-TENANT-A bucket=training-data  1          deny     1s           slowdown
```

## 4. A different answer for a different problem: 429 for list storms

```
$ for i in 1 2 3 4; do s3 ls s3://training-data/ & done; wait
An error occurred (TooManyRequests) when calling the ListObjectsV2 operation (reached max retries: 0): Please reduce your request rate.
An error occurred (TooManyRequests) when calling the ListObjectsV2 operation …
An error occurred (TooManyRequests) when calling the ListObjectsV2 operation …
```

Say: per source address, one listing per second, `429 TooManyRequests` because that is what this
customer's client stack handles better; each rule picks its mode.

## 5. The stop button: a frozen bucket

```
$ s3 cp /tmp/demo-64k.bin s3://frozen/x.bin
upload failed: … An error occurred (SlowDown) when calling the PutObject operation (reached max retries: 0): Please reduce your request rate.
$ tb rules test -access-key DEMO-TENANT-B -bucket frozen -key x -op PutObject
PutObject WRITE (key level): denied by frozen-bucket (slowdown, Retry-After 1s); 2 rules match
```

Say: `rate: 0` is incident response: a runaway job or a compromised key stops in one reload,
with no change to the cluster and no client reconfiguration.

## 6. Change a rule live

Edit the file (tenant A from 2/s to 50/s), see the difference, apply it:

```
$ sed -i 's/limit: {rate: 2, burst: 2}/limit: {rate: 50, burst: 100}/' $TOLLBOOTH_DEMO_CONFIG
$ tb config diff --config $TOLLBOOTH_DEMO_CONFIG | tail -1
1 keys differ; run `tollbooth config reload` to apply …/demo/state/tollbooth.yaml
$ tb config reload --config $TOLLBOOTH_DEMO_CONFIG
ok           true
generation   2
rules        4
duration_ms  0.47
```

Repeat the burst from step 3: all eight succeed. The TUI's status screen shows generation 2.
Then show that a bad file cannot take the proxy down:

```
$ echo "bogus: 1" >> $TOLLBOOTH_DEMO_CONFIG
$ tb config reload --config $TOLLBOOTH_DEMO_CONFIG
ok           false
error        bogus: unknown key
tollbooth config reload: rejected: bogus: unknown key
$ sed -i '/^bogus: 1$/d' $TOLLBOOTH_DEMO_CONFIG     # and put the rate back if you want step 3 again
```

Say: unknown keys are errors, a rejected reload keeps the running config, the swap is one pointer
(0.5 ms here, 0.6 ms measured under load), in-flight requests never notice.

## 6b. The console: graphs, and the rule change without leaving the TUI (PR #27)

In the TUI terminal, `6` is the Graphs screen: braille line charts of requests and throttles per
second, backend time to first byte, denials per second by rule, requests in flight, heap and RSS,
goroutines, over the last ten minutes of polls; `space` pauses, `r` resets. Run
`demo/run.sh load 60` first so the lines move, then repeat the burst from step 3 and watch the
throttle line and the `tenant-a-reads` denial line rise together.

Then `5`, `e`: the config file opens in the TUI's editor. `i` to insert, change `rate: 2` under
`tenant-a-reads` to `rate: 50`, `esc`; the line under the text says `valid: 4 rules` on every
keystroke (type a stray word and it says `INVALID:` with the key and line instead). `ctrl+r` saves
the file (a `tollbooth.yaml.<time>.bak` appears beside it) and reloads the proxy in one step; the
bottom line reports the new generation. `esc` closes the editor.

Say: the operator never leaves the console; the save is explicit, validated, atomic, backed up,
and refused if someone else changed the file meanwhile. The CLI still never writes the file.

## 7. Showback: who used what, by the hour, as a file

Make a second tenant visible, wait for the ten-second flush (production default 60 s):

```
$ AWS_ACCESS_KEY_ID=DEMO-TENANT-B s3 cp /tmp/demo-64k.bin s3://checkpoints/run-7/part-1.bin
$ sleep 10
$ tb usage query --config $TOLLBOOTH_DEMO_CONFIG --group-by access_key,bucket
ACCESS_KEY     BUCKET         REQUESTS  BYTES_IN  BYTES_OUT  PARTIAL
DEMO-TENANT-A  frozen         1         0         0          true
DEMO-TENANT-A  training-data  44        65536     67093      true
DEMO-TENANT-B  checkpoints    4         196608    0          true
1 files, 5 groups, 51 requests, 262144 bytes in, 67284 bytes out; includes the open hour (partial)

$ tb usage top --config $TOLLBOOTH_DEMO_CONFIG --by bucket
BUCKET         REQUESTS  BYTES_IN  BYTES_OUT  PARTIAL
training-data  45        65536     67093      true
checkpoints    4         196608    0          true
```

The chargeback export, and the file behind it:

```
$ tb usage query --config $TOLLBOOTH_DEMO_CONFIG --group-by access_key,bucket,operation --format csv
access_key,bucket,operation,requests,bytes_in,bytes_out,partial
DEMO-TENANT-A,training-data,GetObject,19,0,65536,true
DEMO-TENANT-A,training-data,HeadObject,15,0,0,true
DEMO-TENANT-A,training-data,PutObject,1,65536,0,true
DEMO-TENANT-B,checkpoints,PutObject,3,196608,0,true
…
$ ls demo/state/showback/current/
498403-demo-a01.ndjson
$ head -2 demo/state/showback/current/498403-demo-a01.ndjson
{"schema_version":1,"node_id":"demo-a01","hour_utc":498403,"rows":10,"requests":51,"bytes_in":262144,"bytes_out":67284,"partial":true,…}
{"hour_utc":498403,"node_id":"demo-a01","access_key":"DEMO-TENANT-A","bucket":"training-data","prefix":"","operation":"GetObject","status_class":"2xx","requests":19,"bytes_in":0,"bytes_out":65536}
$ tb usage verify --config $TOLLBOOTH_DEMO_CONFIG
1 files, 1 nodes, 0 problems
```

Say: one row per hour per (access key, bucket, prefix, operation, status class); plain NDJSON
with a header line, sealed at the hour boundary, never rewritten; any tool reads it. The
increment costs 198 ns and keeps no per-object state: a million objects or a billion, the same
memory. `usage verify` proves no hour is missing.

## 8. When the cluster misbehaves, the proxy gets out of the way

```
$ demo/run.sh chaos fail=503          # the demo backend answers 503 to everything
$ curl -s -o /dev/null -w '%{http_code}\n' $TOLLBOOTH_S3/training-data/ckpt/step-0001.bin
503                                   # the backend's own answer, passed through untouched
$ demo/run.sh chaos clear=1
$ tb backends list
ENDPOINT         STATE  IN_FLIGHT  REQUESTS  ERRORS  DIAL  TLS  TIMEOUT  RESET  OTHER  TTFB_MEAN_MS  TTFB_LAST_MS  LAST_ERROR
127.0.0.1:29000  up     0          47        0       0     0    0        0      0      0.5           0.4           -
```

Say: fail open, always; there is no fail-closed knob. If the limiter itself ever panicked, the
request is forwarded and a counter increments. The proxy retries a dead endpoint only before any
request byte has been sent, so nothing is ever duplicated.

## 9. Everything above, as metrics

```
$ curl -s http://$TOLLBOOTH_ADMIN/metrics | grep -E '^tollbooth_' | grep -v ' 0$' | head
tollbooth_limiter_denied_total{rule="list-storm-per-client"} 4
tollbooth_limiter_denied_total{rule="tenant-a-reads"} 5
tollbooth_requests_total{op_class="READ",operation="HeadObject",result="forwarded",status_class="2xx"} 15
tollbooth_requests_total{op_class="READ",operation="HeadObject",result="throttled",status_class="throttled"} 5
…
```

Say: 21 metrics, every one in `docs/telemetry-catalog.md`, labels bounded by configuration, never
by object key or access key.

## Running it against a real S3 endpoint (the POC)

The same stack, with the null backend replaced by the cluster's S3 endpoint. Everything else is
the same: the proxy adds no headers and rewrites nothing, so clients only change their endpoint.

```
export AWS_ACCESS_KEY_ID=<your key> AWS_SECRET_ACCESS_KEY=<your secret>
export LOAD_BUCKET=<an existing bucket you can write to>
BACKEND=<s3 host>:443 BACKEND_SCHEME=https BACKEND_INSECURE=true demo/run.sh start   # INSECURE only for a lab certificate
source demo/env.sh
tb doctor --config $TOLLBOOTH_DEMO_CONFIG           # backend <host>: ok, tcp and HEAD / answered
s3 ls s3://$LOAD_BUCKET/                            # real listing, through the proxy
```

1. **Put your key on a rule.** `tb tui --config $TOLLBOOTH_DEMO_CONFIG`, `5`, `e`, `i`: on the
   `tenant-a-reads` rule change `access_keys: [DEMO-TENANT-A]` to `access_keys: [<your key>]` and
   `rate: 2, burst: 2` to something above the load you are about to run, say `rate: 200, burst: 200`;
   `esc`, `ctrl+r`. The bottom line says `reloaded: generation 2`.
2. **Start traffic.** In another shell, `demo/run.sh load 100`: 100 requests per second of GET,
   PUT, and HEAD against `$LOAD_BUCKET` as your key, after seeding 200 small objects so the reads
   find something. `6` in the TUI: the request line climbs to 100/s, the backend TTFB line shows
   the cluster's answer time, throttles stay at zero. Give it a minute of history.
3. **Lower the limit.** `5`, `e`, `i`, change `rate: 200, burst: 200` to `rate: 30, burst: 30`,
   `esc`, `ctrl+r`. Back on `6`: within two polls the throttle line rises, the request line (what
   completes) drops to about 30/s, and `denied per second, by rule` shows `tenant-a-reads`
   carrying the difference. The generator keeps running: its SDK honors `Retry-After` and backs
   off, nothing is lost, the cluster sees 30/s. `2` shows the rule with live keys and the denied
   count; `enter` on it shows your key as the one being held.
4. **Raise it again** the same way and watch the lines cross back. `demo/run.sh load 300` restarts
   the generator at another rate; `demo/run.sh stop` ends the load and the proxy.
5. **Showback** (`3`, or `tb usage query --config $TOLLBOOTH_DEMO_CONFIG --group-by access_key,bucket`)
   shows your key and bucket with the requests and bytes of the run, by hour, in files under
   `demo/state/showback/`.

For a host install rather than the demo stack: `make release`, the tarball's `tollbooth.service`,
`demo/tollbooth.poc.yaml` with the three fields filled in, `docs/OPERATIONS.md` §1 and §2. For a
measured number on the cluster: `make bench-e2e` on the proxy host with `-direct` at the cluster
and `-proxied` at tollbooth, `bench/crosscheck.sh` with elbencho, and `make soak` overnight
(`docs/adr/0005-performance-targets-at-v0.1.0.md` says what has and has not been verified where).
