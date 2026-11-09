# Dependencies

One line per dependency saying why the stdlib was not enough (CLAUDE.md). Tool binaries are pinned
in the Makefile and never enter `go.mod`. Versions below are the current releases checked on
2026-11-08; the G4 audit at each gate re-checks them. **No module is in `go.mod` yet** (Phase 0 has
no product code); each row below says which phase adds it.

## go.mod

Rows marked **in go.mod** are present; the rest are planned for the phase named.

| Module | Version | License | Where | Why | Phase |
| --- | --- | --- | --- | --- | --- |
| `go.yaml.in/yaml/v4` | v4.0.0 | Apache-2.0 (with MIT libyaml-derived files; ships LICENSE and NOTICE) | `internal/config` | Config is YAML; stdlib has no YAML. The YAML org's maintained fork; `gopkg.in/yaml.v3` is archived and must not appear in the binary (`make build` checks `go version -m`; depguard denies the import). v4 is GA as of 2026; shunt used the rc. `KnownFields(true)` gives the unknown-key rule and positional errors give key paths. | **in go.mod** (1) |
| `github.com/prometheus/client_golang` | v1.25.0 | Apache-2.0 (LICENSE and NOTICE) | `internal/admin`, metric handles in `proxy`, `limiter`, `showback` | Prometheus exposition; stdlib has none. Leanest hot path of the Go clients; handles are pre-resolved (DESIGN §9.3). Pulls `client_model`, `common`, `procfs`, `protobuf`, `klauspost/compress` (all Apache-2.0 or BSD-3) and `go.yaml.in/yaml/v2` through `prometheus/common` (not `gopkg.in`; the build check and depguard still pass). | **in go.mod** (1) |
| `github.com/prometheus/client_model` | v0.6.3 | Apache-2.0 | `internal/limiter` tests only (`dto.Metric`) | Already linked through `client_golang`; imported directly so a test can read a counter value without `prometheus/testutil`, which drags in `kylelemons/godebug`. Listed because `go mod tidy` makes it a direct requirement. | **in go.mod** (direct, test only) |
| `github.com/pires/go-proxyproto` | v0.16.0 | Apache-2.0 | `internal/proxy` listener | PROXY protocol v1 and v2 parsing on the listener. A hand-written parser would be another fuzz target for no gain. | **in go.mod** (1) |
| `github.com/charmbracelet/bubbletea` | v1.3.10 | MIT | `cmd/tollbooth/tui` only (depguard, ADR-0003) | Terminal UI event loop and renderer. | 4 |
| `github.com/charmbracelet/bubbles` | v1.0.0 | MIT | `cmd/tollbooth/tui` only | Table, viewport, text-input components for the five screens. | 4 |
| `github.com/charmbracelet/lipgloss` | v1.1.0 | MIT | `cmd/tollbooth/tui` only | Styling; honors `NO_COLOR`. | 4 |
| `github.com/charmbracelet/x/exp/teatest` | v0.0.0-20261101151554-27ecf3e4d454 (untagged upstream; pseudo-version) | MIT | `cmd/tollbooth/tui` tests only | Golden-frame tests at 80×24 and 120×40. Test-only; never linked. | 4 |
| `github.com/HdrHistogram/hdrhistogram-go` | v1.4.0 | MIT | `bench/` only (depguard) | Latency histograms with bounded relative error for the load generator (ADR-0002). Never linked into `bin/tollbooth`. | 2 |
| `github.com/aws/aws-sdk-go-v2` v1.47.8 (+ `credentials` v1.20.16, `service/s3` v1.114.9, `aws/signer/v4`), `github.com/aws/smithy-go` v1.28.8 | as listed | Apache-2.0 | `test/s3diff` (Phase 1), `bench/loadgen` SDK mode (Phase 2) | The SDK client for the byte-equivalence matrix and its signer for the raw signing modes (presigned, streaming seed signatures); tollbooth itself never signs. Never linked into `bin/tollbooth` (`make build` greps `go version -m`). | **in go.mod** (1), test and bench only |

Anything not in this table needs a one-line justification here before it is added (docs/SPEC.md,
Dependencies). No database driver, ever.

## Tool binaries (Makefile, ./bin)

| Tool | Version | License | Why |
| --- | --- | --- | --- |
| `golangci-lint` | v2.15.0 | GPL-3.0 (tool only, never linked) | The linter set in `.golangci.yml`, including `depguard`. |
| `go-licenses` (`github.com/google/go-licenses/v2`) | v2.0.1 | Apache-2.0 | `make licenses`: which modules `bin/tollbooth` links and under what license; deny list for GPL, LGPL, AGPL, SSPL (G4). |
| `govulncheck` (`golang.org/x/vuln`) | v1.9.0 | BSD-3 | `make vuln`: known vulnerabilities in the code paths actually called (G4). |
| `benchstat` (`golang.org/x/perf`) | latest (untagged) | BSD-3 | `make bench-compare` against the committed baseline. |

## External processes (never code, never vendored)

