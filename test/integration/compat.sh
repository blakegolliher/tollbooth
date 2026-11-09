#!/usr/bin/env bash
# Byte-equivalence and compatibility runs against the pinned backends (test plan categories 5 and
# 6). Stage 1 (this file, Phase 1): bring the backends up, run tollbooth in front of each, and run
# test/s3diff direct versus via. Stage 2 (next PR): the client-tool matrix that writes docs/COMPAT.md.
#
#   compat.sh                 run everything, full logs under run/compat/
#   BACKENDS="garage"         restrict to one backend
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
RUN=$ROOT/run/compat
BIN=$ROOT/bin/tollbooth
mkdir -p "$RUN"
BACKENDS=${BACKENDS:-garage versitygw}
export GOTOOLCHAIN=${GOTOOLCHAIN:-auto}

"$HERE/backends.sh" up
trap '"$HERE/backends.sh" down; pkill -f "tollbooth serve --config $RUN/" 2>/dev/null || true' EXIT
# shellcheck disable=SC1091
eval "$("$HERE/backends.sh" env)"

rc=0
for b in $BACKENDS; do
  case $b in
    garage)    ep=127.0.0.1:3900; region=garage;    ak=$GARAGE_ACCESS_KEY; sk=$GARAGE_SECRET ;;
    versitygw) ep=127.0.0.1:7070; region=us-east-1; ak=$VGW_ACCESS_KEY;    sk=$VGW_SECRET ;;
    *) echo "compat: unknown backend $b" >&2; exit 2 ;;
  esac
  listen=127.0.0.1:18${RANDOM:0:3}
  admin=127.0.0.1:19${RANDOM:0:3}
  cfg=$RUN/tollbooth-$b.yaml
  cat > "$cfg" <<YAML
node_id: compat-$b
base_domains: [tb.example.com]
listen: {address: $listen}
admin: {address: $admin}
backend:
  scheme: http
  endpoints: [$ep]
showback: {dir: $RUN/showback-$b}
logging: {level: info}
YAML
  "$BIN" check-config "$cfg" > "$RUN/check-$b.log" 2>&1
  "$BIN" serve --config "$cfg" > "$RUN/tollbooth-$b.log" 2>&1 &
  pid=$!
  for _ in $(seq 1 100); do curl -s -o /dev/null "http://$admin/readyz" && break; sleep 0.1; done
  echo "compat: $b: tollbooth on $listen (admin $admin), backend $ep"
  if AWS_ACCESS_KEY_ID=$ak AWS_SECRET_ACCESS_KEY=$sk go run "$ROOT/test/s3diff" \
       -direct "http://tb.example.com" -direct-addr "$ep" -via "http://tb.example.com" -via-addr "$listen" \
       -domain tb.example.com -region "$region" -bucket "s3diff-$b" -sizes 0,1,4096,1048576,67108864 \
       > "$RUN/s3diff-$b.log" 2>&1; then
    echo "compat: $b: s3diff ok ($(tail -1 "$RUN/s3diff-$b.log"))"
  else
    echo "compat: $b: s3diff FAILED ($(tail -1 "$RUN/s3diff-$b.log")); see $RUN/s3diff-$b.log" >&2
    rc=1
  fi
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
done
exit $rc
