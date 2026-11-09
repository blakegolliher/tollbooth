# CLAUDE.md — rules for working in this repo

Read docs/SPEC.md (the development prompt; its "Decisions already made" are fixed) and docs/DESIGN.md before every task. docs/STATUS.md says which phase we are in and what its gate is. docs/DECISIONS.md records every ambiguity resolved without an ADR.

## Roles and gates

- Work phase by phase (docs/SPEC.md, Phased delivery plan). At each gate write `docs/reports/gate-N.md` in the prescribed format and stop. Do not begin the next phase until told to.
- A decision in "Decisions already made" is challenged by an ADR with the alternative and the evidence, never by a different implementation.
- Ambiguity: cheap to redo → take the simplest reading that satisfies the text, record it in docs/DECISIONS.md, continue. Expensive (storage schema, wire behavior, public config keys) → ask first.
- Never weaken, skip, or mark-as-flaky a test to pass a gate. A failing test is a finding.
- Report numbers, not adjectives.

## Simplicity rules (from shunt's CLAUDE.md, verbatim where they apply)

- One binary, `tollbooth`, with subcommands `serve`, `status`, `rules`, `config`, `backends`, `usage`, `check-config`, `doctor`, `tui`, `version`. The TUI is a subcommand, not a second binary (docs/SPEC.md, Operator surface; ADR-0003).
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

## Additional rules

- Transparency first, then availability, then latency, then accuracy, then features (docs/SPEC.md, Mission and scope). When two conflict the higher one wins.
- The proxy adds no headers to requests or responses; the data listener's handler is the proxy handler itself, never an `http.ServeMux`; the request-target comes from `r.RequestURI` and travels in `URL.Opaque` (docs/DESIGN.md §4).
- Fail open, always. There is no fail-closed knob. The recover guard re-panics `http.ErrAbortHandler`.
- Object keys and access keys never appear in logs above `debug`, in metric labels, or in client-facing error bodies beyond the `<Resource>` element S3 itself returns.
- Rule identifiers never appear in client-facing responses.
- Commits follow Conventional Commits, are small and single-purpose, and carry **no AI attribution trailers of any kind** (no Co-Authored-By, no Generated-with, no session trailer). Check `git log -1 --format=%B` after every commit.

## Lift procedure (copying code in; docs/reuse.md)

1. Copy the file into the target package and keep the original license header verbatim.
2. Directly under the header add `// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, <date>: <one line on what changed>.` Apache-2.0 §4(b) requires the modification notice.
3. Append the source project's `LICENSE` (and `NOTICE`, if one exists at the commit copied) to `THIRD_PARTY_NOTICES.md`.
4. Add a line to `docs/deps.md`: source repo, path, commit SHA, license, why the stdlib was not enough.
5. The G4 audit at every gate checks all of the above.

Never copy code from MinIO, Garage, or warp (AGPL-3.0) or elbencho (GPL-3.0). Running them as external processes is fine.
