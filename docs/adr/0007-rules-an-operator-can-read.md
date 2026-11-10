# ADR-0007: Rules an operator can read: S3 vocabulary, plain keys, a sentence read back, and a guided form

Status: **proposed** (2026-11-10). Blake decides; Fable grades. This ADR challenges the rule schema
in the specification's Rate limiting section and extends ADR-0006 (the TUI may write the config).

## Context

Feedback from the first demo (Blake, 2026-11-10), verbatim in substance:

> The config file needs to be more straightforward and clear. There's no WRITE in S3, there's no
> LIST. Let's be specific about what we're setting and to what limit. I don't know if a YAML file is
> the right format or key value, but we need something more natural to understand for a layperson:
> someone who's told "we have too many ListObjectsV2 calls coming in, we need to limit them to only
> 20/s", and it should be clear what we're setting to adjust it. Maybe an interactive prompt in the
> TUI would help in addition.

The rule the audience was looking at:

```yaml
- id: tenant-a-reads
  match: {access_keys: [DEMO-TENANT-A], ops: [READ]}
  group_by: [access_key, bucket]
  limit: {rate: 2, burst: 2}
```

What went wrong in that room, and what the code says about each point:

| What the operator met | What is underneath |
| --- | --- |
| `ops: [READ]`, `[WRITE]`, `[LIST]`: words that are not in the S3 API | Operation classes are tollbooth's own grouping. `LIST` is 7 operations (ListBuckets, ListObjects, ListObjectsV2, ListObjectVersions, ListMultipartUploads, ListParts, ListObjectAnnotations); `READ` is 10, `WRITE` 15, `DELETE` 5, `BUCKET` 75. Nothing on screen says which. The limiter already matches single operations (`ops` accepts any of the 113 operation names; the matcher is a bitset per operation), but the examples, the demo config, and the docs all use the classes. |
| `group_by: [access_key, bucket]` | The most important question about a limit, "20 a second for whom: everyone together, or each client?", is answered by a key whose name comes from SQL and whose absence silently means "everyone shares one budget". |
| `limit: {rate: 2, burst: 2}`, `rate: 0` to block | Two numbers in a nested inline map, no unit, and a magic zero. |
| The rule named the wrong access key, so it matched nothing; the demo showed 300 req/s and 0 throttled for several minutes | Nothing warns that a rule matches no traffic. `KEYS 0` on the Limits screen was the only tell. A mistyped operation name is rejected, but without a suggestion (`ListObjectV2` versus `ListObjectsV2`). |
| Typing a 20-character access key into a YAML flow map inside a text editor | The proxy has already seen that key in traffic; the operator should pick it, not type it. |

So there are four separate problems: **vocabulary** (tollbooth's words instead of S3's), **shape**
(nested inline maps, implicit defaults), **feedback** (nothing reads the rule back or says it matches
nothing), and **entry** (a blank text file is the only way in).

## Principles

1. **S3's words, not ours.** A rule names operations exactly as the S3 API reference, an SDK error
   message, and an access log name them.
2. **A rule answers four questions, in the order an operator is asked them**: which requests, how
   many, counted how, and what happens over the limit. Every one is explicit; none has a silent
   default that changes who gets throttled.
3. **The tool says back what it understood.** Every rule is shown as one English sentence wherever
   rules appear. The sentence is generated, never parsed.
4. **Start from what is happening.** The operator points at the operation, key, or bucket that is
   too busy and types a number; they do not start from a blank file.
5. **The file stays the source of truth**, one file, readable and editable by hand, and what the
   form writes is exactly what a person would have written.

## Decision

### 1. The rule, rewritten (recommended: plain YAML keys)

The three rules of the specification's example, and the one from the demo brief, in the proposed form:

```yaml
limits:
  # "We have too many ListObjectsV2 calls coming in; limit them to 20 a second."
  - name: list-storm
    operation: ListObjectsV2
    limit: 20/s
    per: source_ip

  - name: tenant-a-uploads
    operation: [PutObject, UploadPart, CompleteMultipartUpload]
    access_key: [AKIAEXAMPLE1, AKIAEXAMPLE2]
    bucket: [training-data, checkpoints]
    limit: 2000/s
    burst: 4000
    per: [access_key, bucket]

  - name: revoked-key
    access_key: AKIAREVOKED
    limit: blocked
    per: total
    note: "INC-1234, key leaked 2026-11-09"
```

