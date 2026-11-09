# Gate 0 report — Design

Date: 2026-11-08. Branch `phase-0` off `main` (`6e2649a`, the specification). No product code was
written; `cmd/tollbooth/main.go` is an empty `main` so the build and vet targets run (D-013).

## 1. Scope delivered

| Planned item (docs/SPEC.md, Phase 0) | Status | Commit |
| --- | --- | --- |
| `docs/DESIGN.md`: architecture, data structures, hot-path walkthrough with allocation budget | done | `e76193d` |
| One ADR per open decision: ADR-0001 (prefix rules and list operations), ADR-0002 (bench load generator) | done, status *proposed*, recommendations (b) and (c) | `90c174c` |
| ADR for the TUI-in-one-binary decision (requested in Operator surface) | done, *accepted* | `46bc98f` |
| shunt's op table checked against the API reference | done: 95 → 113 entries, 18 added, 6 folded or excluded with reasons, reference fetch date recorded | `ff135d0` |
| Repo scaffold (module, layout, LICENSE, NOTICE, README, package docs) | done | `b8d7fa0` |
| `Makefile` with `build`, `lint`, `test`, `race`, `fuzz`, `bench`, `compat`, `release` (+ `tools`, `tidy`, `vuln`, `licenses`, `bench-compare`) | done; `compat` and `release` are defined and fail until their phase supplies inputs | `6c77c73` |
| CI skeleton: PR workflow (lint, unit+race, 60 s fuzz, govulncheck, go-licenses, CodeQL, Semgrep, integration when compose exists) and nightly (1 h fuzz, bench regression) | done, not yet executed on GitHub (no remote) | `e7f8a63` |
| `.golangci.yml` with the `depguard` rules (charmbracelet only in `cmd/tollbooth/tui`; no `gopkg.in/yaml`; hdrhistogram only in `bench/`) | done | `58076fa` |
| `THIRD_PARTY_NOTICES.md` | done: shunt LICENSE and NOTICE carried; generated section empty (nothing linked) | `660ab75` |
| `docs/CONFIG.md` with the full schema and defaults; `deploy/tollbooth.example.yaml` | done | `e5d81ae` |
| `CLAUDE.md` with the verbatim rules | done | `b476efc` |
| `docs/SHOWBACK_SCHEMA.md` (storage schema is expensive to change, so fixed now) | done | `3194b9d` |
| `docs/telemetry-catalog.md` (every metric before implementation) | done | `6ad5e21` |
| `docs/deps.md`, `docs/reuse.md` | done | `88a958b` |
| `docs/OPERATOR_SURFACE.md` parity matrix | done | `455b5bb` |
| `docs/DECISIONS.md` (20 entries), `docs/STATUS.md` | done | `59bfc44` |
| `docs/OPERATIONS.md`, `docs/COMPAT.md`, `CHANGELOG.md` | deferred to Phases 5, 1, 5 as the specification schedules them | — |

Size: 36 files, 2,557 lines added over `main`; 2,502 of them documentation and configuration.

## 2. Decisions and assumptions

ADRs written this phase: [ADR-0001](../adr/0001-prefix-rules-and-list-operations.md) (proposed),
[ADR-0002](../adr/0002-bench-load-generator.md) (proposed), [ADR-0003](../adr/0003-tui-in-one-binary.md) (accepted).

`docs/DECISIONS.md` entries added: D-001 through D-020. The ones that touch public config keys,
wire behavior, or the storage schema are repeated in section 6 as questions: D-002 (config key
placement), D-004 (`//` request-target), D-007 (map value type), D-010 (showback header fields),
D-003 (`Preflight` class).

Assumptions the design rests on, each verified this phase:

- Go 1.27.3 is the current stable release (go.dev `VERSION` endpoint, 2026-11-08) and
  `GOTOOLCHAIN=auto` downloads it on the dev box (verified: `go version` → `go1.27.3`).
- `hash/maphash.Comparable` exists (Go ≥ 1.24) and hashes struct keys with string fields by
  content without allocating; the limiter and showback shard selection depends on it.
- `URL.Opaque` is the documented way to send a verbatim request-target through `http.Transport`;
  its one limitation (a target starting with `//`) is handled by D-004.
- shunt at `9b36d09` has `LICENSE` and `NOTICE`; its Go files carry no per-file header, so the
  lift adds the short Apache header naming shunt (docs/reuse.md).
- The AWS operations page lists 116 operations and carries no version date; the fetch date is the
  version. Request syntax for the 18 added operations was read from each operation's page.

## 3. Test results

No Go tests exist (no product code). What ran, with full output in `run/gate0-validation.log` and
`run/gate0-tools.log` (the `run/` directory is git-ignored; the logs are reproduced in section 7):

