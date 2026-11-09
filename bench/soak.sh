#!/usr/bin/env bash
# make soak: the Phase 5 soak (docs/SPEC.md, Phase 5 gate: "soak shows flat memory and zero
# unexplained errors"). Null backend and tollbooth as processes, bench/loadgen in sdk mode at 50% of
# the peak measured by a short calibration run, for SOAK_HOURS (default 24), with a chaos step every
# CHAOS_EVERY (default 10m) drawn from a fixed rotation:
#
#   backend-503      the backend answers 503 for 30 s          → proxied 5xx, counted as backend status
#   backend-stall    the backend holds every request 15 s, 20 s long → slow responses, clients wait; no error unless a client times out
#   backend-reset    the backend closes without answering, 20 s → 503 ServiceUnavailable (proxy_error)
#   backend-down     the null backend process is stopped 30 s   → all backends unreachable, 503, health check marks down/up
#   reload-invalid   a bad config file is reloaded              → refused, running config kept
#   reload-valid     the rule set is rewritten and reloaded     → generation advances, no failed requests
#   showback-ro      showback.dir is made read-only for 90 s   → flush errors counted, counting continues
#   sighup           SIGHUP                                      → same as a reload
#
# Every SAMPLE_SECS (default 60) a sample lands in $OUT/soak.csv: RSS, goroutines, heap, total
# requests, throttled, proxy errors, backend 5xx, backend transport errors, flush errors, limiter
# keys, showback tuples, and the chaos step in effect. At the end soak-report.txt says whether memory
# was flat (RSS slope over the second half) and lists every error class with the chaos window it
# belongs to; an error outside a window is "unexplained". A chaos step starts only when the previous
# one has ended, so CHAOS_EVERY is a minimum spacing.
#
# The script refuses to run if its ports are taken (a stale null backend from an earlier run was the
# first smoke's finding) and always rebuilds bin/tollbooth from the working tree.
#
# Knobs: SOAK_HOURS, CHAOS_EVERY, SAMPLE_SECS, CONCURRENCY (16), OUT (bench/results/soak-<date>),
# CALIBRATE (20s).
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
DATE=$(date -u +%Y-%m-%dT%H%M%SZ)
OUT=${OUT:-$HERE/results/soak-$DATE}
SOAK_HOURS=${SOAK_HOURS:-24}
CHAOS_EVERY=${CHAOS_EVERY:-10m}
CONCURRENCY=${CONCURRENCY:-16}
CALIBRATE=${CALIBRATE:-20s}
SAMPLE_SECS=${SAMPLE_SECS:-60}
NULL_ADDR=${NULL_ADDR:-127.0.0.1:19000}
NULL_CTL=${NULL_CTL:-127.0.0.1:19001}
PROXY_ADDR=${PROXY_ADDR:-127.0.0.1:18000}
ADMIN_ADDR=${ADMIN_ADDR:-127.0.0.1:19900}
export GOTOOLCHAIN=${GOTOOLCHAIN:-auto}
export GOMEMLIMIT=${GOMEMLIMIT:-1GiB}

mkdir -p "$OUT/showback" "$ROOT/bin"
cd "$ROOT"
go build -o bin/nullbackend ./bench/nullbackend/cmd/nullbackend
go build -o bin/loadgen ./bench/loadgen
make build > "$OUT/build.txt" 2>&1

secs() { python3 -c "import sys; d=sys.argv[1]; u=d[-1]; n=float(d[:-1]); print(int(n*{'s':1,'m':60,'h':3600}[u]))" "$1"; }
CHAOS_SECS=$(secs "$CHAOS_EVERY")
TOTAL_SECS=$(python3 -c "print(int($SOAK_HOURS*3600))")

