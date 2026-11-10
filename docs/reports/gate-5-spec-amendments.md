# Specification amendments after Gate 5: what to change, and to what

Blake's answer to Gate 5 question 7 (2026-11-10): the specification is edited by Blake or Fable, not
by the builder. This file is the hand-off: every place where `docs/SPEC.md` (and one line of
`CLAUDE.md`) now disagrees with an accepted ADR or a recorded decision, with the current wording,
the proposed wording, and the source. Line numbers are those of `docs/SPEC.md` at `phase-5`
1c24d94. Nothing here has been applied.

Until the edits are made, the ADRs and `docs/DECISIONS.md` are what the code follows.

## 1. Rate limiting: the rule schema (ADR-0007, D-058, D-062)

**Line 183, current:**

> A rule has three parts: **match** (which requests it applies to), **group\_by** (how many
> independent token buckets it creates), and **limit** (rate and burst). All matching rules must
> allow a request; if any denies, the request is throttled.

**Proposed:**

> A rule answers four questions in S3's own words: **which requests** (`operation`, and optionally
> `access_key`, `bucket`, `prefix`, `source_ip`), **how many** (`limit_per_second`,
> `limit_per_minute`, or `limit_per_hour`, or `blocked: true`), **counted how** (`per`: `total`, or
> who gets a budget of their own), and **what the client gets over the limit** (`respond`). All
> matching rules must allow a request; if any denies, the request is throttled. Every rule is read
> back to the operator as one sentence wherever rules are shown (ADR-0007).

**Lines 185 to 206, the YAML block. Proposed replacement:**

```yaml
limits:
  - name: tenant-a-uploads
    operation: [PutObject, UploadPart, CompleteMultipartUpload]   # S3 operation names; Get* is a family; omitted = every operation
    access_key: [AKIAEXAMPLE1, AKIAEXAMPLE2]    # every listed filter must hold (AND); omitted = any
    bucket: [training-data, checkpoints]
    prefix: [ckpt/, logs/]                      # relative to the bucket; longest match wins
    source_ip: 10.20.0.0/16
    limit_per_second: 2000                      # exactly one of limit_per_second, limit_per_minute, limit_per_hour, blocked
    burst: 4000                                 # how many at once; default the limit's own number; 1 = evenly spaced (strict)
    per: [access_key, bucket]                   # required: total, or one GCRA state per distinct value or tuple
  - name: list-storm
    operation: ListObjectsV2
    limit_per_second: 50
    burst: 200
    per: source_ip
    respond: 429                                # 503 SlowDown (default) or 429 TooManyRequests
  - name: frozen-key
    access_key: AKIAREVOKED
    blocked: true                               # refuses every matching request
```

**Lines 210 to 214, the semantics list.** Replace the words, keep the meaning:

| Current | Proposed |
| --- | --- |
| "**Key of a bucket** = `rule.id` + the values of the `group_by` dimensions. `prefix` as a `group_by` dimension means … An empty `group_by` means one shared bucket for everything the rule matches." | "**Key of a bucket** = the rule's `name` + the values of the `per` dimensions. `prefix` as a `per` dimension means the matched configured prefix, not the raw object key. `per: total` means one shared bucket for everything the rule matches; `per` is required on a limit, so sharing is always written down." |
| "… decides the response mode (that rule's `on_limit`) …" | "… decides the response (that rule's `respond`) …" |
| "a bitmask for ops" | "a bitmask over the operations the rule names" |
| "**Rate 0** blocks. Rates are per node unless `cluster.node_count` is set …" | "**`blocked: true`** blocks. Limits are per node unless `cluster.node_count` is set …" |

**Line 216 (GCRA):** "`rate: 0` is a separate branch that always denies" becomes "a blocked rule is
a separate branch that always denies". Add one sentence: "`burst: 1` is strict pacing: one request,
then the next after `1/rate`."

**Lines 220 and 221 (enforcement modes):** the bullets are named `slowdown` and
`too_many_requests`. Proposed names: "`respond: 503` (the default)" and "`respond: 429`". The
bodies, headers, and behavior described are unchanged.

**Line 225 (hot reload):** "Limiter state is keyed by rule id" becomes "keyed by rule name".

`rate_limits.default_on_limit` no longer exists; each rule says its own, default 503.

## 2. Operator surface: the console writes the config on an explicit save (ADR-0006, ADR-0007)

**Line 354, current:**

> Neither the CLI nor the TUI writes the config file. Rules change by editing the file and running
> `tollbooth config reload` (or pressing `r` in the TUI); the Limits screen offers read-only views
> and a dry-run test, not an editor. This is shunt's rule (its config package must never rewrite
> the operator's config) and it removes atomic rewrites, comment preservation, and a race with a
> human editing the same file.

**Proposed:**

