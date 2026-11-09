#!/usr/bin/env bash
# Throttling behavior with real client tools (test plan category 7): each tool, with its default
# retry settings, syncs OBJECTS objects through tollbooth against a rule whose rate is half the
# tool's concurrency and whose burst equals it. Recorded per row: whether the tool completed, how
# many objects landed, the proxy's denied count for the rule, wall time, and the floor the rule
# allows. aws cli and boto3 run twice, in their default retry mode and in adaptive mode
# (AWS_RETRY_MODE=adaptive), because the Go SDK results (test/e2e) showed the mode matters.
#
#   throttle.sh run <backend> <direct-endpoint> <via-endpoint> <admin-endpoint> <region> <ak> <sk> <config> <results-file>
#
# The rules are appended to <config> and loaded with POST /admin/reload, then removed and reloaded
# again at the end, so the run also exercises the reload path with real clients attached.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
BIN=$HERE/bin
VENV=$HERE/.venv
OBJECTS=${OBJECTS:-200}
SIZE=${SIZE:-65536}

# record TOOL MODE OBJECTS CONCURRENCY RATE BURST LANDED EXIT SECONDS FLOOR DENIED [detail]
record() { printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$@" >> "$RESULTS"; }

# denied RULE → the rule's denied counter from GET /admin/limits
denied() {
  curl -sf "http://$ADMIN/admin/limits" | python3 -c '
import json, sys
rule = sys.argv[1]
for r in json.load(sys.stdin)["rules"]:
    if r["id"] == rule:
        print(r["denied"]); break
else:
    print(0)' "$1"
}

# landed BUCKET → number of objects under sync/ in the bucket, counted direct at the backend
landed() {
  aws --endpoint-url "$DIRECT_EP" s3api list-objects-v2 --bucket "$1" --prefix sync/ --query 'length(Contents || `[]`)' --output text 2>/dev/null || echo 0
}

reload() {
  curl -sf -X POST "http://$ADMIN/admin/reload" > /dev/null || { echo "throttle: reload failed" >&2; return 1; }
}

# row TOOL MODE CONCURRENCY RATE BURST command...  runs one sync through the proxy and records it.
row() {
  local tool=$1 mode=$2 conc=$3 rate=$4 burst=$5; shift 5
  local bucket="thr-$tool" rule="thr-$tool" objects=$OBJECTS
  [ "$tool" = s3cmd ] && objects=$S3CMD_OBJECTS
  local before after rc=0 start end secs floor
  aws --endpoint-url "$DIRECT_EP" s3 mb "s3://$bucket" > /dev/null 2>&1 || true
  if [[ "$1" == "$BIN/"* || "$1" == "$VENV/"* ]] && [ ! -x "$1" ]; then
    record "$tool" "$mode" "$objects" "$conc" "$rate" "$burst" 0 127 0 0 0 "tool not installed"
    return
  fi
  before=$(denied "$rule")
  start=$(date +%s.%N)
  "$@" > "$RUN/throttle-$tool-$mode.log" 2>&1 || rc=$?
  end=$(date +%s.%N)
  after=$(denied "$rule")
  secs=$(python3 -c "print(f'{$end - $start:.1f}')")
  floor=$(python3 -c "print(f'{max(0, ($objects - $burst)) / $rate:.1f}')")
  record "$tool" "$mode" "$objects" "$conc" "$rate" "$burst" "$(landed "$bucket")" "$rc" "$secs" "$floor" "$((after - before))" "$(tail -1 "$RUN/throttle-$tool-$mode.log" | cut -c1-200)"
  echo "throttle: $tool/$mode: exit $rc, $(landed "$bucket")/$objects landed, $((after - before)) denied, ${secs}s (floor ${floor}s)"
  aws --endpoint-url "$DIRECT_EP" s3 rm "s3://$bucket/sync/" --recursive --quiet > /dev/null 2>&1 || true
}

run() {
  local backend=$1
  DIRECT_EP=$2 VIA_EP=$3 ADMIN=$4
  local REGION=$5 AK=$6 SK=$7 CFG=$8
  RESULTS=$9
  RUN=$(dirname "$RESULTS")
  : > "$RESULTS"
  export AWS_ACCESS_KEY_ID=$AK AWS_SECRET_ACCESS_KEY=$SK AWS_DEFAULT_REGION=$REGION AWS_EC2_METADATA_DISABLED=true
  ADMIN=${ADMIN#http://}
  local work; work=$(mktemp -d)
  trap 'rm -rf "$work"' RETURN
  mkdir -p "$work/sync"
  local i
  for i in $(seq 0 $((OBJECTS - 1))); do head -c "$SIZE" /dev/urandom > "$work/sync/$(printf '%05d' "$i").bin"; done
  S3CMD_OBJECTS=40
  mkdir -p "$work/sync40"
  for i in $(seq 0 $((S3CMD_OBJECTS - 1))); do cp "$work/sync/$(printf '%05d' "$i").bin" "$work/sync40/"; done

  # Concurrency 20 for every tool that has a knob (aws cli's max_concurrent_requests, boto3's
  # thread pool, s5cmd's workers, rclone's transfers); the rule is rate 10, burst 20. s3cmd uploads
  # sequentially, so its rule is rate 1, burst 1 over 40 objects (half of a concurrency of one would
  # be one request every two seconds; one per second keeps the row under a minute).
  local conc=20 rate=10 burst=20
  cat >> "$CFG" <<YAML
rate_limits:
  rules:
YAML
  local t
  for t in awscli boto3 s5cmd rclone; do
    cat >> "$CFG" <<YAML
    - id: thr-$t
      match: {buckets: [thr-$t]}
      group_by: [access_key]
      limit: {rate: $rate, burst: $burst}
YAML
  done
  cat >> "$CFG" <<YAML
    - id: thr-s3cmd
      match: {buckets: [thr-s3cmd]}
      limit: {rate: 1, burst: 1}
YAML
  reload

  aws configure set default.s3.max_concurrent_requests $conc >/dev/null 2>&1 || true
  row awscli default  $conc $rate $burst aws --endpoint-url "$VIA_EP" s3 sync "$work/sync" "s3://thr-awscli/sync/"
  row awscli adaptive $conc $rate $burst env AWS_RETRY_MODE=adaptive aws --endpoint-url "$VIA_EP" s3 sync "$work/sync" "s3://thr-awscli/sync/"

  local py=("$VENV/bin/python" "$HERE/clients/boto3_throttle.py")
  row boto3 default  $conc $rate $burst env ENDPOINT="$VIA_EP" REGION="$REGION" BUCKET=thr-boto3 OBJECTS="$OBJECTS" CONCURRENCY=$conc SIZE="$SIZE" "${py[@]}"
  row boto3 adaptive $conc $rate $burst env AWS_RETRY_MODE=adaptive ENDPOINT="$VIA_EP" REGION="$REGION" BUCKET=thr-boto3 OBJECTS="$OBJECTS" CONCURRENCY=$conc SIZE="$SIZE" "${py[@]}"

  row s5cmd default $conc $rate $burst "$BIN/s5cmd" --endpoint-url "$VIA_EP" --numworkers $conc sync "$work/sync/" "s3://thr-s5cmd/sync/"

  row rclone default $conc $rate $burst "$BIN/rclone" --s3-provider Other --s3-endpoint "$VIA_EP" --s3-region "$REGION" --s3-access-key-id "$AK" --s3-secret-access-key "$SK" --s3-force-path-style --transfers $conc --checkers $conc sync "$work/sync" ":s3:thr-rclone/sync"

  cat > "$work/s3cfg" <<CFG
[default]
access_key = $AK
secret_key = $SK
host_base = ${VIA_EP#http://}
host_bucket = ${VIA_EP#http://}
use_https = False
signature_v2 = False
bucket_location = $REGION
CFG
  row s3cmd default 1 1 1 "$VENV/bin/s3cmd" -c "$work/s3cfg" sync "$work/sync40/" "s3://thr-s3cmd/sync/"

  for t in awscli boto3 s5cmd rclone s3cmd; do
    aws --endpoint-url "$DIRECT_EP" s3 rb "s3://thr-$t" --force > /dev/null 2>&1 || true
  done
  # Remove the rules again and reload, so the next stage sees the original config.
  python3 - "$CFG" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
i = s.find("rate_limits:\n")
if i >= 0:
    open(p, "w").write(s[:i])
PY
  reload
}

case "${1:-}" in
  run) shift; run "$@" ;;
  *) echo "usage: $0 run <backend> <direct-endpoint> <via-endpoint> <admin-endpoint> <region> <ak> <sk> <config> <results-file>" >&2; exit 2 ;;
esac
