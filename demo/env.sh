# source demo/env.sh — the shell variables and helpers the demo walkthrough (demo/DEMO.md) uses.
# demo/run.sh prints the same lines after it starts the stack.
export TOLLBOOTH_ADMIN=${TOLLBOOTH_ADMIN:-127.0.0.1:29900}
export TOLLBOOTH_S3=${TOLLBOOTH_S3:-http://127.0.0.1:28000}
export TOLLBOOTH_DEMO_CONFIG=${TOLLBOOTH_DEMO_CONFIG:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/state/tollbooth.yaml}
export AWS_ACCESS_KEY_ID=${AWS_ACCESS_KEY_ID:-DEMO-TENANT-A}
export AWS_SECRET_ACCESS_KEY=${AWS_SECRET_ACCESS_KEY:-demo-secret}
export AWS_DEFAULT_REGION=${AWS_DEFAULT_REGION:-us-east-1}
export AWS_EC2_METADATA_DISABLED=true
export AWS_PAGER=""
# One attempt per command so a throttle is visible as the client sees it; drop these two to watch
# the SDK's own retries absorb the SlowDown instead.
export AWS_RETRY_MODE=standard
export AWS_MAX_ATTEMPTS=1
s3()    { aws --endpoint-url "$TOLLBOOTH_S3" s3 "$@"; }
s3api() { aws --endpoint-url "$TOLLBOOTH_S3" s3api "$@"; }
tb()    { "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/bin/tollbooth" "$@"; }
