#!/usr/bin/env bash
# The client compatibility matrix (test plan category 6): aws cli v2, boto3, aws-sdk-go-v2 (via
# test/s3diff), s5cmd, rclone, s3cmd, and elbencho each run their operations twice, direct to the
# backend and through tollbooth, and every cell lands in a results file that compat.sh folds into
# docs/COMPAT.md:
#
#   PASS  the operation succeeded through the proxy
#   FAIL  it succeeded direct and failed through the proxy: a transparency defect
#   GAP   it failed both ways identically: a backend limitation the proxy relayed faithfully
#
#   clients.sh fetch                      install the pinned tools into test/integration/bin and .venv
#   clients.sh run <backend> <direct-endpoint> <via-endpoint> <region> <ak> <sk> <results-file> [existing-bucket]
#
# By default every tool makes and removes buckets of its own (compat-<tool>-<backend>-direct and
# -via). Given an existing bucket, nothing is made or removed but objects under the key prefixes
# compat-<tool>-<backend>-direct/ and -via/ inside it: the mb and rb cells are not run, no command
# names anything outside those prefixes, and each tool's prefixes are emptied when it is done.
#
# Tools are pinned by version and sha256 (computed 2026-11-09; s5cmd's and rclone's match their
# published checksum files, elbencho publishes none). All are external processes; none of their
# code enters the repository. mc is not used (D-022).
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
# COMPAT_BIN and COMPAT_VENV let selftest.sh put logging stand-ins where the tools are looked for.
BIN=${COMPAT_BIN:-$HERE/bin}
VENV=${COMPAT_VENV:-$HERE/.venv}
mkdir -p "$BIN"

S5CMD_VERSION=2.3.0
S5CMD_URL="https://github.com/peak/s5cmd/releases/download/v${S5CMD_VERSION}/s5cmd_${S5CMD_VERSION}_Linux-64bit.tar.gz"
S5CMD_SHA256=de0fdbfa3aceae55e069ba81a0fc17b2026567637603734a387b2fca06c299b4

RCLONE_VERSION=v1.76.0
# rclone's own download host first; GitHub's release CDN is the fallback (it 504s intermittently).
RCLONE_URL="https://downloads.rclone.org/${RCLONE_VERSION}/rclone-${RCLONE_VERSION}-linux-amd64.zip"
RCLONE_URL2="https://github.com/rclone/rclone/releases/download/${RCLONE_VERSION}/rclone-${RCLONE_VERSION}-linux-amd64.zip"
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
    else
      # Same tag from source through the module proxy when the release CDN is down (s5cmd is MIT).
      echo "clients: s5cmd release download failed; building ${S5CMD_VERSION} from source" >&2
      GOTOOLCHAIN=auto GOFLAGS=-mod=mod GOBIN="$BIN" go install "github.com/peak/s5cmd/v2@v${S5CMD_VERSION}" || missing=1
    fi
  fi
  if [ ! -x "$BIN/rclone" ]; then
    if fetch_verified "$RCLONE_URL" "$RCLONE_SHA256" "$BIN/rclone.zip" || fetch_verified "$RCLONE_URL2" "$RCLONE_SHA256" "$BIN/rclone.zip"; then
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
  if [ ! -x "$VENV/bin/python" ] || ! "$VENV/bin/python" -c "import boto3, awscrt" 2>/dev/null; then
    # The pinned boto3 needs Python 3.10 or newer. A host's `python3` can be older (RHEL 9's is
    # 3.9, where pip answers with every version it does know, several screens of them). `python3`
    # builds the environment when it is new enough, as it always has in CI; otherwise the newest
    # other interpreter on the PATH that is, and an environment built by an older interpreter is
    # rebuilt. pip's own output goes to a log; the outcome is one line.
    local py="" cand
    for cand in python3 python3.13 python3.12 python3.11 python3.10; do
      if command -v "$cand" >/dev/null && "$cand" -c 'import sys; sys.exit(0 if sys.version_info >= (3, 10) else 1)' 2>/dev/null; then py=$cand; break; fi
    done
    if [ -z "$py" ]; then
      echo "clients: boto3 $BOTO3_VERSION and s3cmd need Python 3.10 or newer; this host has only $(python3 --version 2>&1). Install one (for example python3.12) and run again; until then the boto3 and s3cmd cells fail as not installed" >&2
      missing=1
    else
      rm -rf "$VENV"
      # botocore[crt] brings the CRC32C and CRC64NVME implementations boto3 needs for the checksum leg.
      if "$py" -m venv "$VENV" && "$VENV/bin/pip" install -q --disable-pip-version-check "boto3==$BOTO3_VERSION" "botocore[crt]" "s3cmd==$S3CMD_VERSION" > "$HERE/.venv-install.log" 2>&1; then
        echo "clients: python environment built with $("$VENV/bin/python" --version 2>&1)"
      else
        echo "clients: installing boto3 $BOTO3_VERSION and s3cmd $S3CMD_VERSION with $py failed: $(tail -1 "$HERE/.venv-install.log" | cut -c1-200) (full output in $HERE/.venv-install.log)" >&2
        missing=1
      fi
    fi
  fi
  command -v aws >/dev/null || { echo "clients: aws cli v2 is expected on the runner image" >&2; missing=1; }
  echo "clients: aws $(aws --version 2>&1 | cut -d' ' -f1), boto3 $BOTO3_VERSION, s3cmd $S3CMD_VERSION, s5cmd $S5CMD_VERSION, rclone $RCLONE_VERSION, elbencho $ELBENCHO_VERSION (missing=$missing)"
  return $missing
}