| Key | Meaning | Values |
| --- | --- | --- |
| `name` | What the rule is called in the console, the metrics, and the logs (was `id`) | required, unique |
| `operation` | **Which requests**: S3 API operation names | one name or a list; a trailing `*` matches a family (`Get*`); omitted = every operation |
| `access_key`, `bucket`, `prefix`, `source_ip` | **Which requests**, narrowed. The same four words as the showback columns | one value or a list; `source_ip` takes an address or a CIDR; omitted = any |
| `limit` | **How many** | `N/s`, `N/min`, or `blocked` |
| `per` | **Counted how**: who gets their own budget | **required**: `total` (everyone the rule matches shares one budget), or `access_key`, `bucket`, `prefix`, `source_ip`, or a list of them (each combination gets its own) |
| `burst` | How many may arrive at once before the rate applies | optional; default one second's worth |
| `respond` | **What happens over the limit**: the status the client gets | optional; `503` (SlowDown, the default, what S3 itself answers) or `429` (TooManyRequests) |
| `note` | Why the rule exists; shown in the console | optional free text |

What changes and what does not:

- **Vocabulary.** `operation` takes S3 operation names only. The class words (`READ`, `WRITE`,
  `LIST`, `DELETE`, `BUCKET`) are no longer accepted; writing one is an error that lists the
  operations it used to mean. A family is written `Get*`, and wherever a family appears the tool
  shows how many operations it covers and which.
- **`per` is required.** "20/s" without "for whom" is the ambiguity that throttles a whole cluster
  by accident. The file must say `per: total` to share one budget.
- **One vocabulary of four nouns.** `access_key`, `bucket`, `prefix`, `source_ip` are the filter
  keys, the values of `per`, and the showback columns. An operator learns them once.
- **Flat block style.** No inline `{…}` maps; one setting per line; the number an operator is asked
  to change sits alone on a line that starts with `limit:`.
- **Typos are caught with a suggestion**: `unknown operation "ListObjectV2"; did you mean
  ListObjectsV2?`. Operation names match case-insensitively and are shown in S3's spelling.
- **Unchanged**: everything below the schema. GCRA, the key store, the single pass (a request must
  pass every rule that matches it), the per-operation bitset, `cluster.node_count`, the response
  bodies, the 10 µs budget. Classes stay internal (the `op_class` metric label and the limiter's
  per-class index). This is a change to `internal/config` and to how rules are displayed, not to
  the limiter.
- **YAML traps avoided**: `*` cannot start a plain YAML scalar, so "every operation" is the absent
  key, not `operation: *`; only a trailing `*` is a wildcard.

Nothing has been tagged, so the old keys are replaced outright rather than kept beside the new ones:
one way to write a rule.

### 2. The sentence read back

Every rule is rendered as one sentence by the tool, from the parsed rule:

```
list-storm        Each source IP may make at most 20 ListObjectsV2 calls per second.
                  Over that, the client gets 503 SlowDown.
tenant-a-uploads  Access keys AKIAEXAMPLE1 and AKIAEXAMPLE2, on buckets training-data and checkpoints:
                  at most 2,000 PutObject, UploadPart, and CompleteMultipartUpload calls per second for
                  each access key on each bucket (up to 4,000 at once). Over that: 503 SlowDown.
revoked-key       Every request signed with access key AKIAREVOKED is refused with 503 SlowDown.
```

It appears in four places: `tollbooth check-config` and `tollbooth rules list` print it; the Limits
screen uses it as the rule's description; the editor shows the sentence for the rule under the
cursor on its verdict line, on every keystroke; the form shows it before anything is saved. An
operator checks their work by reading English, not by re-reading their own YAML.

### 3. Feedback that a rule is doing something

- **Matched, not only denied.** Each rule gets a `matched` counter beside `denied` (one atomic add
  per matching rule on the decision path; a catalogued metric, `tollbooth_limiter_matched_total{rule}`,
  with the same cardinality as `denied`). The Limits screen shows matched per second and throttled
  per second per rule.
