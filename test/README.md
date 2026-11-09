# test/

- `s3diff/` byte-equivalence harness (from shunt's `test/s3diff`): `-record` runs the recording backend and proxy in-process and diffs requests and responses (every PR); against Garage and versitygw it diffs responses (CI `integration`, `make compat`)
- `e2e/` end-to-end scenarios with real SDK clients against the proxy in-process over the null backend (category 7 from Phase 2: throttle responses parsed by aws-sdk-go-v2, completion under a 1 req/s rule, the 200-object sync at half concurrency in both SDK retry modes, the Expect: 100-continue reconnect cost); runs with `go test ./...`
- `integration/` `backends.sh` downloads pinned static Garage and versitygw binaries (no container runtime; MinIO dropped, D-022); `compat.sh` drives the client matrix (`make compat`)
- `lab/` VAST runner; endpoint and credentials from environment variables, never committed
