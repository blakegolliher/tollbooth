#!/usr/bin/env bash
# The demo stack on one machine: the in-repo null backend (or a real S3 endpoint via BACKEND),
# tollbooth with demo/tollbooth.demo.yaml, and optionally a background load so the TUI has
# something to show. Ports are chosen not to collide with bench/soak runs on the same box.
#
#   demo/run.sh start          build if needed, start, print the environment to paste
#   demo/run.sh load [RATE]    start bench/loadgen in sdk mode at RATE req/s (default 20) in the background
#   demo/run.sh chaos ARGS     fault-inject the null backend: fail=503 | stall_for=10s | reset=1 | clear=1
#   demo/run.sh status         what is running
#   demo/run.sh stop           stop everything, keep demo/state (showback files survive for `usage` verbs)
#   demo/run.sh clean          stop and remove demo/state
#
# Knobs: PROXY_ADDR (127.0.0.1:28000), ADMIN_ADDR (127.0.0.1:29900), BACKEND (empty: the null backend on
# 127.0.0.1:29000, control 127.0.0.1:29001), BACKEND_SCHEME (http), DEMO_DIR (demo/state).
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
DEMO_DIR=${DEMO_DIR:-$HERE/state}
PROXY_ADDR=${PROXY_ADDR:-127.0.0.1:28000}
ADMIN_ADDR=${ADMIN_ADDR:-127.0.0.1:29900}
NULL_ADDR=${NULL_ADDR:-127.0.0.1:29000}
NULL_CTL=${NULL_CTL:-127.0.0.1:29001}
BACKEND=${BACKEND:-}
BACKEND_SCHEME=${BACKEND_SCHEME:-http}
export GOTOOLCHAIN=${GOTOOLCHAIN:-auto}
cd "$ROOT"

pidfile() { echo "$DEMO_DIR/$1.pid"; }
alive() { local p; p=$(cat "$(pidfile "$1")" 2>/dev/null || true); [ -n "$p" ] && kill -0 "$p" 2>/dev/null; }
stop_one() { local p; p=$(cat "$(pidfile "$1")" 2>/dev/null || true); if [ -n "$p" ] && kill -0 "$p" 2>/dev/null; then kill "$p" 2>/dev/null || true; fi; rm -f "$(pidfile "$1")"; }
busy() { ss -ltn 2>/dev/null | awk '{print $4}' | grep -qx "$1"; }

start() {
  mkdir -p "$DEMO_DIR/showback"
  [ -x bin/tollbooth ] || make build > "$DEMO_DIR/build.log" 2>&1
  [ -x bin/nullbackend ] || go build -o bin/nullbackend ./bench/nullbackend/cmd/nullbackend
  [ -x bin/loadgen ] || go build -o bin/loadgen ./bench/loadgen
  for port in "$PROXY_ADDR" "$ADMIN_ADDR"; do
    if busy "$port"; then echo "demo: $port is already listening; run 'demo/run.sh stop' first" >&2; exit 1; fi
  done
  local backend=$BACKEND
  if [ -z "$backend" ]; then
    backend=$NULL_ADDR
    if ! alive nullbackend; then
      if busy "$NULL_ADDR"; then echo "demo: $NULL_ADDR is already listening" >&2; exit 1; fi
      bin/nullbackend -listen "$NULL_ADDR" -control "$NULL_CTL" -size 4096 > "$DEMO_DIR/nullbackend.log" 2>&1 &
      echo $! > "$(pidfile nullbackend)"
      for _ in $(seq 1 50); do curl -sf -o /dev/null "http://$NULL_CTL/served" && break; sleep 0.1; done
    fi
  fi
  sed -e "s#@PROXY_ADDR@#$PROXY_ADDR#" -e "s#@ADMIN_ADDR@#$ADMIN_ADDR#" -e "s#@BACKEND_SCHEME@#$BACKEND_SCHEME#" \
      -e "s#@BACKEND@#$backend#" -e "s#@SHOWBACK_DIR@#$DEMO_DIR/showback#" "$HERE/tollbooth.demo.yaml" > "$DEMO_DIR/tollbooth.yaml"
  bin/tollbooth check-config "$DEMO_DIR/tollbooth.yaml"
  bin/tollbooth serve --config "$DEMO_DIR/tollbooth.yaml" > "$DEMO_DIR/tollbooth.log" 2>&1 &
  echo $! > "$(pidfile tollbooth)"
  for _ in $(seq 1 100); do curl -sf -o /dev/null "http://$ADMIN_ADDR/readyz" && break; sleep 0.1; done
  curl -sf -o /dev/null "http://$ADMIN_ADDR/readyz" || { echo "demo: tollbooth did not become ready:" >&2; cat "$DEMO_DIR/tollbooth.log" >&2; exit 1; }
  cat <<BANNER

tollbooth demo is up.
  S3 endpoint for clients:  http://$PROXY_ADDR   (backend: $BACKEND_SCHEME://$backend)
  admin API:                http://$ADMIN_ADDR   (metrics, status, reload; the TUI and CLI talk to it)
  config:                   $DEMO_DIR/tollbooth.yaml
  showback files:           $DEMO_DIR/showback/
  logs:                     $DEMO_DIR/tollbooth.log

Paste into every demo shell:
  source $HERE/env.sh
Then follow $HERE/DEMO.md.
BANNER
}

load() {
  local rate=${1:-20}
  alive loadgen && stop_one loadgen
  bin/loadgen -mode sdk -ops GET=70,PUT=20,HEAD=10 -proxied "http://$PROXY_ADDR" -concurrency 4 -keys 3 -buckets 3 -prefixes 2 \
    -rate "$rate" -duration 24h -warmup 0s -runs 1 -out "$DEMO_DIR" -label demo > "$DEMO_DIR/loadgen.log" 2>&1 &
  echo $! > "$(pidfile loadgen)"
  echo "background load: $rate req/s of GET/PUT/HEAD across 3 keys and 3 buckets (pid $(cat "$(pidfile loadgen)"))"
}

chaos() { curl -sf "http://$NULL_CTL/chaos?$*" || echo "demo: the null backend's control port $NULL_CTL did not answer" >&2; }

status() {
  for n in nullbackend tollbooth loadgen; do
    if alive "$n"; then echo "$n: running (pid $(cat "$(pidfile "$n")"))"; else echo "$n: stopped"; fi
  done
  curl -sf "http://$ADMIN_ADDR/readyz" > /dev/null 2>&1 && echo "readyz: ok" || echo "readyz: not answering"
}

stop() { for n in loadgen tollbooth nullbackend; do stop_one "$n"; done; echo "stopped (demo/state kept)"; }

case ${1:-start} in
  start)  start ;;
  load)   shift; load "$@" ;;
  chaos)  shift; chaos "$@" ;;
  status) status ;;
  stop)   stop ;;
  clean)  stop; rm -rf "$DEMO_DIR"; echo "demo/state removed" ;;
  *)      sed -n 2,20p "$0"; exit 2 ;;
esac