- **"Matches nothing" is said out loud.** A rule with no matching request for five minutes while
  the proxy is serving traffic is flagged on the Limits screen and in `rules list`. This is the
  failure that cost the demo several minutes.
- **Requests by operation, live.** The console already scrapes `/metrics`; the per-operation request
  counters there give "ListObjectsV2 312/s, GetObject 1,204/s" with no change to the proxy. That
  table is where "we have too many ListObjectsV2 calls" becomes visible, and where a limit starts.

### 4. The guided form in the TUI

On the Limits screen `n` opens the form for a new limit, `enter` opens it on the selected rule, `d`
deletes after a confirmation. The raw editor on the Config screen stays for people who want the file.
Numbers below are illustrative.

```
 1 Overview  2 Limits  3 Usage  4 Backends  5 Config  6 Graphs
  LIMITS IN FORCE                                              matched/s  throttled/s
▸ list-storm        each source IP: 20 ListObjectsV2 per second      312          170
  tenant-a-uploads  2 keys, 2 buckets: 2,000 uploads per second       80            0
                    for each key on each bucket
  revoked-key       access key AKIAREVOKED: blocked                    0            0  ⚠ matches nothing (5 min)

  BUSIEST OPERATIONS RIGHT NOW          req/s
  GetObject                             1,204
  ListObjectsV2                           312   limited by list-storm
  PutObject                                96
  n new limit · enter change · d delete · l limit the selected operation · t test a request
```

```
╭ New limit ───────────────────────────────────────────────────────────────────╮
│ Limit what     List▏                                                         │
│                ▸ ListObjectsV2            312/s right now                    │
│                  ListObjects                0/s                              │
│                  ListBuckets                2/s                              │
│                  List*                    all 11 operations starting "List"  │
│ To at most     20  per second                                                │
│ Counted        ( ) everyone together   (•) each source IP                    │
│                ( ) each access key     ( ) each bucket                       │
│ Only for       any access key · any bucket · any prefix · any address        │
│ When over      (•) 503 SlowDown   ( ) 429 TooManyRequests                    │
│ Name           list-storm                                                    │
│ Note                                                                         │
│                                                                              │
│ Means  Each source IP may make at most 20 ListObjectsV2 calls per second.    │
│        Over that, the client gets 503 SlowDown.                              │
│ Now    ListObjectsV2 is arriving at 312/s.                                   │
│                                                                              │
│ ctrl+r save and apply · ctrl+s save only · esc cancel                        │
╰──────────────────────────────────────────────────────────────────────────────╯
```

- **Pickers, not typing.** The operation field searches the 113 names as you type and lists the
  ones in live traffic first with their rates. The access-key and bucket fields offer the values
  the proxy has recorded in the current showback hour, busiest first. Nobody types a 20-character
  key.
- **The four questions are the four fields**, in order, and "Counted" is a required choice.
- **Changing a limit is one field.** `enter` on a rule opens the same form with the cursor on the
  number.
- **What the form writes is the file.** It saves through ADR-0006's path (validated, atomic, backed
  up, refused if the file changed on disk). A changed value is a surgical edit of that one scalar,
  located with the YAML parser's line and column, so every other byte of the file, comments
  included, is untouched; a new rule is appended as the block shown in section 1; a deleted rule
  removes exactly its lines.
- **The CLI stays read-only.** `rules list` and `check-config` gain the sentences; no CLI verb
  writes the file.

## Alternatives considered

The same rule in each candidate:

```yaml
# A. Plain YAML keys (recommended)
- name: list-storm
  operation: ListObjectsV2
  limit: 20/s
  per: source_ip
```

```yaml
# B. One sentence per rule, as a string the tool parses
- list-storm: limit ListObjectsV2 to 20/s per source_ip
```

```ini
# C. key = value sections, like ~/.aws/config
[limit list-storm]
operation = ListObjectsV2
limit     = 20/s
per       = source_ip
```

