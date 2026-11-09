# ADR-0001: What a prefix rule matches for list operations

Status: **proposed** (Phase 0, 2026-11-08). Open decision 1 in docs/SPEC.md. Awaiting Fable and Blake.

## Context

A rule may carry `match.prefixes` and may `group_by: [prefix]`; showback records a `prefix` column.
For object operations the prefix is matched against the object key, longest configured prefix
first. `ListObjects`, `ListObjectsV2`, `ListObjectVersions`, and `ListMultipartUploads` have no
key; they carry an optional `prefix` query parameter that scopes the listing. The question is
whether a rule on `ckpt/` sees a list storm on `ckpt/`.

The field case that motivates this: a training job's checkpoint writer lists `ckpt/` every few
seconds from hundreds of ranks. The operator wants "LIST on `ckpt/` ≤ 50/s per access key" and
wants the showback row for `ckpt/` to include those lists so the chargeback for that prefix is
honest.

## Options

**(a) Object operations only, by key.** A prefix rule never matches a list operation; lists match
only bucket-level rules. Showback records list operations under `prefix: ""`.

- For: one definition of "prefix" (a key prefix), nothing to parse from the query string.
- Against: the list storm on `ckpt/` is invisible to a `ckpt/` rule and lands in the bucket root's
  showback row, which is wrong for chargeback by prefix. An operator who wants to limit it must
  write a bucket-wide LIST rule that also throttles unrelated listings.

**(b) Also list operations, by their `prefix` query parameter.** For the four bucket-level list
operations, the request's `prefix` parameter (percent-decoded once) is matched against the rule's
configured prefixes exactly as a key would be: the longest configured prefix that is a prefix of
the request's prefix wins. A list with no `prefix` parameter, or one shorter than every configured
prefix (`prefix=ck` against a rule on `ckpt/`), matches no prefix rule, because that listing is
wider than the rule's scope. Showback uses the same resolution for its `prefix` column. `ListParts`
is keyed by an object key and is matched by key like any object operation.

- For: the storm on `ckpt/` counts against `ckpt/` in both the limiter and showback; the
  semantics are "a prefix rule covers the traffic that is confined to that prefix", which is what
  an operator means.
- Against: a `prefix=` parameter is a hint the client controls; a client can evade a prefix LIST
  rule by listing the bucket root with a `delimiter` and filtering client-side. That evasion costs
  the client more work and still hits any bucket-level rule, so it is not a security boundary,
  and the rule set never claimed one.

The decoded `prefix` parameter is one extra substring the identity pass already walks past (the
query scanner records the value's byte range; decoding happens only when the value contains `%`),
so (b) costs nothing on requests that are not lists.

## Recommendation

**(b).** It is Fable's starting position and the field case decides it. The cost is one
documented rule in `docs/CONFIG.md` (`match.prefixes` row) and `docs/SHOWBACK_SCHEMA.md`
(`prefix` column), both already written assuming (b).

## Consequences

- `internal/s3id` exposes the list `prefix` parameter on `RequestIdentity` (field `ListPrefix`,
  empty for non-list operations), decoded once.
- The limiter's per-bucket prefix matcher takes `(key or ListPrefix)` and is otherwise unchanged.
- Test plan category 3 gains cases: `ListObjectsV2?prefix=ckpt/a` matches a `ckpt/` rule;
  `?prefix=ck` does not; no `prefix` does not; `ListParts` on `ckpt/x` matches by key.
- Category 8 (showback) gains the mirror cases for the `prefix` column.
- If (a) is chosen instead: delete the `ListPrefix` field and the four test cases; the matcher and
  the showback column are unchanged. The design is written so the switch is a one-line change in
  the identity pass.