CFG=$OUT/tollbooth.yaml
write_cfg() { # write_cfg RATE: the rule set with a wide READ rule at RATE and a frozen bucket
  cat > "$CFG" <<YAML
node_id: soak
listen: {address: $PROXY_ADDR}
admin: {address: $ADMIN_ADDR, pprof: true}
backend:
  scheme: http
  endpoints: [$NULL_ADDR]
  health_check: {interval: 1s, timeout: 500ms}
showback: {dir: $OUT/showback, flush_interval: 60s}
logging: {level: warn}
rate_limits:
  rules:
    - id: wide-read
      match: {ops: [READ]}
      group_by: [access_key, bucket]
      limit: {rate: $1, burst: $1}
    - id: frozen
      match: {buckets: [load-frozen-0]}
      limit: {rate: 0}
YAML
}
write_cfg 1000000000
bin/tollbooth check-config "$CFG" > "$OUT/check-config.txt"

{
  echo "date: $DATE"; echo "go: $(go version)"; echo "kernel: $(uname -srvm)"
  echo "cpu: $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2- | sed 's/^ //') ($(nproc) vCPUs)"
  echo "memory: $(grep MemTotal /proc/meminfo | awk '{printf "%.1f GiB", $2/1048576}')"
  echo "GOMEMLIMIT: $GOMEMLIMIT GOGC: ${GOGC:-default}"
  echo "tollbooth: $(bin/tollbooth version)"
  echo "knobs: SOAK_HOURS=$SOAK_HOURS CHAOS_EVERY=$CHAOS_EVERY SAMPLE_SECS=$SAMPLE_SECS CONCURRENCY=$CONCURRENCY CALIBRATE=$CALIBRATE"
} > "$OUT/env.txt"
cat "$OUT/env.txt"

for port in "$NULL_ADDR" "$NULL_CTL" "$PROXY_ADDR" "$ADMIN_ADDR"; do
  if ss -ltn 2>/dev/null | awk '{print $4}' | grep -qx "$port"; then
    echo "soak: $port is already listening (a stale process from an earlier run?); refusing to start" >&2
    ss -ltnp 2>/dev/null | grep -F "$port" >&2 || true
    exit 1
  fi
done

# The null backend is restarted by the backend-down step from a background subshell, so its pid
# lives in a file the parent reads, not in a variable the subshell cannot update.
start_backend() {
  bin/nullbackend -listen "$NULL_ADDR" -control "$NULL_CTL" -size 4096 >> "$OUT/nullbackend.log" 2>&1 &
  echo $! > "$OUT/null.pid"
  for _ in $(seq 1 50); do curl -sf -o /dev/null "http://$NULL_CTL/served" && return 0; sleep 0.1; done
  echo "soak: null backend did not come up on $NULL_ADDR:" >&2; cat "$OUT/nullbackend.log" >&2
  return 1
}
null_pid() { cat "$OUT/null.pid" 2>/dev/null || true; }
# Never pass an empty or zero pid to kill: pid 0 is the whole process group, make included.
kill_pid() { # kill_pid PID [SIGNAL]
  [ -n "${1:-}" ] && [ "$1" -gt 0 ] || return 0
  if [ -n "${2:-}" ]; then kill "$2" "$1" 2>/dev/null || true; else kill "$1" 2>/dev/null || true; fi
}
LOAD_PID=
PROXY_PID=
cleanup() { kill_pid "$(null_pid)"; kill_pid "$PROXY_PID"; kill_pid "$LOAD_PID"; }
trap cleanup EXIT
start_backend
bin/tollbooth serve --config "$CFG" > "$OUT/tollbooth.log" 2>&1 &
PROXY_PID=$!
ready=0
for _ in $(seq 1 100); do curl -sf -o /dev/null "http://$ADMIN_ADDR/readyz" && { ready=1; break; }; sleep 0.1; done
if [ "$ready" -ne 1 ]; then echo "soak: tollbooth not ready on $ADMIN_ADDR:" >&2; cat "$OUT/tollbooth.log" >&2; exit 1; fi
echo "null backend $NULL_ADDR (control $NULL_CTL) pid $(null_pid), tollbooth $PROXY_ADDR (admin $ADMIN_ADDR) pid $PROXY_PID"

