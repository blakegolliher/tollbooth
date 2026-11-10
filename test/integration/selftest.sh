#!/usr/bin/env bash
# What the client matrix may do to a bucket that is not its own. clients.sh is run against
# stand-ins for every tool that only write down their arguments, once with buckets of its own and
# once inside an existing bucket, and the commands are checked:
#
#   - in an existing bucket no command makes or removes a bucket;
#   - every path it names there lies under compat-<tool>-<backend>-direct/ or -via/;
#   - what it deletes at the end is those prefixes and nothing else;
#   - with buckets of its own, the make-bucket and remove-bucket cells are still run.
#
# Nothing is sent anywhere. `make compat` and `make compat-vast` run this first: a change to
# clients.sh that could reach outside its prefixes on someone's cluster fails here.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
mkdir -p "$T/path" "$T/bin" "$T/venv/bin"
cat > "$T/stub" <<'STUB'
#!/usr/bin/env bash
printf '%s' "$(basename "$0")" >> "$STUB_LOG"
[ -z "${BUCKET:-}" ] || printf ' [BUCKET=%s PREFIX=%s]' "$BUCKET" "${PREFIX:-}" >> "$STUB_LOG"
for a in "$@"; do printf ' %s' "$a" >> "$STUB_LOG"; done
printf '\n' >> "$STUB_LOG"
STUB
chmod +x "$T/stub"
cp "$T/stub" "$T/path/aws"
for t in s5cmd rclone elbencho; do cp "$T/stub" "$T/bin/$t"; done
cp "$T/stub" "$T/venv/bin/python"; cp "$T/stub" "$T/venv/bin/s3cmd"

matrix() { # matrix LOG [existing-bucket]
  STUB_LOG=$1 COMPAT_BIN=$T/bin COMPAT_VENV=$T/venv PATH=$T/path:$PATH \
    "$HERE/clients.sh" run vast http://direct.invalid http://via.invalid us-east-1 AK SK "$1.tsv" "${2:-}" > "$1.out" 2>&1
}
fail() { echo "selftest: FAIL: $*" >&2; exit 1; }
bucket_ops='(^| )(mb|rb|mkdir|purge)( |$)| -[dD] [a-z]'

: > "$T/own.log"; matrix "$T/own.log"
[ "$(grep -cE "$bucket_ops" "$T/own.log")" -ge 20 ] || fail "with buckets of its own the matrix no longer makes and removes them"

B=someone-elses-bucket
: > "$T/existing.log"; matrix "$T/existing.log" "$B"
if grep -nE "$bucket_ops" "$T/existing.log"; then fail "a command makes or removes a bucket in an existing bucket"; fi
# Every mention of the bucket in a path is followed by one of the run's own prefixes.
if grep -oE "(s3://|:s3:)$B[^ \"]*" "$T/existing.log" | grep -vE "^(s3://|:s3:)$B/compat-[a-z0-9]+-vast-(direct|via)/"; then
  fail "a path in the existing bucket lies outside the run's prefixes"
fi
if grep -oE -- "--bucket $B --key [^ ]*" "$T/existing.log" | grep -vE -- "--key compat-[a-z0-9]+-vast-(direct|via)/"; then
  fail "a key in the existing bucket lies outside the run's prefixes"
fi
# The bucket by itself appears only where the tool takes the prefix separately: elbencho's object
# prefix, boto3's environment, and the aws cli's --bucket with a --key checked above.
if grep -E "(^| )$B( |$)" "$T/existing.log" \
     | grep -vE "^elbencho .*--s3objprefix compat-elbencho-vast-(direct|via)/ " \
     | grep -vE "^python \[BUCKET=$B PREFIX=compat-boto3-vast-(direct|via)/\]" \
     | grep -vE -- "--bucket $B --key compat-awscli-vast-(direct|via)/"; then
  fail "the existing bucket is named without a prefix of the run's own"
fi
if grep -E '^rclone ' "$T/existing.log" | grep -v -- '--s3-no-check-bucket'; then fail "rclone would try to make the bucket"; fi
[ "$(grep -cE "^aws .* s3 rm s3://$B/compat-[a-z0-9]+-vast-(direct|via)/ --recursive" "$T/existing.log")" -eq 12 ] || fail "the end-of-tool sweeps are not the twelve prefixes"
# s3cmd's `get --force` overwrites the local copy; every other recursive or forced command must be a sweep.
if grep -E ' --recursive| --force' "$T/existing.log" \
     | grep -vE "^aws .* s3 rm s3://$B/compat-[a-z0-9]+-vast-(direct|via)/ --recursive --quiet$" \
     | grep -vE "^s3cmd -c [^ ]+ get --force s3://$B/compat-s3cmd-vast-(direct|via)/"; then
  fail "a recursive or forced delete other than the sweeps"
fi
echo "selftest: ok ($(wc -l < "$T/own.log") commands with buckets of its own, $(wc -l < "$T/existing.log") in an existing bucket, all inside the run's prefixes)"
