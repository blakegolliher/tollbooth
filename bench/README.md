# bench/

- `nullbackend/` the in-repo S3-shaped backend (library, used by the proxy tests and `test/s3diff -record`) and `nullbackend/cmd/nullbackend`, the same thing as a process for `bench/run.sh`
- `loadgen/` the load generator (ADR-0002): `-mode raw` fires presigned GET URLs over `net/http` for the latency rows (no signing on the hot path), `-mode sdk` drives an aws-sdk-go-v2 operation mix with the SDK's own retries for the throttle path; `-direct` and `-proxied` runs are interleaved and the added latency is proxied minus direct; HDR histograms (`hdrhistogram-go`, MIT, imported here and nowhere else, depguard) as text and JSON
- `run.sh` what `make bench-e2e` runs: null backend, tollbooth with 100 rules loaded and pprof on, the raw and sdk scenarios, CPU and allocation profiles during the proxied run, environment facts; everything lands under `results/<date>/`
- `warp/` (Phase 5) the external cross-check, a script and a pinned release only (AGPL-3.0, never code)

Results and profiles land under `results/<date>/` (docs/SPEC.md, Performance targets). The
reference-node numbers are Phase 5; the dev-box numbers in the gate reports are indicative.