# Calibration: peak req/s of the sdk mix through the proxy on this box, then run at half of it by
# bounding the generator's concurrency and adding client-side latency through the loadgen's own
# pacing (CONCURRENCY workers at a fixed per-request sleep is what -rate does).
bin/loadgen -mode sdk -ops GET=70,PUT=20,HEAD=10 -proxied "http://$PROXY_ADDR" -concurrency "$CONCURRENCY" -keys 10 -buckets 4 -prefixes 4 \
  -duration "$CALIBRATE" -warmup 5s -runs 1 -out "$OUT" -label calibrate > "$OUT/calibrate.txt" 2>&1
PEAK=$(grep -E '^proxied ' "$OUT/calibrate.txt" | awk '{print int($2)}')
HALF=$(( PEAK / 2 ))
[ "$HALF" -ge 1 ] || HALF=1
echo "peak $PEAK req/s at concurrency $CONCURRENCY; soaking at $HALF req/s for ${SOAK_HOURS}h" | tee "$OUT/peak.txt"

# The soak load: one long run in sdk mode at the target rate (loadgen -rate paces every worker).
bin/loadgen -mode sdk -ops GET=70,PUT=20,HEAD=10 -proxied "http://$PROXY_ADDR" -concurrency "$CONCURRENCY" -keys 10 -buckets 4 -prefixes 4 \
  -rate "$HALF" -duration "${TOTAL_SECS}s" -warmup 0s -runs 1 -out "$OUT" -label soak > "$OUT/loadgen.txt" 2>&1 &
LOAD_PID=$!

metric() { curl -sf "http://$ADMIN_ADDR/metrics" | awk -v m="$1" '$1==m {print $2}' | head -1; }
metric_sum() { curl -sf "http://$ADMIN_ADDR/metrics" | awk -v m="$1" 'index($1, m"{")==1 || $1==m {s+=$2} END {print s+0}'; }
rss_kb() { awk '/VmRSS/ {print $2}' "/proc/$PROXY_PID/status" 2>/dev/null || echo 0; }

echo none > "$OUT/chaos.current"
echo "t,elapsed_s,chaos,rss_mib,goroutines,heap_mib,requests_total,throttled_total,errors_total,backend_5xx_total,backend_errors_total,flush_errors_total,limiter_keys,showback_tuples,reloads_ok,reloads_err" > "$OUT/soak.csv"
sample() {
  local now; now=$(date +%s)
  local req thr err b5 berr ferr keys tuples rok rerr gor heap
  req=$(metric_sum tollbooth_requests_total)
  thr=$(curl -sf "http://$ADMIN_ADDR/metrics" | awk '/^tollbooth_requests_total\{.*result="throttled"/ {s+=$2} END {print s+0}')
  err=$(curl -sf "http://$ADMIN_ADDR/metrics" | awk '/^tollbooth_requests_total\{.*result="error"/ {s+=$2} END {print s+0}')
  b5=$(curl -sf "http://$ADMIN_ADDR/metrics" | awk '/^tollbooth_requests_total\{.*status_class="5xx"/ {s+=$2} END {print s+0}')
  berr=$(metric_sum tollbooth_backend_errors_total)
  ferr=$(metric tollbooth_showback_flush_errors_total)
  keys=$(metric tollbooth_limiter_keys)
  tuples=$(metric tollbooth_showback_tuples)
  rok=$(curl -sf "http://$ADMIN_ADDR/metrics" | awk '/^tollbooth_config_reload_total\{result="ok"/ {print $2}')
  rerr=$(curl -sf "http://$ADMIN_ADDR/metrics" | awk '/^tollbooth_config_reload_total\{result="error"/ {print $2}')
  gor=$(metric go_goroutines)
  heap=$(curl -sf "http://$ADMIN_ADDR/metrics" | awk '$1=="go_memstats_heap_alloc_bytes" {printf "%.1f", $2/1048576}')
  echo "$(date -u +%FT%TZ),$((now - START)),$(cat "$OUT/chaos.current"),$(( $(rss_kb) / 1024 )),${gor:-0},${heap:-0},${req:-0},${thr:-0},${err:-0},${b5:-0},${berr:-0},${ferr:-0},${keys:-0},${tuples:-0},${rok:-0},${rerr:-0}" >> "$OUT/soak.csv"
}

