# tollbooth demo: the walkthrough

Twelve minutes, one machine, two terminals. Every command below was run as written against the demo
stack (`demo/run.sh start`) and the outputs are what it printed, trimmed. The audience sees one
thing per step: the proxy is invisible until a rule fires, a rule is a few plain lines in S3's own
words that the tool reads back as a sentence, a reload is instant, the hour's usage is a file they
can open.

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
NAME                LIMIT           PER         OPERATION                                     ONLY_FOR                  RESPOND  KEYS  MATCHED  DENIED
tenant-a-reads      2 per second    bucket      GetObject,HeadObject                          access_key=DEMO-TENANT-A  503      0     0        0
list-storm          1 per second    source_ip   ListObjectsV2                                 any                       429      0     0        0
uploads-per-bucket  500 per second  bucket      PutObject,UploadPart,CompleteMultipartUpload  any                       503      0     0        0
frozen-bucket       blocked         access_key  any                                           bucket=frozen             503      0     0        0

tenant-a-reads      At most 2 GetObject and HeadObject calls per second on each bucket. Only for requests by access key DEMO-TENANT-A. Over the limit, the client gets 503 SlowDown.
list-storm          At most 1 ListObjectsV2 call per second from each source IP. Over the limit, the client gets 429 TooManyRequests.
uploads-per-bucket  At most 500 PutObject, UploadPart, and CompleteMultipartUpload calls per second on each bucket, up to 1,000 at once. Over the limit, the client gets 503 SlowDown.
frozen-bucket       Refuses every request on bucket frozen. The client gets 503 SlowDown.

4 rules, 0 live keys
```

The first of them, as it is written in the file:

```yaml
limits:
  - name: tenant-a-reads
    operation: [GetObject, HeadObject]
    access_key: DEMO-TENANT-A
    limit_per_second: 2
    per: bucket
```

Say: four rules, each a few plain lines in a YAML file, written in the operation names S3 itself
uses; the tool reads every rule back as a sentence, so nobody has to trust their reading of the
file. `MATCHED` beside `DENIED` says whether a rule is seeing the traffic it was written for.

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

Tenant A is allowed 2 GetObject or HeadObject calls per second on each bucket. Eight requests at
once, one attempt each:

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

Say: these eight complete, because the client's own retries absorb the SlowDown. A client that
stays far above its limit eventually gets SlowDown back from its SDK; which tools and retry modes
cope, and at what overload, is measured in `docs/OPERATIONS.md` §9. The limiter decision costs
under 5 µs at a million live keys; it adds no latency a client can measure.

Who is being throttled, right now:

```
$ tb rules top
RULE            ACCESS_KEY  BUCKET         PREFIX  SOURCE_IP  DENIED  LAST
tenant-a-reads              training-data                     5       1s ago
```

(The rule counts `per: bucket`, so the bucket is what it names; the access key is the rule's own
filter.)

What would happen to a request, without sending it:

```
$ tb rules test -access-key DEMO-TENANT-A -bucket training-data -key ckpt/step-0001.bin -op GetObject
GetObject (key level): denied by tenant-a-reads (503 SlowDown, Retry-After 1s); 1 rule matches
RULE            KEY                   LIVE_KEYS  VERDICT  RETRY_AFTER  RESPOND
tenant-a-reads  bucket=training-data  1          deny     1s           503
```

(Right after the burst. A second later the same command says `allowed`: the bucket has refilled.)

## 4. A different answer for a different problem: 429 for list storms

```
$ for i in 1 2 3 4; do s3 ls s3://training-data/ & done; wait
An error occurred (TooManyRequests) when calling the ListObjectsV2 operation (reached max retries: 0): Please reduce your request rate.
An error occurred (TooManyRequests) when calling the ListObjectsV2 operation …
An error occurred (TooManyRequests) when calling the ListObjectsV2 operation …
```

Say: the rule names the operation, `ListObjectsV2`, which is what `aws s3 ls` sends; each source
address may list once a second, and the rule says `respond: 429` because that is what this
customer's client stack handles better.

## 5. The stop button: a frozen bucket

```
$ s3 cp /tmp/demo-64k.bin s3://frozen/x.bin
upload failed: … An error occurred (SlowDown) when calling the PutObject operation (reached max retries: 0): Please reduce your request rate.
$ tb rules test -access-key DEMO-TENANT-B -bucket frozen -key x -op PutObject
PutObject (key level): denied by frozen-bucket (503 SlowDown, Retry-After 1s); 2 rules match
RULE                KEY                LIVE_KEYS  VERDICT  RETRY_AFTER  RESPOND
uploads-per-bucket  bucket=frozen      1          allow    -            503
frozen-bucket       key=DEMO-TENANT-B  0          deny     1s           503
```

Say: `blocked: true` is incident response: a runaway job or a compromised key stops in one reload,
with no change to the cluster and no client reconfiguration.

## 6. Change a rule live

Edit the file (tenant A from 2/s to 50/s), see the difference, apply it:

```
$ sed -i 's/limit_per_second: 2$/limit_per_second: 50/' $TOLLBOOTH_DEMO_CONFIG
$ tb check-config $TOLLBOOTH_DEMO_CONFIG | grep tenant-a-reads
  tenant-a-reads: At most 50 GetObject and HeadObject calls per second on each bucket. Only for requests by access key DEMO-TENANT-A. Over the limit, the client gets 503 SlowDown.
