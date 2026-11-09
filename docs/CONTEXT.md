# CONTEXT — ground truth about the environment

Facts about where tollbooth is built and tested that the code cannot derive. Do not re-derive
what this answers; update it when the environment changes. Dated entries are checks, not
assumptions.

## Toolchain

- Target Go: **1.27.3** (current stable, go.dev `VERSION`, 2026-11-08). `go.mod` pins `go 1.27.3`;
  `go mod tidy` drops a `toolchain` line equal to the `go` line, so the `go` directive alone is the
  pin. CI uses `go-version-file: go.mod`, so CI and local builds are identical.
- Dev box: RHEL 9 (`5.14.0-570.17.1.el9_6.x86_64`), **2 vCPUs** (`nproc` 2 on an AMD EPYC 7402P host; Go
  sees `GOMAXPROCS=2`), so local benchmark numbers are indicative only and `-race` suites run slowly
  while a fuzz run is going. Packaged Go **1.25.3** with `GOTOOLCHAIN=local`
  in the distribution's `go.env`. The Makefile exports `GOTOOLCHAIN=auto`, which downloads 1.27.3
  into the module cache on first use (verified 2026-11-08). Running `go` outside `make` needs
  `GOTOOLCHAIN=auto` in the environment.
- Tool binaries are pinned in the Makefile and installed into `./bin` (golangci-lint v2.15.0,
  go-licenses v2.0.1, govulncheck v1.9.0; all installed and run clean on 2026-11-08).
- `gh` is installed but not authenticated on the dev box; GitHub API lookups use `curl` against
  `api.github.com` unauthenticated. There is no GitHub remote yet.

## Backends for integration tests

No Docker on the dev box, and podman is unverified, so the integration suite does not use
containers (Gate 0 review). `test/integration/backends.sh` fetches pinned binaries into
`test/integration/bin/` and verifies each against a sha256 recorded in the script:

| Backend | Version | Source | Checked 2026-11-08 |
| --- | --- | --- | --- |
| Garage | v2.5.0 (latest tag on git.deuxfleurs.fr) | `garagehq.deuxfleurs.fr/_releases/v2.5.0/x86_64-unknown-linux-musl/garage`, 69,426,824 bytes, static-pie ELF | sha256 `2ff66728…9a9dea8` computed from the download; Garage publishes no checksum file at the release path |
| versitygw | v1.11.0 (released 2026-11-06) | GitHub release tarball `versitygw_v1.11.0_Linux_x86_64.tar.gz` | sha256 `1f075c9d…5b35b52`, identical to the release's `checksums.txt` |
| MinIO server | **not used** (D-022) | — | `dl.min.io` returns HTTP 410 with a notice that the open-source server, `mc`, and KES are archived; the repository is archived; the final tag `RELEASE.2025-10-15T17-29-55Z` has no binary assets (the last with assets is `RELEASE.2025-09-07T16-13-09Z`, sha256 `7c5bd851…fb855f`, verified, should anyone need it). Dropped at Gate 0: a frozen AGPL binary with no CVE fixes is a liability in a suite run for years |

Consequences of MinIO's archival: MinIO and `mc` are out of CI and the compatibility matrix
(D-022; the specification's rows are edited to match). `warp` as the bench cross-check (ADR-0002)
depends on its GitHub release assets still existing at Phase 5 and is re-examined then. The VAST
lab cluster is the primary validation target; Garage and versitygw are the maintained open-source
stand-ins.

Ports in the integration suite: Garage S3 `127.0.0.1:3900` (admin 3903, RPC 3901), versitygw
`127.0.0.1:7070`. Credentials are lab constants in
`backends.sh` except Garage's, which it mints on first start and writes to
`test/integration/data/garage.env`.

## Client tools for the compatibility matrix

`test/integration/clients.sh` pins (checked 2026-11-09): s5cmd 2.3.0 (sha256 matches the release's
`s5cmd_checksums.txt`), rclone v1.76.0 (matches `SHA256SUMS`), elbencho v3.3-3 (no published
checksum; sha256 `cd33b7dd…589cb2` computed from the download), boto3 1.43.126 and s3cmd 2.4.0 in a
venv, aws cli v2 from the runner image. GitHub release downloads sometimes return a transient HTTP
error; the fetchers retry on those (`--retry-all-errors`) and print the HTTP status on failure.

## Lab

The VAST cluster is reached with endpoint and credentials from environment variables
(`VAST_ENDPOINT`, `VAST_ACCESS_KEY_ID`, `VAST_SECRET_ACCESS_KEY`), never committed. Results of
lab runs are committed under `docs/` as the specification asks. The vhost deployment constraint
(Host preserved, so the backend must accept the proxy's hostname as a base domain) applies to it.

## Source of the lifted code

shunt is cloned read-only to a scratch directory at commit `9b36d09ed109add4a2bedfe1dbfae6108038ee5b`
(the `master` head on 2026-11-08) for every lift; nothing is lifted from a different commit without
updating `docs/deps.md`.