> The CLI never writes the config file. The TUI writes it only on an explicit save, from two
> places: the Limits screen's guided form, which adds, changes, or deletes one rule and leaves
> every other byte of the file as it was (ADR-0007), and the Config screen's editor (ADR-0006).
> Every save is validated first, atomic, leaves a timestamped backup beside the file, and is
> refused when the file changed on disk since it was read. Rules can still be changed by editing
> the file and running `tollbooth config reload`.

**Line 355, current:** "The TUI is read-only and off the data path: …"
**Proposed:** "The TUI is off the data path and read-only except for those explicit saves: …" (the
rest of the sentence stands).

**Line 382, the Limits row, current:**

> Rules table: id, match summary, `group_by`, rate and burst, mode, live keys, denied/s. Detail
> pane: top denied keys for the selected rule | `enter` detail, `t` test-request form (the
> `rules test` fields), `/` filter

**Proposed:**

> Rules table: name, the rule in words, matched/s, throttled/s; a rule that has matched nothing
> while traffic flows is marked. Under the selected rule: its sentence and who is being held. Below:
> the busiest S3 operations right now and the limit that covers each | `n` new, `enter` change,
> `d` delete (the guided form), `t` test-request form (the `rules test` fields), `/` filter

The screen table has six screens since ADR-0006: add a Graphs row (`6`: line charts of request and
throttle rates, backend time to first byte, denials per rule, in-flight, memory; `space` pause,
`r` reset), and the Config row gains `e` (edit the file).

## 3. "Nothing in tollbooth writes the config file" (ADR-0006, ADR-0007)

**`docs/SPEC.md` line 472 and `CLAUDE.md` line 22, current:**

> Config is validated at startup and by `check-config`; unknown keys are errors. Nothing in
> tollbooth writes the config file.

**Proposed (both files):**

> Config is validated at startup and by `check-config`; unknown keys are errors. The only thing
> that writes the config file is an explicit save in `tollbooth tui` (ADR-0006, ADR-0007); the CLI
> and the proxy never do.

## 4. Test plan category 7: default retry settings (D-044, Gate 2)

**Line 421, current:** "Each SDK, with its default retry settings, completes a 200-object sync
against a rule whose `rate` is half the client's concurrency and whose `burst` equals it, …"

**Proposed:** "Each client tool and SDK syncs 200 objects against a rule whose limit is half the
client's concurrency and whose burst equals it, in its default retry mode and, where it has one,
in adaptive mode; the result is recorded per client and mode in `docs/THROTTLE.md`. Measured at
every gate since Gate 2: aws cli v2 and boto3 do **not** complete in their default mode and do in
adaptive mode; s5cmd, rclone, and s3cmd complete with their defaults (D-044)." The rest of the row
(the 503 body, `Retry-After`, `Expect: 100-continue`, 429) stands.

## 5. Test plan category 8: the hour of a request that crosses the boundary (D-047, Gate 3)

**Line 422, current:** "A request starting at 10:59:59.9 and ending at 11:00:00.1 lands in hour
10."

**Proposed:** "… lands in hour 11: the hour is the completion hour, read when the request is
recorded (D-025, D-047)." The showback section's own column definition already says so
(`hour_utc` is `floor(completion_unix / 3600)`); this sentence contradicted it.

## 6. Phased delivery plan: the Phase 6 row

**Line 407, current list:** "Hold-and-forward (shaping) mode; cluster-coordinated limits; bandwidth
limits; Parquet export; OpenTelemetry; per-source connection caps; rule editing from the CLI/TUI;
backend drain; a web UI only if the TUI proves insufficient in the field"

**Proposed list:** "Hold-and-forward (shaping) mode (ADR-0004 holds the field evidence);
cluster-coordinated limits; bandwidth limits; Parquet export; OpenTelemetry; per-source connection
caps; backend drain; observe-only rules (count what a rule would throttle without throttling);
live per-key request rates for the console; a web UI only if the TUI proves insufficient in the
field". Rule editing from the TUI was delivered in Phase 5 (ADR-0006, ADR-0007); the CLI still
does not write the file.

## 7. The bench cross-check: elbencho in place of warp (ADR-0005, accepted 2026-11-10)

**Line 106 (open decision 2), line 310 (load generator), line 486 (licensing).** Each names MinIO
`warp` as the independent cross-check. Proposed: name **elbencho** instead, with the reason in one
clause (MinIO archived its open-source projects, D-022; elbencho is already pinned for the
compatibility matrix and is run only as an external process). Line 486's list of copyleft tools
keeps elbencho and Garage and drops MinIO server and `warp`.

## 8. What v0.1.0 claims (ADR-0005, accepted 2026-11-10)

No wording in the specification has to change for this, but the Phase 5 gate text ("all
performance targets met or ADR'd") is satisfied by ADR-0005 option (b): five rows met on the dev
box, four end-to-end rows published as **unverified on the reference node**, and the
reference-node run made the gate for v0.1.1. If the specification should say that itself, the
Performance targets section is the place: one sentence under the table.
