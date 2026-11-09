# Operator surface: parity matrix

Every admin route has a CLI verb and a TUI screen; a test over the admin route table fails when a
registered route is missing from this file (test plan category 12). Both the CLI and the TUI go
through `internal/admin`'s client, so parity is structural. Neither writes the config file.

Phase 4 state: every route in the table is served (`internal/admin`), and every CLI verb exists:
`serve`, `check-config`, `version`, `status`, `rules list|show|test|top`, `config show|reload|diff`,
`backends list`, `usage query|top|verify`, `doctor`, `tui`. `TestOperatorSurfaceParity` reads this
file and fails when a route in `admin.Routes` has no row, no CLI verb, or no TUI screen (the two
exemptions below are marked `—` on purpose).

| Admin route | CLI verb | TUI screen (key) | Phase |
| --- | --- | --- | --- |
| `GET /healthz` | `status` (part of the summary) | Overview (`1`) | 1 |
| `GET /readyz` | `status` | Overview (`1`) | 1 |
| `GET /metrics` | — (Prometheus scrapes it; `status` shows the derived req/s and throttled/s) | Overview (`1`) sparklines | 1 |
| `POST /admin/reload` | `config reload` | Overview (`1`) and Config (`5`), key `r` | 2 |
| `GET /admin/config` | `config show`, `config diff` | Config (`5`) | 2 |
| `GET /admin/limits` | `rules list`, `rules show <id>` | Limits (`2`) | 2 |
| `GET /admin/limits/top?n=` | `rules top [--n] [--window]` | Limits (`2`) detail pane | 2 |
| `POST /admin/limits/test` | `rules test --access-key --bucket --key --ip --op` | Limits (`2`), key `t` | 2 (D-039) |
| `GET /admin/usage?from=&to=&group_by=&where=&format=` | `usage query`, `usage top` (files or this node; same code path) | Usage (`3`) | 3 |
| `GET /admin/status` | `status` | Overview (`1`) | 4 |
| `GET /admin/backends` | `backends list` | Backends (`4`) | 4 |
| `GET /debug/pprof/*` (only with `admin.pprof: true`) | — (operator uses `go tool pprof`; documented in OPERATIONS.md) | — (exempt: diagnostic, not an operator function) | 1 |

Local verbs with no route (they read files or the environment, not the running proxy):
`check-config`, `doctor`, `usage verify`, `version`, `serve`, `tui`. The TUI's Config screen key
`v` runs `check-config` on the file on disk.

## CLI conventions

- `--format table|json|csv` on every verb; `json` is the admin API response unwrapped.
- Admin endpoint and token: `--admin`, `--token`; then `TOLLBOOTH_ADMIN`, `TOLLBOOTH_TOKEN`; then
  the config file (`--config`).
- Failures exit non-zero with one line on stderr.
- Golden tests for every verb in all three formats (Phase 4).

## TUI conventions

- Read-only; polls every 2 s (`--interval`); 80×24 minimum; keyboard only; honors `NO_COLOR` and
  `TERM=dumb`; shows the last state under a staleness banner when the admin API is unreachable.
- `?` help on every screen; `q` quits; `1`–`5` switch screens.
- `teatest` golden frames at 80×24 and 120×40, including the admin-unreachable state (Phase 4).
