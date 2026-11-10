# Reuse: what is lifted from shunt, what is run, what is written

shunt (`github.com/blakegolliher/shunt`, branch `master`, commit `9b36d09`, Apache-2.0, same
author) holds roughly a third of this project. Its packages are all under `internal/`, so they are
copied, not imported. shunt is not modified for this project. The reverse integration (tollbooth's
packages into shunt) is a later project and does not shape this one.

## The map

| shunt source (at `9b36d09`) | Take | Adapt | Lands in |
| --- | --- | --- | --- |
| `internal/s3/op.go`, `classify.go`, with `TestOpNamesComplete`, `TestEveryRow`, `TestPrecedence` | The `Op` enum (95 entries) with classifier precedence and its tests | Add the 18 operations in docs/OPERATIONS_TABLE.md and the operation classes; match against the identity pass's query bitmask instead of `url.Values`; a test regenerates docs/OPERATIONS_TABLE.md from the Go table (the Go table is the single source of truth; no YAML, no generator) | `internal/s3id`, Phase 1 |
| `internal/s3/request.go` (`Parse(r, Domains)` → `RequestInfo`, `NewDomains`), `testdata/fuzz/FuzzParse` | Bucket and key extraction for both addressing styles, with the fuzz corpus | Extend with `access_key` (SigV4 header and query, SigV2 header and query), `source_ip` (peer, PROXY protocol, trusted XFF), and the list `prefix` parameter (ADR-0001). shunt's passthrough mode never parses the credential, so the access-key extractor is new | `internal/s3id`, Phase 1 |
| `internal/s3/bucketname.go` (`ValidBucketName`) | Bucket name validation | Used by `check-config` for `showback.prefixes` keys and a rule's `bucket` values | `internal/config`, Phase 1 |
| `internal/s3/errors.go` (`Render`, `Write`, the code table) | The S3 error table and XML rendering | Add `TooManyRequests`; add `Retry-After`; render from a pooled buffer | `internal/proxy`, Phase 1 |
| `internal/sigv4/canonical.go`: `RawPath`, `RawQuery` | Raw-URI preservation | Parse only. `Scope`, `CanonicalRequest`, `Sign`, `Verify` are not copied | `internal/s3id`, Phase 1 |
| `internal/proxy/handler.go` passthrough path (`preparePassthrough`, `roundTrip`, `relay`, `finish`), `copy.go`, `headers.go` | The streaming data path: `Host` preserved, pooled `io.CopyBuffer`, retry on connect error before any body byte, `ErrAbortHandler` on short read, per-chunk deadlines | Strip every resign-mode branch, admission gate, directory lookup, dual-delete, fallback-read, XML rewrite, `answerWatch`, `httptrace`, `Via`, request-id and `User-Agent` additions. A few hundred lines remain | `internal/proxy`, Phase 1 |
| `internal/upstream/cluster.go` (`Cluster`, endpoints, `Next`, `IsConnectError`) | Backend pool | Drop `Set`, `Registry`, secrets, generations; add active health checks and passive marking as one small type | `internal/proxy`, Phase 1 |
| `internal/listener/listener.go` | TLS listener | Add PROXY protocol (`go-proxyproto`) and trusted `X-Forwarded-For`; certificate hot reload through an `atomic.Pointer` | `internal/proxy`, Phase 1 |
| `internal/telemetry/metrics.go`, `accesslog.go`, `requestid.go`, `catalog_test.go` | Metrics scaffolding, catalog test, `StatusClass`, request ids, the `Access` record | `Access` becomes the showback aggregator's input; drop the migration, fleet, and window code | `internal/admin`, `internal/proxy`, Phase 1 |
| `internal/runtimecfg` | **Nothing** | shunt's bundle-and-drain exists because bundles own cluster connections; tollbooth's config is one `atomic.Pointer` and limiter state lives in the limiter keyed by rule name | — |
| `test/s3diff`, `test/e2e` (compose, `up.sh`, `gen-cert.sh`, Garage config), `test/bench/s3bench` | Byte-equivalence diff harness, backend compose, HDR latency harness | Starting point for test plan categories 5, 6, 9, 10; add versitygw and the recording backend | `test/`, `bench/`, Phases 1–2 |
| `CLAUDE.md`, `docs/adr/`, `docs/deps.md`, `docs/telemetry-catalog.md`, `docs/STATUS.md`, `docs/reuse.md`, `.golangci.yml`, `Makefile`, `.github/workflows/ci.yml` | The working framework | Copied in shape, rewritten in content | repo root and `docs/`, Phase 0 (done) |

## Lift procedure (every copied file, including from the author's own repo)

1. Copy the file into the target package and keep the original license header verbatim.
2. Directly under the header add `// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, <date>: <one line on what changed>.` Apache-2.0 §4(b) requires the modification notice.
3. Append shunt's `LICENSE` and `NOTICE` (present at `9b36d09`) to `THIRD_PARTY_NOTICES.md`.
4. Add a line to `docs/deps.md` (Lifted code): source repo, path, commit SHA, license, why the stdlib was not enough.
5. The G4 audit at every gate checks all of the above.

Note on step 1: shunt's Go files carry no per-file license header (the license is the repository
`LICENSE`). Each lifted file therefore gets, as its first lines, the Apache-2.0 short header
naming shunt and its copyright, followed by the modification line, so the origin is visible in
the file itself and not only in the notices.

## Never copied

MinIO server, `warp`, `mc` (AGPL-3.0), Garage (AGPL-3.0), elbencho (GPL-3.0). They run as external
processes only. The aws-samples rate-limit demo (MIT-0) informed the rate-plus-burst idea; no code
is taken from it. AWS documentation is referenced by link; only operation and error-code names
appear in the repo.