chaos_log() { echo "$(date -u +%FT%TZ) $*" | tee -a "$OUT/chaos.log"; }
# shellcheck disable=SC2206 # SOAK_STEPS is a space-separated override for checking single steps
STEPS=(${SOAK_STEPS:-backend-503 backend-stall backend-reset backend-down reload-invalid reload-valid showback-ro sighup})
inject() { curl -sf "http://$NULL_CTL/chaos?$1" > /dev/null || chaos_log "inject $1 FAILED (control port unreachable)"; }
chaos() { # chaos STEP: inject, hold, clear; the window is logged with start and end and seen by sample
  set +e # runs as a background subshell; a failed injection is logged, not fatal
  echo "$1" > "$OUT/chaos.current"
  chaos_log "begin $1"
  case $1 in
    backend-503)   inject fail=503; sleep 30; inject clear=1 ;;
    backend-stall) inject stall_for=15s; sleep 20; inject clear=1 ;;
    backend-reset) inject reset=1; sleep 20; inject clear=1 ;;
    backend-down)  kill_pid "$(null_pid)"; sleep 30; start_backend || chaos_log "backend restart FAILED" ;;
    reload-invalid) cp "$CFG" "$CFG.good"; echo "bogus: 1" >> "$CFG"; curl -s -o "$OUT/reload-invalid.json" -w '%{http_code}\n' -X POST "http://$ADMIN_ADDR/admin/reload" >> "$OUT/chaos.log"; cp "$CFG.good" "$CFG"; sleep 5 ;;
    reload-valid)  write_cfg $(( 500000000 + RANDOM )); curl -s -o "$OUT/reload-valid.json" -w '%{http_code}\n' -X POST "http://$ADMIN_ADDR/admin/reload" >> "$OUT/chaos.log"; sleep 5 ;;
    showback-ro)   chmod -R a-w "$OUT/showback"; sleep 90; chmod -R u+w "$OUT/showback" ;;
    sighup)        kill_pid "$PROXY_PID" -HUP; sleep 5 ;;
    *)             chaos_log "unknown step $1" ;;
  esac
  chaos_log "end $1"
  echo none > "$OUT/chaos.current"
}

START=$(date +%s)
sample
next_chaos=$(( START + CHAOS_SECS ))
step=0
CHAOS_PID=
while [ $(( $(date +%s) - START )) -lt "$TOTAL_SECS" ]; do
  sleep "$SAMPLE_SECS"
  if ! kill -0 "$LOAD_PID" 2>/dev/null; then
    if [ $(( $(date +%s) - START )) -lt $(( TOTAL_SECS - SAMPLE_SECS )) ]; then chaos_log "loadgen exited early: FAIL"; fi
    break
  fi
  if ! kill -0 "$PROXY_PID" 2>/dev/null; then chaos_log "tollbooth exited: FAIL"; break; fi
  if [ "$(date +%s)" -ge "$next_chaos" ] && { [ -z "$CHAOS_PID" ] || ! kill -0 "$CHAOS_PID" 2>/dev/null; }; then
    echo "${STEPS[$(( step % ${#STEPS[@]} ))]}" > "$OUT/chaos.current" # before the spawn: the next sample must see it
    chaos "${STEPS[$(( step % ${#STEPS[@]} ))]}" &
    CHAOS_PID=$!
    step=$(( step + 1 ))
    next_chaos=$(( $(date +%s) + CHAOS_SECS ))
  fi
  sample
done
[ -n "$CHAOS_PID" ] && wait "$CHAOS_PID" 2>/dev/null || true
echo none > "$OUT/chaos.current"
sample
curl -sf "http://$ADMIN_ADDR/debug/pprof/heap" -o "$OUT/heap-final.pprof" || true
curl -sf "http://$ADMIN_ADDR/metrics" > "$OUT/metrics-final.txt" || true
kill_pid "$LOAD_PID"
kill_pid "$PROXY_PID" -TERM; wait "$PROXY_PID" 2>/dev/null || true
kill_pid "$(null_pid)"
trap - EXIT

python3 "$HERE/soak-report.py" "$OUT"
echo "results under $OUT"