| Tool | License | Used as | Where |
| --- | --- | --- | --- |
| MinIO server | AGPL-3.0 | **not used**: archived upstream, dropped at Gate 0 (D-022, docs/CONTEXT.md) | — |
| Garage | AGPL-3.0 | static binary v2.5.0, sha256-pinned | `test/integration/backends.sh` |
| versitygw | Apache-2.0 | static binary v1.11.0, sha256-pinned (matches the release's checksums.txt) | `test/integration/backends.sh` |
| MinIO `warp` | AGPL-3.0 | release binary if still published, bench cross-check (ADR-0002) | `bench/warp` |
| elbencho | GPL-3.0 | static binary v3.3-3, sha256-pinned (computed; no published checksum), compatibility matrix | `test/integration/clients.sh` |
| aws cli v2 | Apache-2.0 | the runner image's | `test/integration/clients.sh` |
| boto3 1.43.126, s3cmd 2.4.0 | Apache-2.0, GPL-2.0 | pip into a venv | `test/integration/clients.sh` |
| s5cmd 2.3.0 | MIT | static binary, sha256 matches `s5cmd_checksums.txt` | `test/integration/clients.sh` |
| rclone v1.76.0 | MIT | static binary, sha256 matches `SHA256SUMS` | `test/integration/clients.sh` |
| `mc` | AGPL-3.0 | **not used** (dropped with MinIO, D-022) | — |

## Lifted code

Status column: **lifted** rows are in the tree with their headers and modification lines; *planned* rows land in the feature that needs them.

| Source repo | Path | Commit | License | Destination | Why the stdlib was not enough |
| --- | --- | --- | --- | --- | --- |
| github.com/blakegolliher/shunt | `internal/s3/op.go`, `classify.go`, `classify_test.go` | `9b36d09ed109add4a2bedfe1dbfae6108038ee5b` | Apache-2.0 (LICENSE and NOTICE carried) | `internal/s3id/op.go`, `classify.go`, `classify_test.go` | 95-operation classifier table with precedence and tests; extended by 18 operations (docs/OPERATIONS_TABLE.md), given classes, rewritten to match a query bitmask instead of `url.Values`, and indexed by (method, level) | **lifted 2026-11-09** |
| github.com/blakegolliher/shunt | `internal/s3/request.go`, `request_test.go`, `testdata/fuzz/FuzzParse/*` | same | Apache-2.0 | `internal/s3id/parse.go`, `parse_test.go`, `testdata/fuzz/FuzzParse/*` | Bucket and key extraction for both addressing styles with the fuzz corpus; extended with access key, source IP and trusted X-Forwarded-For, list prefix, flags; `net.SplitHostPort` replaced by a byte-level port strip (it allocated an error for a bare host) | **lifted 2026-11-09** |
| github.com/blakegolliher/shunt | `internal/config/config.go` (`Load`, `Parse`, `KeyedDecodeError`, `keyPath`), `internal/s3/bucketname.go` (`ValidBucketName`) | same | Apache-2.0 | `internal/config/load.go`, `validate.go` | The YAML loader with unknown keys rejected and every error keyed by its dotted path; bucket name validation for `showback.prefixes` keys | **lifted 2026-11-09** |
| github.com/blakegolliher/shunt | `internal/s3/errors.go` | same | Apache-2.0 | `internal/proxy/s3error.go` | S3 error rendering, reduced to the four codes tollbooth originates; `TooManyRequests`, `Retry-After`, a generated request id, and a pooled body buffer added | **lifted 2026-11-09** |
| github.com/blakegolliher/shunt | `internal/sigv4/canonical.go` (`RawPath` only) | same | Apache-2.0 | `internal/s3id/parse.go` (`RawTarget`) | Raw request-target preservation, returning path and query as two substrings; `RawQuery` (a `url.Values` decode) is not needed and not copied; nothing from signing or verification is copied | **lifted 2026-11-09** |
| github.com/blakegolliher/shunt | `internal/proxy/handler.go` (passthrough path), `copy.go`, `headers.go` | same | Apache-2.0 | `internal/proxy/handler.go`, `copy.go`, `headers.go` | Streaming data path, pooled `io.CopyBuffer`, per-chunk deadlines, retry-before-bytes, `ErrAbortHandler` on short read; every resign, admission, directory, and `Via` path removed | **lifted 2026-11-09** |
| github.com/blakegolliher/shunt | `internal/upstream/cluster.go` (endpoints, `IsConnectError`, health) | same | Apache-2.0 | `internal/proxy/backend.go` | Backend pool with round-robin, passive marking, active checks | **lifted 2026-11-09** |
| github.com/blakegolliher/shunt | `internal/listener/listener.go` | same | Apache-2.0 | `internal/proxy/listener.go` | TLS listener with min version and `http/1.1` only; PROXY protocol and trusted XFF added | **lifted 2026-11-09** |
| github.com/blakegolliher/shunt | `internal/telemetry/metrics.go`, `accesslog.go`, `requestid.go`, `catalog_test.go` | same | Apache-2.0 | `internal/admin/metrics.go`, `catalog_test.go`, `internal/proxy/access.go`, `s3error.go` (request id) | Metrics scaffolding with every handle pre-resolved, catalog and cardinality tests, request ids, the access record | **lifted 2026-11-09** |
| github.com/blakegolliher/shunt | `test/s3diff`, `test/e2e` compose and scripts, `test/bench` | same | Apache-2.0 | `test/s3diff`, `test/integration`, `bench/` | Byte-equivalence diff harness, backend compose, HDR latency harness | planned |

Not lifted: `internal/runtimecfg` (tollbooth's config is one `atomic.Pointer`), `internal/sigv4`
signing and verification, `internal/s3/s3response`, `internal/sigv4/chunked`, anything under
`internal/control`, `directory`, `migrate`, `admission`, `cp`, `member`, and the web UI.
