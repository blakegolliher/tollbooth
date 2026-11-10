#!/usr/bin/env bash
# make bench-e2e: the null backend, tollbooth with 100 rules loaded and pprof on, and bench/loadgen
# direct versus proxied with interleaved runs (ADR-0002; docs/SPEC.md, Performance targets,
# Method). CPU and allocation profiles are captured from the admin listener during a proxied run.
# Everything lands under bench/results/<date>/ together with the environment facts and the exact
# config. Knobs (environment): DURATION (30s), WARMUP (30s), RUNS (3), CONCURRENCY (32), SIZE (4096),
# KEYS (10), BUCKETS (4), PREFIXES (4), RULES (100), SCENARIOS ("raw sdk throttle"), OUT.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
DATE=$(date -u +%Y-%m-%dT%H%M%SZ)
OUT=${OUT:-$HERE/results/$DATE}
DURATION=${DURATION:-30s}
WARMUP=${WARMUP:-30s}
RUNS=${RUNS:-3}
CONCURRENCY=${CONCURRENCY:-32}
SIZE=${SIZE:-4096}
KEYS=${KEYS:-10}
BUCKETS=${BUCKETS:-4}
PREFIXES=${PREFIXES:-4}
RULES=${RULES:-100}
SCENARIOS=${SCENARIOS:-raw sdk throttle}
NULL_ADDR=${NULL_ADDR:-127.0.0.1:19000}
PROXY_ADDR=${PROXY_ADDR:-127.0.0.1:18000}
ADMIN_ADDR=${ADMIN_ADDR:-127.0.0.1:19900}
export GOTOOLCHAIN=${GOTOOLCHAIN:-auto}

mkdir -p "$OUT" "$ROOT/bin"
cd "$ROOT"
go build -o bin/nullbackend ./bench/nullbackend/cmd/nullbackend
go build -o bin/loadgen ./bench/loadgen
[ -x bin/tollbooth ] || make build

# Environment facts the specification asks every report to carry.
{
  echo "date: $DATE"
  echo "go: $(go version)"
  echo "kernel: $(uname -srvm)"
  echo "cpu: $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2- | sed 's/^ //')  ($(nproc) vCPUs)"
  echo "memory: $(grep MemTotal /proc/meminfo | awk '{printf "%.1f GiB", $2/1048576}')"
  echo "GOMAXPROCS: ${GOMAXPROCS:-default} GOGC: ${GOGC:-default} GOMEMLIMIT: ${GOMEMLIMIT:-default}"
  echo "tollbooth: $(bin/tollbooth version)"
  echo "knobs: DURATION=$DURATION WARMUP=$WARMUP RUNS=$RUNS CONCURRENCY=$CONCURRENCY SIZE=$SIZE KEYS=$KEYS BUCKETS=$BUCKETS PREFIXES=$PREFIXES RULES=$RULES"
} > "$OUT/env.txt"
cat "$OUT/env.txt"

# Config: RULES rules that name the load's operations but never match it (distinct buckets), so
# every request walks them; one wide GetObject and HeadObject rule counted per access key and
# bucket with a limit the load cannot reach; and one throttle rule on bucket load-throttle for the
# throttle scenario.
CFG=$OUT/tollbooth.yaml
{
  cat <<YAML
node_id: bench
listen: {address: $PROXY_ADDR}
admin: {address: $ADMIN_ADDR, pprof: true}
backend:
  scheme: http
  endpoints: [$NULL_ADDR]
showback: {dir: $OUT/showback}
logging: {level: warn}
limits:
YAML
  for i in $(seq 1 "$RULES"); do
    cat <<YAML
  - name: rule-$(printf '%03d' "$i")
    operation: [GetObject, HeadObject, PutObject, UploadPart]
    bucket: other-$i
    prefix: [ckpt/, logs/]
    limit_per_second: 1000
    burst: 2000
    per: [access_key, bucket, prefix]
YAML
  done
  cat <<YAML
  - name: wide-read
    operation: [GetObject, HeadObject]
    limit_per_second: 1000000000
    per: [access_key, bucket]
  - name: throttle
    bucket: load-throttle-0
    limit_per_second: 500
    per: access_key
YAML
} > "$CFG"
bin/tollbooth check-config "$CFG"

bin/nullbackend -listen "$NULL_ADDR" -size "$SIZE" > "$OUT/nullbackend.log" 2>&1 &
NULL_PID=$!
bin/tollbooth serve --config "$CFG" > "$OUT/tollbooth.log" 2>&1 &
PROXY_PID=$!
trap 'kill $NULL_PID $PROXY_PID 2>/dev/null || true' EXIT
for _ in $(seq 1 100); do curl -sf -o /dev/null "http://$ADMIN_ADDR/readyz" && break; sleep 0.1; done
echo "null backend $NULL_ADDR, tollbooth $PROXY_ADDR (admin $ADMIN_ADDR), $RULES+2 rules"

common=(-direct "http://$NULL_ADDR" -proxied "http://$PROXY_ADDR" -concurrency "$CONCURRENCY" -keys "$KEYS" -buckets "$BUCKETS" -prefixes "$PREFIXES" -duration "$DURATION" -warmup "$WARMUP" -runs "$RUNS" -size "$SIZE" -out "$OUT")

profile() { # profile NAME SECONDS: capture CPU and allocation profiles from the proxy during the run
  local name=$1 secs=$2
  curl -sf -o "$OUT/$name-cpu.pprof" "http://$ADMIN_ADDR/debug/pprof/profile?seconds=$secs" &
  curl -sf -o "$OUT/$name-allocs.pprof" "http://$ADMIN_ADDR/debug/pprof/allocs"
  wait
}

for sc in $SCENARIOS; do
  case $sc in
    raw)
      # Added latency and small-op throughput: presigned 4 KiB GETs, direct versus proxied. The
      # profile is taken inside the first proxied measurement: after the direct warm-up, the direct
      # run, and the proxied warm-up.
      ( sleep "$(python3 -c "
import sys
def secs(d): return float(d[:-1]) * (60 if d.endswith('m') else 1)
print(2 * secs(sys.argv[1]) + secs(sys.argv[2]) + 2)" "$WARMUP" "$DURATION")"; profile raw 10 ) &
      PROF_PID=$!
      bin/loadgen -mode raw -label get4k "${common[@]}" | tee "$OUT/raw.txt"
      wait "$PROF_PID" ;;
    sdk)
      # Realistic mix with the SDK's own signing and retries.
      bin/loadgen -mode sdk -ops GET=70,PUT=20,HEAD=10 -label mix "${common[@]}" | tee "$OUT/sdk.txt" ;;
    throttle)
      # The throttle path: every request to bucket load-throttle-0 at rate 500 per access key;
      # attempts and throttled attempts are counted by the SDK middleware.
      bin/loadgen -mode sdk -ops GET=100 -bucket-base load-throttle- -buckets 1 -label throttle -proxied "http://$PROXY_ADDR" -concurrency "$CONCURRENCY" -keys 1 -prefixes 1 -duration "$DURATION" -warmup 5s -runs 1 -out "$OUT" \
        | tee "$OUT/throttle.txt" ;;
  esac
done

curl -sf "http://$ADMIN_ADDR/metrics" > "$OUT/metrics.txt"
curl -sf "http://$ADMIN_ADDR/admin/limits" > "$OUT/limits.json"
echo "results under $OUT"
