# ADR-0006: The TUI as the operator console: performance graphs and a config editor that saves

Status: **accepted** (Blake, 2026-11-09, by merging #27 after the question was put to him in the
PR). Written on Blake's direction: "it needs to be where we can configure, and get a visual cue as
to how the service is performing … graphs like nfsview … inline editor and syntax checker like the
bucket policy tool". This ADR challenged one fixed decision; the specification's sentence is to be
amended as proposed below (Fable's edit).

## Context

The specification (Operator surface) fixes two things about `tollbooth tui`: it is read-only, and
"neither the CLI nor the TUI writes the config file. Rules change by editing the file and running
`tollbooth config reload`"; rule editing from the CLI/TUI is a Phase 6 backlog item. The reasons
given: shunt's rule that a config package never rewrites the operator's file, and avoiding atomic
rewrites, comment preservation, and a race with a human editing the same file.

Blake's two references:

- **nfsview** (Rust, ratatui): tabs of braille line charts over a rolling history (120 samples by
  default), several series per chart (avg, p90, p95, p99) with the current value in the title,
  `space` pause, `r` reset, `p` cycles the series mode; per-op latency histograms as sparklines.
- **vast-bucket-policy-manager** (Go, Bubble Tea, the same three charm libraries tollbooth uses):
  a `textarea` editor with NORMAL and INSERT modes, live validation (JSON syntax with line:col,
  then structural checks) shown under the editor, `e` for `$EDITOR`, a save/discard prompt when
  leaving a dirty buffer, and a timestamped backup written before every save.

What the TUI has today (Phase 4): five screens, req/s and throttled/s sparklines over five minutes
of polls, a dry-run request form, a read-only diff of the file against the running config, `r`
reload and `v` validate.

## Decision

**1. A Graphs screen (`6`)**, read-only, no conflict with the specification. Braille line charts
drawn by about 150 lines of our own code (no new dependency: "no second TUI library", and a
charting library would add a dependency the stdlib covers): request and throttle rates, in-flight,
backend TTFB per endpoint, denied per second per rule, and the process's heap and goroutines
scraped from `/metrics` on the same poll. History is the last N polls (`--history`, default 300,
ten minutes at the 2 s interval); `space` pauses, `r` resets the history (the reload key moves to
`R` on this screen only). Values appear in each chart's title, like nfsview.

**2. An editor on the Config screen**, which is where the fixed decision is challenged. `e` opens
the file on disk in a `textarea` (raw text, so comments and ordering are the operator's own);
INSERT and NORMAL modes; on every change the buffer is validated with the same code as
`check-config` (`config.Parse`: YAML syntax with line and column, unknown keys, bounds) plus the
restart-only check against the running config (D-046), and the findings are shown under the
editor; the running-config diff updates from the buffer; `E` opens `$EDITOR` on a temporary copy
and reads it back; `esc` from a dirty buffer asks save/discard.

**Saving.** The TUI writes the config file **only** on an explicit `ctrl+s`, and only when the
buffer validates. It writes the operator's text verbatim to a temporary file beside the target and
renames it over (atomic on the same filesystem), after writing `<file>.<UTC timestamp>.bak` with
the previous content; it refuses when the file's modification time changed since the buffer was
loaded ("changed on disk; reload the buffer with `L`"), which is the human-editor race the
specification worried about. After a save the operator presses `r` to reload, or `ctrl+r` to save
and reload in one step. The CLI still never writes the file; nothing writes it without a keypress.

This asks Blake to amend the specification sentence to: "The CLI never writes the config file. The
TUI writes it only on an explicit save from its editor, after the buffer validates, atomically, with
a timestamped backup beside it, and never when the file changed on disk since it was loaded."

## Alternatives considered

- **Keep the TUI read-only and shell out to `$EDITOR` only.** No conflict, but the operator loses
  the live validation that is the point of the request; `$EDITOR` is kept as an option (`E`).
- **Edit a draft file and never the live path.** Satisfies the letter of the rule; in practice the
  operator copies the draft over the live file by hand, with none of the safeguards above.
- **Apply edits through the admin API without a file.** The running process would diverge from the
  file and a restart would lose the change; rejected outright (the file is the source of truth).
- **A charting library** (charmbracelet's `ntcharts`, MIT). Rejected: one more dependency for a
  braille rasterizer and axis labels the stdlib writes in an afternoon; `docs/deps.md` would owe a
  line it cannot honestly fill.

## Consequences

- Specification: the "neither the CLI nor the TUI writes the config file" sentence is amended as
  above, the TUI screen table gains Graphs (`6`) and the Config row gains the editor keys, and the
  "rule editing from the CLI/TUI" backlog item is partly delivered (the TUI; the CLI stays
  read-only). `docs/OPERATOR_SURFACE.md` and `docs/DESIGN.md` §8 follow.
- Tests: golden frames for Graphs at both sizes and for the editor in NORMAL, INSERT, invalid,
  dirty-prompt, and saved states; a unit test for the chart rasterizer; a test that a save writes
  the backup, renames atomically, and refuses a changed file.
- The TUI stays off the data path: one more admin GET (`/metrics`) per poll, read by the same
  client, no new route and no new metric.
