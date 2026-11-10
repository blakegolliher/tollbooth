# Operator surface: parity matrix

Every admin route has a CLI verb and a TUI screen; a test over the admin route table fails when a
registered route is missing from this file (test plan category 12). Both the CLI and the TUI go
through `internal/admin`'s client, so parity is structural. The CLI never writes the config file;
the TUI writes it only on an explicit save, from the Limits form (ADR-0007) or the Config editor
(ADR-0006).

Phase 4 state: every route in the table is served (`internal/admin`), and every CLI verb exists:
`serve`, `check-config`, `version`, `status`, `rules list|show|test|top`, `config show|reload|diff`,
`backends list`, `usage query|top|verify`, `doctor`, `tui`. `TestOperatorSurfaceParity` reads this
file and fails when a route in `admin.Routes` has no row, no CLI verb, or no TUI screen (the two
exemptions below are marked `—` on purpose).

| Admin route | CLI verb | TUI screen (key) | Phase |
| --- | --- | --- | --- |
| `GET /healthz` | `status` (part of the summary) | Overview (`1`) | 1 |
| `GET /readyz` | `status` | Overview (`1`) | 1 |
| `GET /metrics` | — (Prometheus scrapes it; `status` shows the derived req/s and throttled/s) | Overview (`1`) sparklines; Graphs (`6`) heap, RSS, goroutines; Limits (`2`) requests per second by S3 operation | 1, 5 |
| `POST /admin/reload` | `config reload` | Overview (`1`) and Config (`5`), key `r`; `ctrl+r` in the Limits form and in the editor saves and reloads | 2, 5 |
| `GET /admin/config` | `config show`, `config diff` | Config (`5`); `e` opens the editor (ADR-0006) | 2, 5 |
| `GET /admin/limits` | `rules list`, `rules show <name>` (each rule with its sentence, matched, denied) | Limits (`2`): the rules in words with matched/s and throttled/s; `n` new, `enter` change, `d` delete open the guided form (ADR-0007) | 2, 5 |
| `GET /admin/limits/top?n=` | `rules top [--n] [--window]` | Limits (`2`), under the selected rule | 2 |
| `POST /admin/limits/test` | `rules test --access-key --bucket --key --ip --op` | Limits (`2`), key `t` | 2 (D-039) |
| `GET /admin/usage?from=&to=&group_by=&where=&format=` | `usage query`, `usage top` (files or this node; same code path) | Usage (`3`); the Limits form's access-key and bucket pickers (the last two hours) | 3, 5 |
| `GET /admin/status` | `status` | Overview (`1`); Graphs (`6`) rates and in-flight | 4 |
| `GET /admin/backends` | `backends list` | Backends (`4`); Graphs (`6`) TTFB per endpoint | 4 |
| `GET /debug/pprof/*` (only with `admin.pprof: true`) | — (operator uses `go tool pprof`; documented in OPERATIONS.md) | — (exempt: diagnostic, not an operator function) | 1 |

Local verbs with no route (they read files or the environment, not the running proxy):
`check-config`, `doctor`, `usage verify`, `version`, `serve`, `tui`. The TUI's Config screen key
`v` runs `check-config` on the file on disk; its editor (`e`) runs the same check on every keystroke.
The editor and the Limits form are the only places in tollbooth that write the config file: on
`ctrl+s` or `ctrl+r` (and on a confirmed delete in the form), after the result validates, atomically,
with a `<file>.<timestamp>.bak` beside it, refused when the file changed on disk since it was
loaded. The form changes only the rule it was opened on: a changed value is patched in place, a new
rule is appended as a block, a deleted rule's lines are removed, and every other byte of the file,
comments included, stays as it was (`internal/config/edit.go`; the result is re-parsed and must
equal the intended rules, or nothing is written).

## CLI conventions

- `--format table|json|csv` on every verb; `json` is the admin API response unwrapped.
- Admin endpoint and token: `--admin`, `--token`; then `TOLLBOOTH_ADMIN`, `TOLLBOOTH_TOKEN`; then
  the config file (`--config`).
- Failures exit non-zero with one line on stderr.
- Golden tests for every verb in all three formats (Phase 4).

## TUI conventions

- Polls every 2 s (`--interval`); 80×24 minimum; keyboard only; honors `NO_COLOR` and `TERM=dumb`;
  shows the last state under a staleness banner when the admin API is unreachable. Read-only except
  the explicit saves of the Limits form (ADR-0007) and the Config editor (ADR-0006), both of which
  need `--config`.
- `?` help on every screen; `q` quits; `1`–`6` switch screens, as do `tab` and `→` (next) and
  `shift+tab` and `←` (previous) when no text field or the editor owns the keys. Graphs (`6`): braille line charts
  over the last `--history` polls (300); `space` pauses, `r` resets.
- Limits (`2`): `↑`/`↓` select a rule; `n` new, `enter` change, `d` delete (asks `y`); `t` tests a
  request; `/` filters. In the form `tab`/`shift+tab` move between fields, the arrows pick a
  suggestion or a choice, `enter` accepts, `ctrl+r` saves and applies, `ctrl+s` saves only, `esc`
  cancels (and asks before discarding changes).
- `teatest` golden frames at 80×24 and 120×40, including the admin-unreachable state (Phase 4), the
  Limits screen with live rates and a rule that matches nothing, and ten states of the form.