| Command | Result |
| --- | --- |
| `GOTOOLCHAIN=auto go version` | `go1.27.3 linux/amd64` |
| `gofmt -l .` | no output (clean) |
| `go vet ./...` | exit 0 |
| `go build ./...` and `make build` | exit 0; `bin/tollbooth` produced; `go version -m` check for `gopkg.in/yaml.v3` passed (nothing linked) |
| `make test` | exit 0; 8 packages, `[no test files]` each |
| `make tools` (golangci-lint v2.15.0, go-licenses v2.0.1, govulncheck v1.9.0) | see section 3.1 |
| `make lint` | see section 3.1 |
| `make vuln` | see section 3.1 |
| `make licenses` | see section 3.1 |
| `bash -n scripts/third-party-notices.sh` | ok |
| YAML parse of `deploy/tollbooth.example.yaml`, both workflows, `.golangci.yml` | all parse; example config has the 13 expected top-level keys |

Test plan categories 1–12: all **skipped — no code in Phase 0**, each scheduled in
`docs/DESIGN.md` §12. Coverage: n/a. Race: n/a. Fuzz: n/a. `docs/COMPAT.md`: Phase 1.

### 3.1 Tool runs

Full log: `run/gate0-tools.log`. Summary lines, verbatim:

```
GOBIN=.../bin go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
GOBIN=.../bin go install github.com/google/go-licenses/v2@v2.0.1
GOBIN=.../bin go install golang.org/x/vuln/cmd/govulncheck@v1.9.0
tools rc=0
== make lint
0 issues.
go vet ./...
lint rc=0
== make vuln
No vulnerabilities found.
vuln rc=0
== make licenses
third-party-notices: regenerated THIRD_PARTY_NOTICES.md
licenses rc=0
```

`make licenses` ran `go-licenses check` with the copyleft deny list against `./cmd/tollbooth`
(nothing linked, nothing denied) and regenerated the empty "Linked modules" table. Its first run
appended one empty table row (an `awk` over empty input); the script was fixed to skip empty lines
and rerun, after which `git diff THIRD_PARTY_NOTICES.md` is empty, which is what the CI job
`notices are committed` asserts.

## 4. Performance

Not applicable in Phase 0 (specification: Phases 2 to 5). The targets are restated with their
measurement plan in `docs/DESIGN.md` §9 (allocation budget: ≤ 7 small allocations per forwarded
request outside `net/http`, 0 in the limiter decision and showback increment) and §9.4 (memory
model with per-key and per-tuple estimates to be measured at Gates 2 and 3).

## 5. Known gaps and risks

| Gap or risk | Proposed fix | Phase |
| --- | --- | --- |
| The dev box has no Docker; `podman` availability is unverified. The integration suite and `make compat` need a compose runtime. | `make compat` detects `docker compose`, `podman compose`, `podman-compose`; verify `podman` on the box before Phase 1 or run integration in CI only. | 1 |
| CI workflows have never executed (no GitHub remote yet). Action versions and the Semgrep container invocation are from memory of current practice. | Push the branch and fix on the first run; Phase 1 gate includes a green CI link. | 1 |
| `golang.org/x/perf` (benchstat) and `charmbracelet/x/exp/teatest` are untagged upstream; the Makefile uses `@latest` for benchstat and `docs/deps.md` records a pseudo-version for teatest. | Pin benchstat to a pseudo-version when `make bench-compare` first runs (Gate 2). | 2 |
| The copy-buffer memory row (`max_inflight × copy_buffer_bytes` worst case) can exceed the 1 GiB target under a flood of concurrent large bodies. | Measured in the Phase 5 soak; `listen.max_inflight` and `proxy.copy_buffer_bytes` are the knobs; sizing guidance in `docs/OPERATIONS.md`. | 5 |
| `ListDirectoryBuckets` is indistinguishable from `ListBuckets` without `x-id` or the host, so it is folded (D-009). An S3 Express deployment behind tollbooth would see them as one operation. | Acceptable in v1 (same class, same limits); revisit if S3 Express directory buckets become a target. | 6 |
| `PostObject`'s AWS page could not be fetched (returned no content). Its shape is taken from shunt's table (`POST` on a bucket with no subresource). | Verify against the compatibility matrix (browser-form upload case) in Phase 1. | 1 |
| The `prefix` limiter matcher is a linear scan over a rule's configured prefixes. | Fine for tens of prefixes; `check-config` warns past 64; a sorted structure is a Phase 2 optimization only if the 10 µs budget is missed. | 2 |
| `trusted_proxies` CIDR match is a linear `Contains` over the list. | Same: linear is within budget for the handful of load-balancer ranges this is for. | 2 |

## 6. Questions

Each with the recommended answer, so a reply can be "1 yes, 2 yes, 3 a".