# record TOOL OP PASS|FAIL|GAP [detail]
record() { printf '%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "${4:-}" >> "$RESULTS"; }

# side SIDE command...  runs the command with @EP@, @B@, @P@, @S@, @CFG@ substituted for the side.
# @B@ is the bucket and @P@ the key prefix inside it: a bucket of the tool's own and no prefix, or
# the existing bucket and the tool's own prefix.
side() {
  local s=$1; shift
  local ep b p cfg args=()
  if [ "$s" = direct ]; then ep=$DIRECT_EP; cfg=$S3CFG_DIRECT; else ep=$VIA_EP; cfg=$S3CFG_VIA; fi
  b="$BUCKET_BASE-$s"; p=""
  if [ -n "$EXISTING_BUCKET" ]; then b=$EXISTING_BUCKET; p="$BUCKET_BASE-$s/"; fi
  local a
  for a in "$@"; do
    a=${a//@EP@/$ep}; a=${a//@B@/$b}; a=${a//@P@/$p}; a=${a//@S@/$s}; a=${a//@CFG@/$cfg}
    args+=("$a")
  done
  "${args[@]}" 2>&1
}

# bucket TOOL OP command...  is `both` for a step that makes or removes a bucket: not run, and not
# recorded, when the bucket is an existing one that is not ours to make or remove.
bucket() {
  [ -n "$EXISTING_BUCKET" ] || both "$@"
}

# sweep  empties the current tool's two prefixes in an existing bucket, direct at the backend. The
# prefix always begins with compat-<tool>-<backend>-, so nothing else in the bucket is named.
sweep() {
  [ -n "$EXISTING_BUCKET" ] || return 0
  local s
  for s in direct via; do
    aws --endpoint-url "$DIRECT_EP" s3 rm "s3://$EXISTING_BUCKET/$BUCKET_BASE-$s/" --recursive --quiet >/dev/null 2>&1 || true
  done
}

# both TOOL OP command...  → PASS / FAIL / GAP from the direct and via outcomes.
both() {
  local tool=$1 op=$2; shift 2
  if [[ "$1" == "$BIN/"* || "$1" == "$VENV/"* ]] && [ ! -x "$1" ]; then
    record "$tool" "$op" FAIL "tool not installed (download failed)"; return
  fi
  local dout vout drc=0 vrc=0
  dout=$(side direct "$@") || drc=$?
  vout=$(side via "$@") || vrc=$?
  if [ $vrc -eq 0 ]; then
    record "$tool" "$op" PASS
  elif [ $drc -ne 0 ]; then
    record "$tool" "$op" GAP "backend: $(printf '%s' "$dout" | tail -1 | cut -c1-300)"
  else
    record "$tool" "$op" FAIL "$(printf '%s' "$vout" | tail -1 | cut -c1-300)"
  fi
}

run() {
  local backend=$1
  DIRECT_EP=$2 VIA_EP=$3
  local REGION=$4 AK=$5 SK=$6
  RESULTS=$7
  EXISTING_BUCKET=${8:-}
  : > "$RESULTS"
  export AWS_ACCESS_KEY_ID=$AK AWS_SECRET_ACCESS_KEY=$SK AWS_DEFAULT_REGION=$REGION AWS_EC2_METADATA_DISABLED=true
  local work; work=$(mktemp -d)
  trap 'rm -rf "$work"' RETURN
  head -c 4096 /dev/urandom > "$work/small.bin"
  head -c $((26 * 1024 * 1024)) /dev/urandom > "$work/big.bin" # six 5 MiB parts at the cli's threshold
  mkdir -p "$work/tree/a/b" && echo one > "$work/tree/a/1.txt" && echo two > "$work/tree/a/b/2.txt"
  for s in direct via; do
    hp=$DIRECT_EP; [ "$s" = via ] && hp=$VIA_EP
    hp=${hp#http://}
    cat > "$work/s3cfg-$s" <<CFG
[default]
access_key = $AK
secret_key = $SK
host_base = $hp
host_bucket = $hp
use_https = False
signature_v2 = False
bucket_location = $REGION
multipart_chunk_size_mb = 5
CFG
  done
  S3CFG_DIRECT=$work/s3cfg-direct S3CFG_VIA=$work/s3cfg-via

  # aws cli v2 ------------------------------------------------------------------------------
  BUCKET_BASE="compat-awscli-$backend"
  aws configure set default.s3.multipart_threshold 5MB >/dev/null 2>&1 || true
  aws configure set default.s3.multipart_chunksize 5MB >/dev/null 2>&1 || true
  bucket awscli mb        aws --endpoint-url @EP@ s3 mb "s3://@B@"
  both awscli cp          aws --endpoint-url @EP@ s3 cp "$work/small.bin" "s3://@B@/@P@dir/small.bin"
  both awscli get         bash -c "aws --endpoint-url @EP@ s3 cp s3://@B@/@P@dir/small.bin $work/small.@S@ && cmp -s $work/small.bin $work/small.@S@"
  both awscli sync        aws --endpoint-url @EP@ s3 sync "$work/tree" "s3://@B@/@P@tree"
  both awscli ls          aws --endpoint-url @EP@ s3 ls "s3://@B@/@P@tree/a/"
  both awscli multipart   aws --endpoint-url @EP@ s3 cp "$work/big.bin" "s3://@B@/@P@dir/big.bin"
  both awscli part-number aws --endpoint-url @EP@ s3api get-object --bucket @B@ --key @P@dir/big.bin --part-number 2 "$work/part2.@S@"
  both awscli head-part   aws --endpoint-url @EP@ s3api head-object --bucket @B@ --key @P@dir/big.bin --part-number 2
  both awscli attributes  aws --endpoint-url @EP@ s3api get-object-attributes --bucket @B@ --key @P@dir/small.bin --object-attributes ETag ObjectSize
  both awscli presign     bash -c "curl -sf -o /dev/null \"\$(aws --endpoint-url @EP@ s3 presign s3://@B@/@P@dir/small.bin --expires-in 300)\""
  for algo in CRC32 CRC32C SHA1 SHA256 CRC64NVME; do
    both awscli "checksum-$algo" aws --endpoint-url @EP@ s3api put-object --bucket @B@ --key "@P@dir/sum-$algo" --body "$work/small.bin" --checksum-algorithm "$algo"
  done
  both awscli rm          aws --endpoint-url @EP@ s3 rm "s3://@B@/@P@dir/small.bin"
  bucket awscli rb        aws --endpoint-url @EP@ s3 rb "s3://@B@" --force
  sweep

  # boto3 -----------------------------------------------------------------------------------
  local dpy vpy
  BUCKET_BASE="compat-boto3-$backend"
  local dbucket="$BUCKET_BASE-direct" vbucket="$BUCKET_BASE-via" dprefix="" vprefix=""
  if [ -n "$EXISTING_BUCKET" ]; then dbucket=$EXISTING_BUCKET; vbucket=$EXISTING_BUCKET; dprefix="$BUCKET_BASE-direct/"; vprefix="$BUCKET_BASE-via/"; fi
  dpy=$(ENDPOINT="$DIRECT_EP" REGION="$REGION" BUCKET="$dbucket" PREFIX="$dprefix" ADDRESSING=path "$VENV/bin/python" "$HERE/clients/boto3_matrix.py" 2>&1 || true)
  vpy=$(ENDPOINT="$VIA_EP" REGION="$REGION" BUCKET="$vbucket" PREFIX="$vprefix" ADDRESSING=path "$VENV/bin/python" "$HERE/clients/boto3_matrix.py" 2>&1 || true)
  while IFS=' ' read -r op status detail; do
    [ -n "$op" ] || continue
    if [ "$status" = PASS ]; then
      record boto3 "$op" PASS
    else
      dstatus=$(printf '%s\n' "$dpy" | awk -v o="$op" '$1==o {print $2}')
      if [ "$dstatus" = FAIL ]; then
        record boto3 "$op" GAP "backend: $(printf '%s\n' "$dpy" | awk -v o="$op" '$1==o {$1="";$2=""; print}' | cut -c1-300)"
      else
        record boto3 "$op" FAIL "$detail"
      fi
    fi
  done <<< "$vpy"
  sweep

  # aws-sdk-go-v2 is exercised by test/s3diff (compat.sh stage 1); its cells come from that run.

  # s5cmd -----------------------------------------------------------------------------------
  BUCKET_BASE="compat-s5cmd-$backend"
  bucket s5cmd mb      "$BIN/s5cmd" --endpoint-url @EP@ mb "s3://@B@"
  both s5cmd cp        "$BIN/s5cmd" --endpoint-url @EP@ cp "$work/small.bin" "s3://@B@/@P@dir/small.bin"
  both s5cmd get       "$BIN/s5cmd" --endpoint-url @EP@ cp "s3://@B@/@P@dir/small.bin" "$work/s5.@S@"
  both s5cmd sync      "$BIN/s5cmd" --endpoint-url @EP@ sync "$work/tree/" "s3://@B@/@P@tree/"
  both s5cmd ls        "$BIN/s5cmd" --endpoint-url @EP@ ls "s3://@B@/@P@tree/a/"
  both s5cmd multipart "$BIN/s5cmd" --endpoint-url @EP@ cp --concurrency 2 --part-size 5 "$work/big.bin" "s3://@B@/@P@dir/big.bin"
  both s5cmd rm        "$BIN/s5cmd" --endpoint-url @EP@ rm "s3://@B@/@P@dir/*"
  bucket s5cmd rb      bash -c "$BIN/s5cmd --endpoint-url @EP@ rm 's3://@B@/*' >/dev/null 2>&1; $BIN/s5cmd --endpoint-url @EP@ rb s3://@B@"
  sweep

  # rclone ----------------------------------------------------------------------------------
  BUCKET_BASE="compat-rclone-$backend"
  local rc=("$BIN/rclone" --s3-provider Other --s3-endpoint @EP@ --s3-region "$REGION" --s3-access-key-id "$AK" --s3-secret-access-key "$SK" --s3-force-path-style --s3-upload-cutoff 5M --s3-chunk-size 5M)
  # rclone makes the bucket before an upload unless told the bucket is there.
  [ -z "$EXISTING_BUCKET" ] || rc+=(--s3-no-check-bucket)
  bucket rclone mb      "${rc[@]}" mkdir ":s3:@B@"
  both rclone cp        "${rc[@]}" copyto "$work/small.bin" ":s3:@B@/@P@dir/small.bin"
  both rclone get       "${rc[@]}" copyto ":s3:@B@/@P@dir/small.bin" "$work/rclone.@S@"
  both rclone sync      "${rc[@]}" sync "$work/tree" ":s3:@B@/@P@tree"
  both rclone ls        "${rc[@]}" ls ":s3:@B@/@P@tree"
  both rclone multipart "${rc[@]}" copyto "$work/big.bin" ":s3:@B@/@P@dir/big.bin"
  both rclone rm        "${rc[@]}" delete ":s3:@B@/@P@dir"
  bucket rclone rb      "${rc[@]}" purge ":s3:@B@"
  sweep

  # s3cmd -----------------------------------------------------------------------------------
  BUCKET_BASE="compat-s3cmd-$backend"
  local s3c=("$VENV/bin/s3cmd" -c @CFG@)
  bucket s3cmd mb      "${s3c[@]}" mb "s3://@B@"
  both s3cmd cp        "${s3c[@]}" put "$work/small.bin" "s3://@B@/@P@dir/small.bin"
  both s3cmd get       "${s3c[@]}" get --force "s3://@B@/@P@dir/small.bin" "$work/s3cmd.@S@"
  both s3cmd sync      "${s3c[@]}" sync "$work/tree/" "s3://@B@/@P@tree/"
  both s3cmd ls        "${s3c[@]}" ls "s3://@B@/@P@tree/"
  both s3cmd multipart "${s3c[@]}" put "$work/big.bin" "s3://@B@/@P@dir/big.bin"
  both s3cmd rm        "${s3c[@]}" rm "s3://@B@/@P@dir/small.bin"
  bucket s3cmd rb      "${s3c[@]}" rb --recursive --force "s3://@B@"
  sweep

  # elbencho --------------------------------------------------------------------------------
  BUCKET_BASE="compat-elbencho-$backend"
  local el=("$BIN/elbencho" --s3endpoints @EP@ --s3key "$AK" --s3secret "$SK" --s3region "$REGION")
  # elbencho names its own objects; in an existing bucket they go under the tool's prefix.
  [ -z "$EXISTING_BUCKET" ] || el+=(--s3objprefix "@P@")
  bucket elbencho mb      "${el[@]}" -d "@B@"
  both elbencho cp        "${el[@]}" -w -t 2 -n 1 -N 4 -s 1m -b 1m "@B@"
  both elbencho get       "${el[@]}" -r -t 2 -n 1 -N 4 -s 1m -b 1m "@B@"
  both elbencho multipart "${el[@]}" -w -t 1 -n 1 -N 1 -s 12m -b 6m "@B@"
  both elbencho ls        "${el[@]}" --s3listobj 1000 "@B@"
  both elbencho rm        "${el[@]}" -F -t 2 -n 1 -N 4 "@B@"
  bucket elbencho rb      "${el[@]}" -D "@B@"
  sweep
}

case "${1:-}" in
  fetch) fetch ;;
  run) shift; run "$@" ;;
  *) echo "usage: $0 fetch | run <backend> <direct-endpoint> <via-endpoint> <region> <ak> <sk> <results-file> [existing-bucket]" >&2; exit 2 ;;
esac
