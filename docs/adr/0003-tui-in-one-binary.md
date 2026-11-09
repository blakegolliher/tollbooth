# ADR-0003: The TUI ships in `bin/tollbooth`, and Bubble Tea is imported only from `cmd/tollbooth/tui`

Status: **accepted** (Phase 0, 2026-11-08). Records the decision in docs/SPEC.md, Operator surface.
Same reasoning as shunt ADR-0009 (a large dependency kept in the binary but off the data path).

## Context

tollbooth has no web UI. Operators get a CLI and a terminal UI with structural parity: both go
through one admin API client. The TUI framework (`charmbracelet/bubbletea`, `bubbles`, `lipgloss`,
all MIT) is the largest dependency the binary will carry that has nothing to do with proxying.
The choice is between a second binary (`tollbooth-tui`) and a subcommand of the one binary with
the import fenced.

## Decision

One binary. `tollbooth tui` is a subcommand. The charmbracelet packages are imported only from
`cmd/tollbooth/tui`; a `depguard` rule in `.golangci.yml` fails the build on an import anywhere
else, and the test plan (category 12) asserts it. The TUI is read-only, polls the admin API, and
touches nothing on the data path: `serve` never constructs a Bubble Tea program, so the framework
is dead code in a serving process beyond its contribution to binary size.

## Consequences

- Binary size grows by the framework (measured at Gate 4 and recorded in `docs/STATUS.md`).
  Acceptable: one artifact to ship, one version to check, one `systemd` unit, one container image.
- `govulncheck` reports framework advisories against the shipped binary; they are triaged like any
  other dependency's.
- `THIRD_PARTY_NOTICES.md` carries the three MIT notices; `docs/deps.md` has a line for each.
- `teatest` (`charmbracelet/x/exp/teatest`, MIT) is a test-only dependency for golden frames; it is
  not tagged upstream, so it is pinned to a pseudo-version and that is noted in `docs/deps.md`.
- If the field ever needs the TUI on a host without the proxy, the subcommand runs anywhere the
  binary does; a separate binary is not needed for that.
