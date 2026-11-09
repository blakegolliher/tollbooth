#!/usr/bin/env bash
# Fetches the pinned S3 backends used by the integration suite into test/integration/bin/ and
# starts or stops them. No container runtime: both ship as static binaries. Every download is
# verified against the sha256 pinned here, computed on 2026-11-08; versitygw's matches the
# checksums.txt published with its release, Garage publishes none (docs/CONTEXT.md). Both are
# external processes only; none of their code enters this repository (docs/SPEC.md, Licensing).
# MinIO is not used (archived upstream; docs/DECISIONS.md D-022).
#
#   backends.sh fetch            download, verify, and build what is missing
#   backends.sh up               start garage (:3900) and versitygw (:7070) in the background
#   backends.sh down             stop them
#   backends.sh env              print the access keys as shell exports
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
BIN=$HERE/bin
DATA=$HERE/data
mkdir -p "$BIN" "$DATA"

GARAGE_VERSION=v2.5.0
GARAGE_URL="https://garagehq.deuxfleurs.fr/_releases/${GARAGE_VERSION}/x86_64-unknown-linux-musl/garage"
GARAGE_SHA256=2ff667286217cd73ebcb1c03af993822365000441dfe31a80cb64c5189a9dea8

VERSITYGW_VERSION=v1.11.0
VERSITYGW_URL="https://github.com/versity/versitygw/releases/download/${VERSITYGW_VERSION}/versitygw_${VERSITYGW_VERSION}_Linux_x86_64.tar.gz"
VERSITYGW_SHA256=1f075c9dc92fef232f5b3c7465711ddc5e1b062b2b6df4a3bd246a4be5b35b52

fetch_verified() { # url sha256 dest
  local url=$1 sha=$2 dest=$3 tmp
  tmp=$(mktemp "$dest.XXXXXX")
  curl -sfL --retry 3 --max-time 600 -o "$tmp" "$url"
  echo "$sha  $tmp" | sha256sum -c --quiet - || { rm -f "$tmp"; echo "backends: checksum mismatch for $url" >&2; exit 1; }
  mv "$tmp" "$dest"
}

fetch() {
  if [ ! -x "$BIN/garage" ]; then
    fetch_verified "$GARAGE_URL" "$GARAGE_SHA256" "$BIN/garage"; chmod +x "$BIN/garage"
  fi
  if [ ! -x "$BIN/versitygw" ]; then
    fetch_verified "$VERSITYGW_URL" "$VERSITYGW_SHA256" "$BIN/versitygw.tar.gz"
    tar -xzf "$BIN/versitygw.tar.gz" -C "$BIN" --strip-components=1 "versitygw_${VERSITYGW_VERSION}_Linux_x86_64/versitygw"
    rm -f "$BIN/versitygw.tar.gz"
  fi
  echo "backends: garage ${GARAGE_VERSION}, versitygw ${VERSITYGW_VERSION} in $BIN"
}

# Fixed lab credentials. Garage mints its own key on first start; `env` prints what it minted.
VGW_ACCESS=vgwadmin; VGW_SECRET=vgwadminsecret

up() {
  fetch
  mkdir -p "$DATA/garage/meta" "$DATA/garage/data" "$DATA/versitygw"
  if [ ! -f "$DATA/garage/garage.toml" ]; then
    cat > "$DATA/garage/garage.toml" <<TOML
metadata_dir = "$DATA/garage/meta"
data_dir = "$DATA/garage/data"
db_engine = "sqlite"
replication_factor = 1
rpc_bind_addr = "127.0.0.1:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
[s3_api]
s3_region = "garage"
api_bind_addr = "127.0.0.1:3900"
root_domain = ".s3.garage.localhost"
[admin]
api_bind_addr = "127.0.0.1:3903"
admin_token = "$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
TOML
  fi
  "$BIN/garage" -c "$DATA/garage/garage.toml" server > "$DATA/garage.log" 2>&1 & echo $! > "$DATA/garage.pid"
  ROOT_ACCESS_KEY=$VGW_ACCESS ROOT_SECRET_KEY=$VGW_SECRET "$BIN/versitygw" --port 127.0.0.1:7070 posix "$DATA/versitygw" > "$DATA/versitygw.log" 2>&1 & echo $! > "$DATA/versitygw.pid"
  # Garage needs a layout and a key once; idempotent on a reused data dir.
  for _ in $(seq 1 50); do "$BIN/garage" -c "$DATA/garage/garage.toml" status > /dev/null 2>&1 && break; sleep 0.2; done
  node=$("$BIN/garage" -c "$DATA/garage/garage.toml" status | awk '/^[0-9a-f]{16}/ {print $1; exit}')
  "$BIN/garage" -c "$DATA/garage/garage.toml" layout assign -z dc1 -c 1G "$node" > /dev/null 2>&1 || true
  "$BIN/garage" -c "$DATA/garage/garage.toml" layout apply --version 1 > /dev/null 2>&1 || true
  if [ ! -f "$DATA/garage.env" ]; then
    "$BIN/garage" -c "$DATA/garage/garage.toml" key create tollbooth-ci > "$DATA/garage-key.txt"
    {
      echo "export GARAGE_ACCESS_KEY=$(awk '/Key ID/ {print $3}' "$DATA/garage-key.txt")"
      echo "export GARAGE_SECRET=$(awk '/Secret key/ {print $3}' "$DATA/garage-key.txt")"
    } > "$DATA/garage.env"
    # shellcheck disable=SC1091
    . "$DATA/garage.env"
    "$BIN/garage" -c "$DATA/garage/garage.toml" key allow --create-bucket "$GARAGE_ACCESS_KEY" > /dev/null
  fi
  for port in 3900 7070; do
    for _ in $(seq 1 100); do curl -s -o /dev/null "http://127.0.0.1:$port/" && break; sleep 0.2; done
  done
  echo "backends: up (garage :3900, versitygw :7070); logs in $DATA"
}

down() {
  for p in garage versitygw; do
    if [ -f "$DATA/$p.pid" ]; then kill "$(cat "$DATA/$p.pid")" 2>/dev/null || true; rm -f "$DATA/$p.pid"; fi
  done
  echo "backends: down"
}

env_() {
  [ -f "$DATA/garage.env" ] && cat "$DATA/garage.env"
  echo "export VGW_ACCESS_KEY=$VGW_ACCESS VGW_SECRET=$VGW_SECRET"
}

case "${1:-}" in
  fetch) fetch ;;
  up) up ;;
  down) down ;;
  env) env_ ;;
  *) echo "usage: $0 fetch|up|down|env" >&2; exit 2 ;;
esac