1. **ADR-0001**: adopt (b), prefix rules also match the four bucket-level list operations by their `prefix` parameter? Recommended: **yes**.
2. **ADR-0002**: adopt (c), in-repo Go generator with warp as an external cross-check? Recommended: **yes**.
3. **Limiter map value** (D-007): (a) `map[key]int64` as designed, or (b) `map[key]*int64` as the specification's text says? Recommended: **(a)**; same lock, one fewer allocation per new key.
4. **Schema and key additions** (D-002, D-010): accept the showback header fields `schema`, `bytes_in`, `bytes_out`, `recovered`, `written_at` beyond the five the specification names, and the config keys `base_domains` (top-level), `kill_switches.limiter_disable`, `kill_switches.showback_disable`, `admin.token` (inline, redacted)? Recommended: **yes** to all; say which to drop otherwise.
5. **`Preflight` class** (D-003): `BUCKET` as designed, or `UNKNOWN`? Recommended: **`BUCKET`**.
6. **`//` request-target** (D-004): answer `400 InvalidURI` and count `proxy_error`? Recommended: **yes**; the alternative (forwarding in absolute-form) is not byte-identical.
7. **Toolchain** (D-014): pin `go 1.27.3` with `GOTOOLCHAIN=auto`? Recommended: **yes**.
8. **Phase 1 may begin** on approval of 1–7, on branch `phase-1`, starting with the shunt lift in the order of `docs/reuse.md`? Recommended: **yes**.

## 7. Reproduce

From a clean checkout of branch `phase-0`:

```
git log --oneline main..HEAD                     # the 17 commits in section 1
git log --format=%B main..HEAD | grep -i -E 'co-authored|generated-with|session'   # expect no output
GOTOOLCHAIN=auto go version                      # go1.27.3
gofmt -l .                                       # expect no output
go vet ./...
make build
make test
make tools && make lint && make vuln && make licenses
bash -n scripts/third-party-notices.sh
python3 -c "import yaml; print(sorted(yaml.safe_load(open('deploy/tollbooth.example.yaml'))))"
git diff --stat main..HEAD | tail -1             # 36 files changed, 2557 insertions
```

The op-table check: `docs/OPERATIONS_TABLE.md` against the operations page fetched 2026-11-08
(116 operations) and the per-operation "Request Syntax" sections for the 18 additions; the shunt
side is `internal/s3/op.go` and `classify.go` at `9b36d09` (`git clone https://github.com/blakegolliher/shunt && git checkout 9b36d09`).

## Guardrail passes

- **G1 Simplicity**: no exported Go identifiers added (package `doc.go` files and an empty `main`
  only). No dependency added to `go.mod`. No body buffering anywhere. Feature flags: none; kill
  switches: two, both default `false`, each documents what breaks (docs/CONFIG.md). LOC added
  2,557, deleted 0, test-to-code ratio n/a.
- **G2 Validation**: section 3. Nothing to fuzz, diff, or benchmark yet.
- **G3 Telemetry and docs**: every metric the specification names is in `docs/telemetry-catalog.md`
  with labels and buckets before any implementation; `showback_overflow_total` and
  `showback_collapsed_total` reconciled to one name (D-015). `docs/CONFIG.md` covers every key.
  Nothing in this phase contradicts the specification except the items raised as questions above;
  none was implemented differently from the text.
- **G4 Dependency and license audit**: nothing linked. Planned modules checked for license files
  on 2026-11-08 (`go mod download`): `go.yaml.in/yaml/v4` Apache-2.0 + NOTICE; `client_golang`
  Apache-2.0 + NOTICE; `go-proxyproto` Apache-2.0; bubbletea, bubbles, lipgloss MIT;
  `hdrhistogram-go` MIT. shunt's LICENSE and NOTICE are carried in `THIRD_PARTY_NOTICES.md`. No
  code from MinIO, Garage, or warp. YAML is `go.yaml.in/yaml`; the `make build` check and the
  depguard rule enforce it from Phase 1.

## What I would change in the specification

- Name the metric once: the text uses both `tollbooth_showback_overflow_total` and
  `showback_collapsed_total` for the same counter.
- State the YAML module with its major (`go.yaml.in/yaml/v4`) now that v4 is GA.
- The limiter paragraph says `map[key]*int64`; if question 3 lands on (a), change it to
  `map[key]int64` so the text and the code agree.
- The classifier section says the table "is shunt's `internal/s3/op.go`"; the table is in
  `classify.go` and the enum in `op.go`. Cosmetic.
- Give the `Preflight` operation a class in the classifier table, or say it is `UNKNOWN`.
- The request-identity table could say what happens to a request-target beginning with `//`
  (D-004), since it is the one case `URL.Opaque` cannot carry.
