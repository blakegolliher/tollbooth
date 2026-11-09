#!/usr/bin/env bash
# The client compatibility matrix (test plan category 6): aws cli v2, boto3, aws-sdk-go-v2 (via
# test/s3diff), s5cmd, rclone, s3cmd, and elbencho run their operations through tollbooth in front
# of one backend, and every cell lands in a results file that compat.sh folds into docs/COMPAT.md.
#
#   clients.sh fetch                      install the pinned tools into test/integration/bin and .venv
#   clients.sh run <backend> <endpoint> <region> <ak> <sk> <results-file>
#
# Tools are pinned by version and sha256 (computed 2026-11-09; s5cmd's and rclone's match their
# published checksum files, elbencho publishes none). All are external processes; none of their
# code enters the repository. mc is not used (D-022).
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
BIN=$HERE/bin
VENV=$HERE/.venv
mkdir -p "$BIN"

S5CMD_VERSION=2.3.0
S5CMD_URL="https://github.com/peak/s5cmd/releases/download/v${S5CMD_VERSION}/s5cmd_${S5CMD_VERSION}_Linux-64bit.tar.gz"
S5CMD_SHA256=de0fdbfa3aceae55e069ba81a0fc17b2026567637603734a387b2fca06c299b4

RCLONE_VERSION=v1.76.0
RCLONE_URL="https://github.com/rclone/rclone/releases/download/${RCLONE_VERSION}/rclone-${RCLONE_VERSION}-linux-amd64.zip"
RCLONE_SHA256=b00f00b3266c3764413afe599d3ec316f6f43b3e0ea8a0bc1d186d2133e46df7

ELBENCHO_VERSION=v3.3-3
ELBENCHO_URL="https://github.com/breuner/elbencho/releases/download/${ELBENCHO_VERSION}/elbencho-static-x86_64.tar.gz"
ELBENCHO_SHA256=cd33b7dd53dde164800df8eaefff9de8d680f9dc10fa195cc50a7a63db589cb2

BOTO3_VERSION=1.43.126
S3CMD_VERSION=2.4.0

# fetch_verified url sha256 dest → 0 on success; 1 (not exit) on a download or checksum failure, so
# one tool's bad day does not take the others down.
fetch_verified() {
  local url=$1 sha=$2 dest=$3 tmp code rc
  tmp=$(mktemp "$dest.XXXXXX")
  code=$(curl -sfL --retry 8 --retry-delay 5 --retry-all-errors --max-time 600 -o "$tmp" -w '%{http_code}' "$url") || rc=$?
  if [ "${rc:-0}" -ne 0 ]; then
    echo "clients: download failed (curl exit $rc, last HTTP status ${code:-none}): $url" >&2; rm -f "$tmp"; return 1
  fi
  if ! echo "$sha  $tmp" | sha256sum -c --quiet -; then
    rm -f "$tmp"; echo "clients: checksum mismatch for $url" >&2; return 1
  fi
  mv "$tmp" "$dest"
}

# fetch installs every tool it can and returns non-zero if any is missing afterwards. A missing
# tool shows up as FAIL cells in the matrix with "not installed", never as a silent skip.
fetch() {
  local missing=0
  if [ ! -x "$BIN/s5cmd" ]; then
    if fetch_verified "$S5CMD_URL" "$S5CMD_SHA256" "$BIN/s5cmd.tar.gz"; then
      tar -xzf "$BIN/s5cmd.tar.gz" -C "$BIN" s5cmd && rm -f "$BIN/s5cmd.tar.gz"
    else missing=1; fi
  fi
  if [ ! -x "$BIN/rclone" ]; then
    if fetch_verified "$RCLONE_URL" "$RCLONE_SHA256" "$BIN/rclone.zip"; then
      unzip -q -o -j "$BIN/rclone.zip" "rclone-${RCLONE_VERSION}-linux-amd64/rclone" -d "$BIN" && rm -f "$BIN/rclone.zip"
      chmod +x "$BIN/rclone"
    else missing=1; fi
  fi
  if [ ! -x "$BIN/elbencho" ]; then
    if fetch_verified "$ELBENCHO_URL" "$ELBENCHO_SHA256" "$BIN/elbencho.tar.gz"; then
      tar -xzf "$BIN/elbencho.tar.gz" -C "$BIN" elbencho && rm -f "$BIN/elbencho.tar.gz"
      chmod +x "$BIN/elbencho"
    else missing=1; fi
  fi
  if [ ! -x "$VENV/bin/python" ] || ! "$VENV/bin/python" -c "import boto3" 2>/dev/null; then
    if python3 -m venv "$VENV" && "$VENV/bin/pip" install -q "boto3==$BOTO3_VERSION" "s3cmd==$S3CMD_VERSION"; then :; else missing=1; fi
  fi
  command -v aws >/dev/null || { echo "clients: aws cli v2 is expected on the runner image" >&2; missing=1; }
  echo "clients: aws $(aws --version 2>&1 | cut -d' ' -f1), boto3 $BOTO3_VERSION, s3cmd $S3CMD_VERSION, s5cmd $S5CMD_VERSION, rclone $RCLONE_VERSION, elbencho $ELBENCHO_VERSION (missing=$missing)"
  return $missing
}

