# test/

- `s3diff/` byte-equivalence harness (from shunt's `test/s3diff`, Phase 1)
- `e2e/` end-to-end scenarios (Phase 1 on)
- `integration/` `backends.sh` downloads pinned static Garage and versitygw binaries and builds MinIO from its last release tag (no container runtime); `compat.sh` drives the client matrix (`make compat`)
- `lab/` VAST runner; endpoint and credentials from environment variables, never committed
