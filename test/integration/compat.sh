#!/usr/bin/env bash
# Client compatibility matrix (test plan category 6). Phase 1 fills this in: it brings the backends
# up with backends.sh, runs the proxy in front of each, drives aws cli v2, boto3, aws-sdk-go-v2,
# s5cmd, rclone, mc, s3cmd, and elbencho through it, and writes docs/COMPAT.md.
set -euo pipefail
echo "compat: defined in Phase 1 (test/integration/compat.sh); backends.sh is ready" >&2
exit 1
