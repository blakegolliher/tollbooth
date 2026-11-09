# Gate 4 report: Phase 4, Operator surface

Written 2026-11-09 on branch `phase-4` (PRs #20 `p4/admin` and #21 `p4/tui`, stacked). Dev box:
RHEL 9, 2 vCPUs, Go 1.27.3. CI: ubuntu-latest, 4 vCPUs. Every number below has a command in
section 7.

## 1. Scope delivered

| Planned item (docs/SPEC.md, Phase 4 row) | Status | Where |
| --- | --- | --- |
| Remaining CLI verbs: `status`, `config show\|reload\|diff`, `backends list`, `doctor` | done, every verb `--format table\|json\|csv` | #20 |
| The shared admin client | done since Phase 2 (`internal/admin.Client`); every admin verb and the TUI use it | #12, #20, #21 |
| Admin API additions: `GET /admin/status`, `POST /admin/limits/test`, `GET /admin/backends` | done (`/admin/limits/test` since Phase 2, D-039); status rates from a one-second sampler (D-050); per-endpoint counters on the pool | #20 |
| `tollbooth tui` with the five screens | done: Overview, Limits, Usage, Backends, Config with every key the specification lists (`r`, `enter`, `t`, `/`, `g`, `s`, `x`, `v`, `?`, `q`, `1`–`5`), polling, STALE banner, 80×24 minimum, ASCII under `NO_COLOR` and `TERM=dumb` (D-053) | #21 |
| Parity matrix test | done: `TestOperatorSurfaceParity` over `admin.Routes` × `docs/OPERATOR_SURFACE.md`, plus a registration probe of every route | #20 |
| CLI golden tests in all three formats | done: `TestGoldens`, 11 verbs × 3 formats + the reload ok/rejected pair, 38 files; `TestDoctorGolden` with environment-specific cells masked | #20 |
| `teatest` goldens | done: 30 golden frames at 80×24 and 120×40 (every screen, help, Limits detail and test form, Usage bars, STALE with and without a prior poll) via the model's `Update`/`View`, plus a `teatest` smoke run of the real program (D-052) | #21 |
| Runbook tasks (rule change; chargeback export) from the TUI and CLI alone | done: `docs/OPERATIONS.md` "Task: change a rule" and "Task: export chargeback data" | #21 |
| Not in the row, delivered | `doctor` (D-051) includes the tmpfs check deferred from Phase 3; `usage` verbs already shipped in Phase 3; one serve defect fixed (D-054: a context-driven stop could snapshot showback before the last request completed) | #21 |
| Deferred | `OPERATIONS.md` beyond the two tasks and the throttling section (install, upgrade, sizing, metric-by-metric) | Phase 5 |

## 2. Decisions and assumptions

No ADR this phase (ADR-0003, the TUI in one binary, was accepted at Gate 0 and is now built).
`docs/DECISIONS.md` gained D-050 to D-055:

| Id | One line |
| --- | --- |
| D-050 | `/admin/status` rates from a one-second sampler of two plain atomic counters the handler bumps; per-endpoint atomics on the pool for `/admin/backends` |
| D-051 | `doctor`'s check list: config, `RLIMIT_NOFILE` against the in-flight budget, `GOMAXPROCS`, `GOMEMLIMIT`, TLS pair and expiry, each backend by TCP and `HEAD /`, the kernel's clock-sync flag (`adjtimex`), `showback.dir` writable, headroom, not tmpfs; exit 1 only on a fail |
| D-052 | TUI golden frames by driving the model (`Update`/`View`) with the ASCII profile; `teatest` for one headless smoke run |
| D-053 | The TUI polls everything in one command per interval, keeps the last snapshot, flips a STALE banner on failure; sparklines from its own polls |
| D-054 | Background goroutines stop only from `drain`, after the data server has drained; request contexts are not cancelled by serve's context, so a context-driven stop equals `SIGTERM` (found by the race suite) |
| D-055 | The showback on/off throughput A/B is a gated measurement (`TOLLBOOTH_BIG=1`), not a PR-gate test: a 10% ratio on shared CPUs is noise |

## 3. Test results

| # | Category | Where | Result |
| --- | --- | --- | --- |
| 1–3, 8 | Classifier, identity, limiter, showback | unchanged, except that the showback on/off throughput A/B now runs only with `TOLLBOOTH_BIG=1` (D-055) | pass |
| 4 | Fuzzing | five targets | 5 targets × 60 s on the dev box (G2 run), no crashers; unchanged targets |
| 5 | Byte-equivalence | record mode every PR; Garage and versitygw in CI | record mode 293 cases, 0 diffs; CI run 42660074668 (#20): Garage 293 cases, 0 diffs, 28 backend gaps; versitygw 293 cases, 0 diffs, 8 gaps |
| 6 | Client compatibility | CI `integration` | 122 cells, 0 failures, 2 backend gaps (unchanged) |
| 7 | Throttling behavior | `test/e2e`, `docs/THROTTLE.md` | e2e pass; THROTTLE rows from the same run: aws cli default 78/200 and boto3 default 191/200 (fail, as at Gates 2 and 3); aws cli adaptive 200/200; **boto3 adaptive 199/200 this run** (200/200 in the two earlier runs: adaptive mode is far better but not a guarantee at 2× overload); s5cmd, rclone, s3cmd complete. `docs/THROTTLE.md` regenerated from this run |
| 9 | Chaos | unchanged rows | pass |
| 10 | Performance regression | `make bench-compare` | no benchmark changed this phase; the nightly compares against the Gate 2 baseline |
| 11 | Static analysis | CI on every PR | lint 0 issues, vet clean, govulncheck "No vulnerabilities found" (v1.9.0), go-licenses clean (four MIT modules added, TUI and test only), CodeQL and Semgrep clean on #20 and #21 after one fix (doctor's TLS probe pins TLS 1.2, the `missing-ssl-minversion` rule) |
| 12 | Operator surface | `TestOperatorSurfaceParity` (`internal/admin`); `TestGoldens` 36 subtests + `TestDoctorGolden` 3 formats (`cmd/tollbooth`); `TestGoldenFrames` 26 subtests + `TestGoldenUnreachable` 4 subtests + `TestKeysAndNotices` + `TestProgramSmoke` (`cmd/tollbooth/tui`); `TestStatusAndBackendsRoutes`, `TestSamplerRates` | all pass, 0 skip |

Race suite: `make race` clean on the fourth run, after a golden normalization, one real fix (D-054), and moving the showback throughput A/B behind `TOLLBOOTH_BIG=1` (D-055).
Coverage over `internal/` and `cmd/`: **90.2%** (floor 85%; Gate 3 was 92.3%; the TUI's rendering is covered through the golden frames).

Allocation pins unchanged: forwarded 4 KiB GetObject 7 (with the limiter, showback, and the
per-endpoint counters all on), throttled 6, limiter decision 0, showback increment 0.

Depguard: charmbracelet imported only from `cmd/tollbooth/tui` (`.golangci.yml`
`no-charm-outside-tui`); `make build` greps the binary for the test and bench modules.

## 4. Performance

Nothing this phase touches the data path except two atomic adds (the plain request counters,
D-050) and the per-endpoint atomics around `RoundTrip`; the allocation pins above did not move.
The end-to-end rows are Phase 5. Measured this phase:

| Measure | Result | Environment |
| --- | --- | --- |
| `GET /admin/status` | one request assembling counters, limiter status, showback hour, config view, and backend rows; no data-path lock taken | dev box |
| TUI poll (status, limits, top, backends, config, usage over two sealed hours) | six admin requests per tick; the teatest smoke run polls at 50 ms without missing frames | dev box |
| `doctor` | 17 ms wall for 10 checks against `deploy/tollbooth.example.yaml` on the dev box: two fails as designed there (the example backend is not running; `/var/lib/tollbooth` is not creatable from an unprivileged shell), one warn (`GOMEMLIMIT` unset), exit 1 | dev box |

## 5. Known gaps and risks

| Gap or risk | Proposed fix | Phase |
| --- | --- | --- |
| The TUI reads this node's showback directory only (`GET /admin/usage`); a multi-node export is a CLI task over the collected directories | Documented in the runbook; a `--path` on the TUI's Usage screen is a one-line addition if the field asks | 6 |
| `doctor`'s clock check reads the kernel's `STA_UNSYNC` flag, which some time daemons never clear even when synchronized (it then warns, never fails) | Documented as a warning; `chronyc`/`timedatectl` probes are daemon-specific and were not added | — |
| The CLI goldens mask the clock-dependent cells (uptime, "ago"); the TUI goldens inject the clock. A verb that prints a new time field needs a normalizer entry or the golden drifts | `TestGoldens` fails loudly, so drift cannot be silent | — |
| `OPERATIONS.md` has the two gate tasks and the throttling section only | The full runbook | 5 |
| boto3 in adaptive retry mode landed 199 of 200 objects in one of three CI runs at 2× overload (D-044 records "completes" for adaptive); the Go SDK's adaptive e2e asserts 0 failures and has passed every run so far | Soften the runbook's claim to "completes in every run measured but one"; if the Go e2e ever fails the same way, record it per mode like the default | 5 |
| D-034 and the Gate 2 and 3 gaps carried | unchanged | 5, 6 |

## 6. Questions

1. The runbook tasks in `docs/OPERATIONS.md` are written for an operator who has never seen the
   code; is a walkthrough by someone other than Opus wanted before Phase 5, or does Fable's read
   stand in for it? Recommended: **Fable's read**; the Phase 5 soak is the next chance to try them
   for real.
2. `GET /admin/status` requires both the config and the backends providers (D-050); fine?
   Recommended: **yes**.
3. Phase 5 (Hardening and v0.1.0) may begin on `phase-5` once #20 and #21 are merged into
   `phase-4`? Recommended: **yes**. Phase 5 needs the reference node (16 vCPU, 25 GbE) for the
   performance targets; the dev box cannot produce them.

## 7. Reproduce

```
make all                                     # build, lint, test (goldens, parity, frames), race (85% floor), s3diff record mode
make fuzz FUZZ_TIME=60s
go test -v ./internal/admin/ -run 'Parity|Status|Sampler'
go test -v ./cmd/tollbooth/ -run 'Goldens|Doctor'           # -update rewrites testdata/golden
go test -v ./cmd/tollbooth/tui/                              # -update rewrites testdata/*.golden
bin/tollbooth doctor --config deploy/tollbooth.example.yaml --format table
bin/tollbooth tui --admin 127.0.0.1:9900 --token "$(cat admin.token)" --config tollbooth.yaml
make vuln && make licenses
```

## Appendix: guardrail passes

### G1 Simplicity review

| Identifier | Callers |
| --- | --- |
| `admin.BackendStatus`, `Rates`, `StatusView`, `Server.Backends/NodeID/Version/StartedAt/Showback`, `Server.Run` | `cmd/tollbooth/serve.go`, `status.go`, `tui`, `internal/proxy`, tests |
| `proxy.Pool.Status` | `serve.go` |
| `admin.Metrics.TotalRequests/TotalThrottled/InflightCount` | `internal/proxy/handler.go`, `sampler.go`, tests |
| `tui.New`, `Run`, `Options`, `Model` (`Init`, `Update`, `View`), `Screen` constants | `cmd/tollbooth/tuicmd.go`, tests |
| `main.runStatus/runConfig/runBackends/runDoctor/runTUI` | `main.go` dispatch, tests |

Unexported where one caller: everything in `tui` except the Bubble Tea interface and `New`/`Run`/`Options`.
Dependencies added: `bubbletea` v1.3.10, `bubbles` v1.0.0, `lipgloss` v1.1.0 (MIT, TUI only, ADR-0003),
`x/exp/teatest` and `x/exp/golden` (MIT, test only); each has its `docs/deps.md` row and its notice.
Body buffering: none. Feature flags and kill switches: unchanged. LOC (`phase-4..p4/tui`, excluding the golden files and the notices): 26 files, +3,339 / −23; code (non-test Go) 8,472 → 10,811, tests 7,186 → 8,075; test-to-code ratio 0.85 → 0.75. The drop is the TUI's rendering code (`view.go`, 480 lines) and the verbs' table rendering, whose tests are the 68 golden files, which count as assets, not lines: 30 TUI frames and 38 CLI goldens hold the expected output that would otherwise be string literals in `_test.go`.

### G2 Validation

Run on the dev box (`run/g2-phase4.log`, then `run/race-p4.log` after the one fix; full output in
those files, summary lines pasted):

```
== make lint          0 issues; go vet ./... clean                                                        lint exit 0
== make race (1st)    FAIL: TestGoldenFrames/limits-detail-120x40, the deny ring's "9s ago" under -race       race exit 2
                      → fixed by normalizing the age in the frames (the frame is otherwise byte-identical)
== make race (2nd)    FAIL: TestServeShowbackSnapshotAndDrain: the final snapshot held 3 of 4 requests (1 in ~3 runs under load)   race exit 2
                      → a real defect, D-054: the background context derived from serve's context, so a context-driven stop
                        took the final snapshot at the same instant instead of after the data server drained; the test now
                        also dumps the serve log on failure
== make race (3rd)    FAIL: TestShowbackCostUnderTwoPercent, off 896 vs on 774 req/s under the parallel suite (13.6%)   race exit 2
                      → the throughput A/B is a measurement, not a unit test, and now runs only with TOLLBOOTH_BIG=1
                        (D-055: moved out of the PR gate, not weakened; the 2% claim is Phase 5's on the reference node)
== make race (4th)    every package ok under -race -count=1; total coverage 90.2%                       race exit 0
== make fuzz 60s      FuzzParse (config), FuzzClassify, FuzzParse, FuzzScanQuery (s3id), FuzzRead (showback)    fuzz exit 0
== make s3diff-record 293 cases, 0 diffs, 0 backend gaps                                                   s3diff-record exit 0
== make vuln          No vulnerabilities found.                                                             vuln exit 0
== make licenses      THIRD_PARTY_NOTICES.md regenerated: the four MIT TUI/test modules and their transitive notices added   licenses exit 0
== make build         bin/tollbooth links no aws-sdk, smithy, HdrHistogram, gopkg.in/yaml; charmbracelet only via cmd/tollbooth/tui   build exit 0
```

Garage and versitygw s3diff, the client matrix, and the throttle rows ran in CI on #20 and #21
(section 3).

### G3 Telemetry and docs

New paths and what observes them: the status summary and the TUI are readers only; the two plain
counters and the per-endpoint atomics are rendered by `/admin/status` and `/admin/backends`;
`doctor` writes to stdout. No catalog change (21 metrics). Docs updated: `docs/OPERATOR_SURFACE.md`
(Phase 4 state, every route served), `docs/OPERATIONS.md` (two tasks), `docs/DESIGN.md` §8 and
§12, `docs/STATUS.md`, `docs/DECISIONS.md`. No contradiction with the specification found this
phase; the specification's `/admin/usage?format=` is served as `json` and `csv` (Gate 3, question 3).

### G4 Dependency and license audit

`make licenses` (go-licenses v2.0.1, deny list GPL/LGPL/AGPL/SSPL) clean. Added this phase, all
MIT and all outside the data path: `github.com/charmbracelet/bubbletea` v1.3.10, `bubbles` v1.0.0,
`lipgloss` v1.1.0 (linked into `bin/tollbooth` for the `tui` verb only; depguard confines the import
to `cmd/tollbooth/tui`, ADR-0003) and their transitive MIT/BSD modules (`charmbracelet/x/*`,
`colorprofile`, `muesli/termenv`, `muesli/cancelreader`, `rivo/uniseg`, `xo/terminfo`,
`lucasb-eyer/go-colorful`, `mattn/*`, `aymanbagabas/go-osc52`); `charmbracelet/x/exp/teatest` and
`x/exp/golden` are test-only. `THIRD_PARTY_NOTICES.md` regenerated with their notices; `docs/deps.md`
has a row for each direct module. `make vuln`: no vulnerabilities. No code lifted this phase; no
code from MinIO, Garage, warp, or elbencho. YAML remains `go.yaml.in/yaml`.
