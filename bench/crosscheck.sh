#!/usr/bin/env bash
# The external cross-check of the in-repo load generator (ADR-0002 as amended by ADR-0005): elbencho
# (GPL-3.0, run as a process, never linked; pinned by test/integration/clients.sh) drives S3 PUTs and
# GETs of the same object size as loadgen's raw mode against the direct and the proxied endpoints and
# reports throughput and latency percentiles, so a generator and a proxy that agree with each other
# are checked against a tool written by someone else.
#
#   bench/crosscheck.sh <direct-endpoint> <proxied-endpoint> <access-key> <secret> [bucket]
#
# Knobs: THREADS (16), OBJECTS (2000 per thread), SIZE (4k), OUT (bench/results/crosscheck-<date>).
# The backend must be a real S3 server (Garage or versitygw from test/integration/backends.sh, or
# the lab cluster): elbencho writes before it reads.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
DIRECT=${1:?direct endpoint, e.g. http://127.0.0.1:3900}
PROXIED=${2:?proxied endpoint, e.g. http://127.0.0.1:18000}
AK=${3:?access key}
SK=${4:?secret key}
BUCKET=${5:-crosscheck}
THREADS=${THREADS:-16}
OBJECTS=${OBJECTS:-2000}
SIZE=${SIZE:-4k}
DATE=$(date -u +%Y-%m-%dT%H%M%SZ)
OUT=${OUT:-$HERE/results/crosscheck-$DATE}
ELBENCHO=${ELBENCHO:-$ROOT/test/integration/bin/elbencho}
mkdir -p "$OUT"

[ -x "$ELBENCHO" ] || "$ROOT/test/integration/clients.sh" fetch
[ -x "$ELBENCHO" ] || { echo "crosscheck: elbencho not available" >&2; exit 1; }
"$ELBENCHO" --version > "$OUT/elbencho-version.txt" 2>&1 || true

run() { # run LABEL ENDPOINT: create the bucket, write, read, delete; results as CSV via --csvfile
  local label=$1 ep=$2
  local common=(--s3endpoints "$ep" --s3key "$AK" --s3secret "$SK" --s3region us-east-1 --csvfile "$OUT/$label.csv" --lat --latpercent)
  "$ELBENCHO" "${common[@]}" -d "$BUCKET-$label" > "$OUT/$label-mkdir.txt" 2>&1 || true
  echo "== $label write: $THREADS threads × $OBJECTS objects × $SIZE" | tee -a "$OUT/summary.txt"
  "$ELBENCHO" "${common[@]}" -w -t "$THREADS" -n 1 -N "$OBJECTS" -s "$SIZE" -b "$SIZE" "$BUCKET-$label" 2>&1 | tee "$OUT/$label-write.txt" | grep -E "IOPS|Throughput|Latency|lat" | tee -a "$OUT/summary.txt" || true
  echo "== $label read" | tee -a "$OUT/summary.txt"
  "$ELBENCHO" "${common[@]}" -r -t "$THREADS" -n 1 -N "$OBJECTS" -s "$SIZE" -b "$SIZE" "$BUCKET-$label" 2>&1 | tee "$OUT/$label-read.txt" | grep -E "IOPS|Throughput|Latency|lat" | tee -a "$OUT/summary.txt" || true
  "$ELBENCHO" "${common[@]}" -F -t "$THREADS" -n 1 -N "$OBJECTS" "$BUCKET-$label" > "$OUT/$label-delete.txt" 2>&1 || true
  "$ELBENCHO" "${common[@]}" -D "$BUCKET-$label" > "$OUT/$label-rmdir.txt" 2>&1 || true
}

# Interleaved: direct, proxied, direct, proxied, so both see the same machine state.
for round in 1 2; do
  run "direct-$round" "$DIRECT"
  run "proxied-$round" "$PROXIED"
done
echo "results under $OUT (summary.txt; per-run CSV with latency percentiles)"