| | A. plain YAML keys | B. sentence | C. key = value |
| --- | --- | --- | --- |
| Reads naturally to a layperson | yes, four labelled lines | best | yes |
| Easy to get wrong by hand | indentation matters; caught on every keystroke by the editor, avoided by the form | no indentation; but word order and connective words are a grammar to remember | least |
| Error messages | by key, with the existing validator | need a new parser to be good | by line |
| Names with spaces, commas, or `#` (S3 prefixes allow all three) | YAML quoting, one known rule | our own quoting rules | our own escaping for lists |
| Room for `burst`, `respond`, `note` | add a key | every option lengthens the sentence and the grammar | add a key |
| One file, one format | yes | yes | no: either two formats in one product, or the whole config converted, against the specification's "One YAML file" |
| New code that is a public contract | a few renamed keys and two small value parsers (`20/s`, scalar-or-list) | a sentence grammar and its fuzz target | an INI parser and its fuzz target, plus every config test and doc |

**Why A.** B's advantage is that it reads as a sentence; section 2 gives that without making
operators write a grammar, because the tool prints the sentence for whatever they wrote. C's
advantage is freedom from indentation; the editor's live validation and the form remove most of
that risk at a fraction of C's cost, and C breaks a fixed decision for a benefit the form already
delivers. A is also the only candidate whose fields map one to one onto the form, so a person who
used the form recognizes the file, and the reverse.

**D. Keep the schema; only switch the examples to operation names.** Possible today, since `ops`
already accepts them, and it answers "there's no WRITE in S3" at no cost. It leaves `group_by`,
the nested maps, the silent shared default, and the missing feedback, which were the other three
problems.

Prior art, from memory of the consoles and not re-checked for this ADR: Cloudflare's rate-limiting
rule builder and AWS WAF's rate-based rules both ask the same four questions in the same order
(which requests match, which characteristic is counted separately, what rate, what action).

## Phasing

Each step is shippable on its own; sizes are estimates, not measurements.

1. **Vocabulary and shape** (config only): the new keys, `N/s`, required `per`, operation names
   with trailing `*` and did-you-mean, the sentence renderer, `check-config` and `rules list`
   printing it; `docs/CONFIG.md`, the example and demo configs, the specification's schema block.
   Roughly 400 lines of code and tests; every rule fixture in the tests is rewritten.
2. **The console**: the sentence on the Limits screen and under the editor's cursor; requests by
   operation from `/metrics`; the form with pickers; surgical saves. Roughly 700 lines with golden
   frames.
3. **Matched counter** and the "matches nothing" flag: one metric (catalog first), one atomic add
   per matching rule, a benchmark to show the decision path stays inside its budget.

Not in this ADR, worth its own: an **observe-only** rule (counts what it would throttle without
throttling), the safest way to try a limit on production traffic; and a read-only admin route over
the in-memory showback table for live per-key and per-bucket rates ("the busiest access key right
now"), which the form's "Now" line and pickers would use.

## Consequences

- Specification: the Rate limiting section's YAML block and its "three parts" paragraph are
  rewritten around the four questions; `rate_limits.default_on_limit` goes (each rule says its own;
  the default is 503); the Operator surface's Limits row gains `n`, `enter`, `d`, `l`. The
  semantics list (keys, evaluation, match cost, rate 0) stands, with `per` for `group_by`.
- `docs/CONFIG.md`, `deploy/tollbooth.example.yaml`, `demo/`, `docs/OPERATIONS.md` §7, the CLI and
  TUI goldens, and the admin API's rule fields (`/admin/limits` returns the sentence and, in step 3,
  `matched`) follow.
- Nothing is released, so there is no compatibility shim. A config in the old shape fails
  `check-config` with errors that name the new keys.
- The fuzz target for the config parser covers the two new value parsers.

## Questions for Blake

1. Format: A (plain YAML keys), B (sentence), or C (key = value)? **Recommended: A.**
2. `per` required, with `per: total` for a shared budget? **Recommended: yes.**
3. Drop the class words (`READ`, `WRITE`, `LIST`, …) from the file entirely, with `Get*`-style
   families instead? **Recommended: yes**; they stay as internal grouping only.
4. Build order 1, 2, 3 as above, in Phase 5 before the v0.1.0 tag? **Recommended: step 1 now**
   (it is the vocabulary the demo asked for and it changes a public contract, so it should land
   before anything is tagged); steps 2 and 3 on your word.
5. Observe-only rules and live per-key rates as their own ADRs later? **Recommended: yes, later.**