# record TOOL OP PASS|FAIL [detail]
record() { printf '%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "${4:-}" >> "$RESULTS"; }

# try TOOL OP command...  → PASS when the command exits 0, else FAIL with the last stderr line
try() {
  local tool=$1 op=$2; shift 2
  local out
  if [[ "$1" == "$BIN/"* || "$1" == "$VENV/"* ]] && [ ! -x "$1" ]; then
    record "$tool" "$op" FAIL "tool not installed (download failed)"; return
  fi
  if out=$("$@" 2>&1); then
    record "$tool" "$op" PASS
  else
    record "$tool" "$op" FAIL "$(printf '%s' "$out" | tail -1 | cut -c1-160)"
  fi
}

run() {
  local backend=$1 ENDPOINT=$2 REGION=$3 AK=$4 SK=$5
  RESULTS=$6
  : > "$RESULTS"
  export AWS_ACCESS_KEY_ID=$AK AWS_SECRET_ACCESS_KEY=$SK AWS_DEFAULT_REGION=$REGION AWS_EC2_METADATA_DISABLED=true
  local work; work=$(mktemp -d)
  trap 'rm -rf "$work"' RETURN
  head -c 4096 /dev/urandom > "$work/small.bin"
  head -c $((26 * 1024 * 1024)) /dev/urandom > "$work/big.bin" # six 5 MiB parts at the cli's threshold
  mkdir -p "$work/tree/a/b" && echo one > "$work/tree/a/1.txt" && echo two > "$work/tree/a/b/2.txt"

  # aws cli v2 ------------------------------------------------------------------------------
  local b="compat-awscli-$backend"
  aws configure set default.s3.multipart_threshold 5MB >/dev/null 2>&1 || true
  aws configure set default.s3.multipart_chunksize 5MB >/dev/null 2>&1 || true
  try awscli mb        aws --endpoint-url "$ENDPOINT" s3 mb "s3://$b"
  try awscli cp        aws --endpoint-url "$ENDPOINT" s3 cp "$work/small.bin" "s3://$b/dir/small.bin"
  try awscli get       aws --endpoint-url "$ENDPOINT" s3 cp "s3://$b/dir/small.bin" "$work/small.back" && cmp -s "$work/small.bin" "$work/small.back" || record awscli get-verify FAIL "downloaded bytes differ"
  try awscli sync      aws --endpoint-url "$ENDPOINT" s3 sync "$work/tree" "s3://$b/tree"
  try awscli ls        aws --endpoint-url "$ENDPOINT" s3 ls "s3://$b/tree/a/"
  try awscli multipart aws --endpoint-url "$ENDPOINT" s3 cp "$work/big.bin" "s3://$b/dir/big.bin"
  try awscli part-number aws --endpoint-url "$ENDPOINT" s3api get-object --bucket "$b" --key dir/big.bin --part-number 2 "$work/part2.bin"
  try awscli head-part aws --endpoint-url "$ENDPOINT" s3api head-object --bucket "$b" --key dir/big.bin --part-number 2
  try awscli attributes aws --endpoint-url "$ENDPOINT" s3api get-object-attributes --bucket "$b" --key dir/small.bin --object-attributes ETag ObjectSize
  try awscli presign   bash -c "curl -sf -o /dev/null \"\$(aws --endpoint-url $ENDPOINT s3 presign s3://$b/dir/small.bin --expires-in 300)\""
  for algo in CRC32 CRC32C SHA1 SHA256 CRC64NVME; do
    try awscli "checksum-$algo" aws --endpoint-url "$ENDPOINT" s3api put-object --bucket "$b" --key "dir/sum-$algo" --body "$work/small.bin" --checksum-algorithm "$algo"
  done
  try awscli rm        aws --endpoint-url "$ENDPOINT" s3 rm "s3://$b/dir/small.bin"
  try awscli rb        aws --endpoint-url "$ENDPOINT" s3 rb "s3://$b" --force

  # boto3 -----------------------------------------------------------------------------------
  local pyout
  if pyout=$(ENDPOINT="$ENDPOINT" REGION="$REGION" BUCKET="compat-boto3-$backend" ADDRESSING=path "$VENV/bin/python" "$HERE/clients/boto3_matrix.py" 2>&1); then :; fi
  while IFS=' ' read -r op status detail; do
    [ -n "$op" ] && record boto3 "$op" "$status" "$detail"
  done <<< "$pyout"

  # aws-sdk-go-v2 is exercised by test/s3diff (compat.sh stage 1); its cells come from that run.

  # s5cmd -----------------------------------------------------------------------------------
  b="compat-s5cmd-$backend"
  local s5="$BIN/s5cmd --endpoint-url $ENDPOINT"
  try s5cmd mb        $s5 mb "s3://$b"
  try s5cmd cp        $s5 cp "$work/small.bin" "s3://$b/dir/small.bin"
  try s5cmd get       $s5 cp "s3://$b/dir/small.bin" "$work/s5.back"
  try s5cmd sync      $s5 sync "$work/tree/" "s3://$b/tree/"
  try s5cmd ls        $s5 ls "s3://$b/tree/a/"
  try s5cmd multipart $s5 cp --concurrency 2 --part-size 5 "$work/big.bin" "s3://$b/dir/big.bin"
  try s5cmd rm        $s5 rm "s3://$b/dir/*"
  try s5cmd rb        bash -c "$s5 rm 's3://$b/*' >/dev/null 2>&1; $s5 rb s3://$b"

  # rclone ----------------------------------------------------------------------------------
  b="compat-rclone-$backend"
  local rc=(
    "$BIN/rclone" --s3-provider Other --s3-endpoint "$ENDPOINT" --s3-region "$REGION"
    --s3-access-key-id "$AK" --s3-secret-access-key "$SK" --s3-force-path-style --s3-upload-cutoff 5M --s3-chunk-size 5M
  )
  try rclone mb        "${rc[@]}" mkdir ":s3:$b"
  try rclone cp        "${rc[@]}" copyto "$work/small.bin" ":s3:$b/dir/small.bin"
  try rclone get       "${rc[@]}" copyto ":s3:$b/dir/small.bin" "$work/rclone.back"
  try rclone sync      "${rc[@]}" sync "$work/tree" ":s3:$b/tree"
  try rclone ls        "${rc[@]}" ls ":s3:$b/tree"
  try rclone multipart "${rc[@]}" copyto "$work/big.bin" ":s3:$b/dir/big.bin"
  try rclone rm        "${rc[@]}" delete ":s3:$b/dir"
  try rclone rb        "${rc[@]}" purge ":s3:$b"

  # s3cmd -----------------------------------------------------------------------------------
  b="compat-s3cmd-$backend"
  local hostport=${ENDPOINT#http://}
  local s3cfg="$work/s3cfg"
  cat > "$s3cfg" <<CFG
[default]
access_key = $AK
secret_key = $SK
host_base = $hostport
host_bucket = $hostport
use_https = False
signature_v2 = False
bucket_location = $REGION
multipart_chunk_size_mb = 5
CFG
  local s3c=("$VENV/bin/s3cmd" -c "$s3cfg")
  try s3cmd mb        "${s3c[@]}" mb "s3://$b"
  try s3cmd cp        "${s3c[@]}" put "$work/small.bin" "s3://$b/dir/small.bin"
  try s3cmd get       "${s3c[@]}" get --force "s3://$b/dir/small.bin" "$work/s3cmd.back"
  try s3cmd sync      "${s3c[@]}" sync "$work/tree/" "s3://$b/tree/"
  try s3cmd ls        "${s3c[@]}" ls "s3://$b/tree/"
  try s3cmd multipart "${s3c[@]}" put "$work/big.bin" "s3://$b/dir/big.bin"
  try s3cmd rm        "${s3c[@]}" rm "s3://$b/dir/small.bin"
  try s3cmd rb        "${s3c[@]}" rb --recursive --force "s3://$b"

  # elbencho --------------------------------------------------------------------------------
  b="compat-elbencho-$backend"
  local el=("$BIN/elbencho" --s3endpoints "$ENDPOINT" --s3key "$AK" --s3secret "$SK" --s3region "$REGION")
  try elbencho mb        "${el[@]}" -d "$b"
  try elbencho cp        "${el[@]}" -w -t 2 -n 1 -N 4 -s 1m -b 1m "$b"
  try elbencho get       "${el[@]}" -r -t 2 -n 1 -N 4 -s 1m -b 1m "$b"
  try elbencho multipart "${el[@]}" -w -t 1 -n 1 -N 1 -s 12m -b 6m "$b"
  try elbencho ls        "${el[@]}" --s3listobj 1000 "$b"
  try elbencho rm        "${el[@]}" -F -t 2 -n 1 -N 4 "$b"
  try elbencho rb        "${el[@]}" -D "$b"
}

case "${1:-}" in
  fetch) fetch ;;
  run) shift; run "$@" ;;
  *) echo "usage: $0 fetch | run <backend> <endpoint> <region> <ak> <sk> <results-file>" >&2; exit 2 ;;
esac