$ tb config diff --config $TOLLBOOTH_DEMO_CONFIG
KEY                                      RUNNING  FILE
limits[tenant-a-reads].limit_per_second  2        50

1 keys differ; run `tollbooth config reload` to apply …/demo/state/tollbooth.yaml
$ tb config reload --config $TOLLBOOTH_DEMO_CONFIG
ok           true
generation   2
rules        4
duration_ms  0.42
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

## 6b. The console: add a limit without opening the file

The request the form is built for: *"we have too many GetObject calls coming in; limit them to 20 a
second for each access key."* Run `demo/run.sh load 100` first so there is traffic to look at.

In the TUI terminal, `2` is the Limits screen: the rules in words with what each matches and
throttles per second, and under them the busiest S3 operations right now (frames below are from the
running demo, 112 columns):

```
  LIMIT              MEANS                                                                 MATCHED/S THROTTLED/S
▸ tenant-a-reads     GetObject, HeadObject: 2 per second on each bucket; key DEMO-TENAN…           0           0
  list-storm         ListObjectsV2: 1 per second from each source IP                               0           0
  uploads-per-bucket PutObject +2: 500 per second on each bucket                                  22           0
  frozen-bucket      all requests: blocked; bucket frozen                                          0           0

  BUSIEST OPERATIONS RIGHT NOW            REQ/S  THROTTLED/S  LIMITED BY
  GetObject                                  71            0  tenant-a-reads (part)
  PutObject                                  23            0  uploads-per-bucket
  HeadObject                                7.0            0  tenant-a-reads (part)
```

`n` opens the form. Type `get`: the list under the field shows the S3 operations that match, the
ones in traffic first with their rates. `enter` takes `GetObject`. `tab`, `20`. `tab` three times to
**Counted** (past the unit and "at once"), `→` until `each access key`:

```
│ New limit                                                                                                    │
│   Limit what     GetObject                                                                                   │
│   To at most     20  per second   at once: same as the limit                                                 │
│ ▸ Counted         ‹ each access key ›                                                                        │
│   Only for       access key any                                                                              │
│                  bucket     any                                                                              │
│                  prefix     any                                                                              │
│                  source IP  any                                                                              │
│   Over the limit 503 SlowDown (what S3 itself answers)                                                       │
│   Name           getobject-per-access-key                                                                    │
│   Note           why this limit exists (optional)                                                            │
│ ── who gets a budget of their own: ←/→ to choose ─────────────────────────────────────────────────────────── │
│   ( ) everyone together               all matching requests share one budget                                 │
│   ( ) each source IP                  a budget of its own for every client                                   │
│   (•) each access key                 a budget of its own for every key                                      │
│   ( ) each bucket                     a budget of its own for every bucket                                   │
│   ( ) each access key on each bucket  a budget of its own for every pair                                     │
│                                                                                                              │
│ Means   At most 20 GetObject calls per second for each access key. Over the limit, the client gets 503       │
│         SlowDown.                                                                                            │
│ Now     GetObject: 73/s right now                                                                            │
```

One more `tab` shows what the **Only for** fields offer: the access keys the proxy has seen in the
last two hours, busiest first (`AKIALOAD002  6,579 requests lately` …), so nobody types a key.
Then `ctrl+r` (from any field): the bottom line says `reloaded: generation 2, 5 rules, 0.6 ms`
(generation 3 if you did step 6), a `tollbooth.yaml.<time>.bak` is beside the file, and the file
has gained exactly this, with every other byte and comment as it was:

```yaml
  - name: getobject-per-access-key
    operation: GetObject
    limit_per_second: 20
    per: access_key
```

Within one poll the Limits screen shows the rule at work (three load keys at about 25 GetObject a
second each, against 20):

```
  getobject-per-access-key GetObject: 20 per second for each access key                           76         1.5
```

`enter` on the rule opens the same form with the cursor on the number: replace `20` with `5`,
`ctrl+r`. The detail under the rule now names who is being held, and `6` (Graphs) shows the
throttle line rising:

```
▸ getobject-per-access-key GetObject: 5 per second for each access key                            17         3.0

  getobject-per-access-key: At most 5 GetObject calls per second for each access key. Over the limit, the
  client gets 503 SlowDown.
  held in the last minute: key=AKIALOAD000 (95) · key=AKIALOAD002 (82) · key=AKIALOAD001 (40)
```

(The request rate falls with it, from 100/s to about 25/s here: the generator's four workers spend
their time in the SDK's retry backoff. A client held this far under its demand slows itself down.)
`d` on the rule, `y`: it is deleted and the change applied. After the add, the change, and the
delete the config file was byte-identical to the one the demo started with.

A rule that has matched no request since it was loaded, while traffic flows, is marked `none ⚠`
after a minute. If you came here without steps 3 to 5, `tenant-a-reads`, `list-storm`, and
`frozen-bucket` show it, correctly: the background load uses other keys, never lists, and never
touches the frozen bucket. It is the mark that catches a rule written for the wrong access key.

Say: the question was asked in S3's words and answered in them; the operator picked the operation
and the key from what the proxy is seeing, read the rule back as a sentence before saving, and
watched it bite. The save is explicit, validated, atomic, backed up, and refused if someone else
changed the file meanwhile. The file is still the source of truth, and `5`, `e` opens it in the
console's editor (validated on every keystroke, `ctrl+r` saves and reloads) for anyone who would
rather write the four lines. The CLI still never writes the file.

`6` is the Graphs screen: braille line charts of requests and throttles per second, backend time
to first byte, denials per second by rule, requests in flight, heap and RSS, goroutines, over the
last ten minutes of polls; `space` pauses, `r` resets.

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
tollbooth_limiter_denied_total{rule="list-storm"} 3
tollbooth_limiter_denied_total{rule="tenant-a-reads"} 5
tollbooth_limiter_matched_total{rule="list-storm"} 4
tollbooth_limiter_matched_total{rule="tenant-a-reads"} 8
tollbooth_requests_total{op_class="LIST",operation="ListObjectsV2",result="throttled",status_class="throttled"} 3
tollbooth_requests_total{op_class="READ",operation="HeadObject",result="forwarded",status_class="2xx"} 3
tollbooth_requests_total{op_class="READ",operation="HeadObject",result="throttled",status_class="throttled"} 5
…
```

Say: 22 metrics, every one in `docs/telemetry-catalog.md`, labels bounded by configuration, never
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

1. **Start traffic.** In another shell, `demo/run.sh load 100`: 100 requests per second of GET,
   PUT, and HEAD against `$LOAD_BUCKET` as your key, after seeding 200 small objects so the reads
   find something. `tb tui --config $TOLLBOOTH_DEMO_CONFIG`, `6`: the request line climbs to
   100/s, the backend TTFB line shows the cluster's answer time, throttles stay at zero (the demo
   rules name `DEMO-TENANT-A`, not your key). Give it a minute of history.
2. **See what is arriving.** `2` (Limits): under the rules, "Busiest operations right now" lists
   GetObject at about 70/s, PutObject at 20/s, HeadObject at 10/s, each with the limit that covers
   it or `no limit`.
3. **Limit it.** `n`; type `get`, `enter` takes GetObject; `tab`, `30`; `tab` three times to
   Counted, `→` to `each access key`; one more `tab` and `↓`, `enter` picks your key from the keys
   the proxy has seen (optional: without it the rule gives every key its own 30 a second). The
   Means line reads the rule back; `ctrl+r` saves and applies. Within two polls the rule's row
   shows matched about 70/s and a throttled rate; `6` shows the `throttled` line rising and
   `denied per second, by rule` naming the rule.
   Two things to say out loud. The `req` line counts every request the proxy answers, throttled
   ones included; requests minus throttled is what reaches the cluster. And the generator's SDK
   retries a SlowDown with its own backoff (it does not read `Retry-After` by default, D-043):
   held far below its demand, its workers spend their time backing off, the offered rate itself
   falls (100/s to about 25/s with the null backend and a limit of 5), and some operations use up
   their retries and fail with SlowDown. That is what a tenant over its limit should see, and
   `demo/state/loadgen.log` counts them as errors at the end of the run.
4. **Change it, then remove it.** `2`, select the rule, `enter`: the cursor is on the number. Type
   a higher one, `ctrl+r`, and watch the lines cross back. "At once" is the burst: it defaults to
   the limit's own number, and `↓`, `enter` on the field chooses "evenly spaced" (strict, one at a
   time); a burst far above the limit passes that many requests before the limit
   bites (`limit_per_second: 30` with `burst: 250` is about six seconds of GetObject at 70/s). `d`, `y`
   deletes the rule. `demo/run.sh load 300` restarts the generator at another rate;
   `demo/run.sh stop` ends the load and the proxy. The same changes by hand: `5`, `e` opens the
   file in the editor.
5. **Showback** (`3`, or `tb usage query --config $TOLLBOOTH_DEMO_CONFIG --group-by access_key,bucket`)
   shows your key and bucket with the requests and bytes of the run, by hour, in files under
   `demo/state/showback/`.

For a host install rather than the demo stack: `make release`, the tarball's `tollbooth.service`,
`demo/tollbooth.poc.yaml` with the three fields filled in, `docs/OPERATIONS.md` §1 and §2. For a
measured number on the cluster: `make bench-e2e` on the proxy host with `-direct` at the cluster
and `-proxied` at tollbooth, `bench/crosscheck.sh` with elbencho, and `make soak` overnight
(`docs/adr/0005-performance-targets-at-v0.1.0.md` says what has and has not been verified where).
