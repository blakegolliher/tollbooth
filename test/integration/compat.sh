#!/usr/bin/env bash
# Byte-equivalence, compatibility, and throttling runs against the pinned backends (test plan
# categories 5, 6, and 7): bring the backends up, run tollbooth in front of each, run test/s3diff
# direct versus via (stage 1), the client-tool matrix through the proxy (stage 2, clients.sh), and
# the client-tool throttling rows (stage 3, throttle.sh, Garage only), then write docs/COMPAT.md and
# docs/THROTTLE.md from every cell.
#
#   compat.sh                 run everything, full logs under run/compat/
#   BACKENDS="garage"         restrict to one backend
#   STAGES="s3diff"           restrict to one stage (s3diff, clients, throttle)
#
# Against a cluster that already exists (test plan category 6: "and against a VAST cluster in the
# lab, manual, results committed"):
#
#   BACKENDS=vast VAST_ENDPOINT=host:port VAST_BUCKET=<existing bucket> \
#     AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=… compat.sh
#
# runs the s3diff and client stages through tollbooth in front of that endpoint and writes
# docs/COMPAT-vast.md (docs/COMPAT.md stays CI's). With VAST_BUCKET no bucket is created or
# deleted: everything is written under the key prefixes s3diff/ and compat-<tool>-vast-direct/ and
# -via/ inside that bucket and removed afterwards, the make-bucket and remove-bucket cells are not
# run, and nothing else in the bucket is named. Without VAST_BUCKET the run CREATES AND DELETES
# BUCKETS (s3diff-vast, compat-<tool>-vast-direct and -via), which the credential must be allowed
# to do. Plaintext HTTP, path-style addressing only (VAST_REGION, default us-east-1). The throttle
# stage is not run there: it measures the tools, and Garage is enough for that.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
RUN=$ROOT/run/compat
BIN=$ROOT/bin/tollbooth
mkdir -p "$RUN"
BACKENDS=${BACKENDS:-garage versitygw}
STAGES=${STAGES:-s3diff clients throttle}
THROTTLE_BACKEND=${THROTTLE_BACKEND:-garage} # the tools' behavior is under test, not the backend's; one is enough
export GOTOOLCHAIN=${GOTOOLCHAIN:-auto}

# A run against an existing cluster says what it needs before anything is downloaded or started.
case " $BACKENDS " in *" vast "*)
  if [ -z "${VAST_ENDPOINT:-}" ] || [ -z "${AWS_ACCESS_KEY_ID:-}" ] || [ -z "${AWS_SECRET_ACCESS_KEY:-}" ]; then
    cat >&2 <<'USAGE'
compat: a run against an existing cluster needs three things in the environment:

  export AWS_ACCESS_KEY_ID=<a key that may read, write, and delete objects in the bucket>
  export AWS_SECRET_ACCESS_KEY=<its secret>
  VAST_ENDPOINT=<host or address>:<port> VAST_BUCKET=<an existing bucket> make compat-vast

VAST_ENDPOINT is the cluster's S3 endpoint over plaintext HTTP. With VAST_BUCKET the run writes
and removes only keys under s3diff/ and compat-<tool>-vast-direct/ and -via/ in that bucket.
Without it the run creates and deletes buckets of its own, which the key must be allowed to do.

Nothing was downloaded, started, or sent.
USAGE
    for v in VAST_ENDPOINT AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY; do [ -n "${!v:-}" ] || echo "compat: $v is not set" >&2; done
    exit 2
  fi ;;
esac

# A client tool that cannot be fetched becomes FAIL cells, not an aborted run: stage 1 still runs.
case " $STAGES " in *" clients "*|*" throttle "*) "$HERE/clients.sh" fetch || echo "compat: some client tools are missing; their cells will fail" >&2 ;; esac
# The pinned local backends are started only when one of them is asked for; an existing cluster
# (vast) is used as it is.
local_backends=0
for b in $BACKENDS; do case $b in garage|versitygw) local_backends=1 ;; esac; done
if [ "$local_backends" = 1 ]; then
  "$HERE/backends.sh" up
  trap '"$HERE/backends.sh" down; pkill -f "tollbooth serve --config $RUN/" 2>/dev/null || true' EXIT
  # shellcheck disable=SC1091
  eval "$("$HERE/backends.sh" env)"
else
  trap 'pkill -f "tollbooth serve --config $RUN/" 2>/dev/null || true' EXIT
fi

rc=0
for b in $BACKENDS; do
  case $b in
    garage)    ep=127.0.0.1:3900; region=garage;    ak=$GARAGE_ACCESS_KEY; sk=$GARAGE_SECRET ;;
    versitygw) ep=127.0.0.1:7070; region=us-east-1; ak=$VGW_ACCESS_KEY;    sk=$VGW_SECRET ;;
    vast)
      ep=${VAST_ENDPOINT:?set VAST_ENDPOINT=host:port, the S3 endpoint of the cluster, plaintext HTTP}
      region=${VAST_REGION:-us-east-1}
      ak=${AWS_ACCESS_KEY_ID:?set AWS_ACCESS_KEY_ID to a key that may create and delete buckets on the cluster}
      sk=${AWS_SECRET_ACCESS_KEY:?set AWS_SECRET_ACCESS_KEY}
      if [ -n "${VAST_BUCKET:-}" ]; then
        echo "compat: vast: using the existing bucket $VAST_BUCKET on $ep: keys under s3diff/ and compat-<tool>-vast-{direct,via}/ are written and removed; no bucket is created or deleted"
        how="inside an existing bucket under prefixes of the run's own, so the make-bucket and remove-bucket cells were not run"
      else
        echo "compat: vast: this run creates and deletes the buckets s3diff-vast and compat-<tool>-vast-{direct,via} on $ep"
        how="with buckets of the run's own"
      fi
      # What the cluster says it is, and when and how it was run, for the note under the table.
      server=$(curl -s -m 10 -o /dev/null -D - -I "http://$ep/" | tr -d '\r' | awk -F': ' 'tolower($1)=="server" {print $2}' | head -1) || true
      printf 'The `vast` column: run by hand on %s against a cluster answering `Server: %s`, path-style addressing over plaintext HTTP, %s. Tools: %s.\n' \
        "$(date -u +%F)" "${server:-unknown}" "$how" "$(aws --version 2>&1 | cut -d' ' -f1), the others as pinned in clients.sh" > "$RUN/note-vast.txt" ;;
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
  # An existing cluster is addressed path-style only (tb.example.com is not a domain it serves) and,
  # given a bucket, inside that bucket under a prefix of the run's own.
  existing=""; s3diff_extra=()
  if [ "$b" = vast ]; then
    existing=${VAST_BUCKET:-}
    s3diff_extra=(-styles path)
    [ -z "$existing" ] || s3diff_extra+=(-existing-bucket "$existing")
  fi
  case " $STAGES " in *" s3diff "*)
    if AWS_ACCESS_KEY_ID=$ak AWS_SECRET_ACCESS_KEY=$sk go run "$ROOT/test/s3diff" \
         -direct "http://tb.example.com" -direct-addr "$ep" -via "http://tb.example.com" -via-addr "$listen" \
         -domain tb.example.com -region "$region" -bucket "s3diff-$b" -sizes 0,1,4096,1048576,67108864 \
         "${s3diff_extra[@]}" > "$RUN/s3diff-$b.log" 2>&1; then
      echo "compat: $b: s3diff ok ($(tail -1 "$RUN/s3diff-$b.log"))"
      printf 'aws-sdk-go-v2\ts3diff\tPASS\t%s\n' "$(tail -1 "$RUN/s3diff-$b.log")" > "$RUN/results-s3diff-$b.tsv"
    else
      echo "compat: $b: s3diff FAILED ($(tail -1 "$RUN/s3diff-$b.log")); see $RUN/s3diff-$b.log" >&2
      printf 'aws-sdk-go-v2\ts3diff\tFAIL\t%s\n' "$(tail -1 "$RUN/s3diff-$b.log")" > "$RUN/results-s3diff-$b.tsv"
      rc=1
    fi ;;
  esac
  case " $STAGES " in *" clients "*)
    if "$HERE/clients.sh" run "$b" "http://$ep" "http://$listen" "$region" "$ak" "$sk" "$RUN/results-clients-$b.tsv" "$existing" > "$RUN/clients-$b.log" 2>&1; then :; fi
    fails=$(awk -F'\t' '$3=="FAIL"' "$RUN/results-clients-$b.tsv" | wc -l)
    gaps=$(awk -F'\t' '$3=="GAP"' "$RUN/results-clients-$b.tsv" | wc -l)
    echo "compat: $b: clients $(wc -l < "$RUN/results-clients-$b.tsv") cells, $fails failures, $gaps backend gaps"
    [ "$fails" -eq 0 ] || rc=1 ;;
  esac
  case " $STAGES " in *" throttle "*)
    if [ "$b" = "$THROTTLE_BACKEND" ] && [ "$b" != vast ]; then
      if "$HERE/throttle.sh" run "$b" "http://$ep" "http://$listen" "$admin" "$region" "$ak" "$sk" "$cfg" "$RUN/results-throttle-$b.tsv" > "$RUN/throttle-$b.log" 2>&1; then :; else
        echo "compat: $b: throttle stage exited non-zero; see $RUN/throttle-$b.log" >&2; rc=1
      fi
      cat "$RUN/throttle-$b.log"
    fi ;;
  esac
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
done

# docs/COMPAT.md: tool × operation, one column per backend, from every results file. A run against
# an existing cluster alone writes docs/COMPAT-vast.md and leaves CI's file as it is.
compat_out=$ROOT/docs/COMPAT.md
[ "$local_backends" = 1 ] || compat_out=$ROOT/docs/COMPAT-vast.md
python3 - "$RUN" "$compat_out" $BACKENDS <<'PY'
import glob, os, sys
import re
run, out, backends = sys.argv[1], sys.argv[2], sys.argv[3:]


def clean(detail):
    """A cell's detail as one short line of text: a tool that prints the bytes it could not parse
    (an object's content where it expected XML) must not put them in the table."""
    detail = re.sub(r"(\\x[0-9a-fA-F]{2}|\\[tnr\\])+", "…", detail)
    if detail.count("…") > 3:
        detail = detail[:detail.index("…")] + "… (binary data)"
    detail = " ".join(detail.replace("|", "/").split())
    return detail if len(detail) <= 200 else detail[:200] + "…"


cells = {}  # (tool, op) -> {backend: (status, detail)}
for b in backends:
    for f in glob.glob(os.path.join(run, f"results-s3diff-{b}.tsv")) + glob.glob(os.path.join(run, f"results-clients-{b}.tsv")):
        for line in open(f):
            parts = line.rstrip("\n").split("\t")
            if len(parts) < 3:
                continue
            tool, op, status = parts[0], parts[1], parts[2]
            detail = clean(parts[3]) if len(parts) > 3 else ""
            cells.setdefault((tool, op), {})[b] = (status, detail)
tools = []
for tool, _ in cells:
    if tool not in tools:
        tools.append(tool)
lines = ["# Client compatibility matrix", "",
         "<!-- GENERATED by test/integration/compat.sh from run/compat/results-*.tsv. Do not edit. -->", "",
         "Every client tool runs its operations twice, direct to the backend and through tollbooth, in front of each backend "
         "(test plan category 6). PASS: the operation succeeded through the proxy. FAIL: it succeeded direct and failed through "
         "the proxy, a transparency defect, with the last error line. GAP: it failed both ways identically, a backend limitation "
         "the proxy relayed faithfully, with the backend's words. Backends run as pinned static binaries "
         "(test/integration/backends.sh); tools are pinned in clients.sh. A `vast` column is a run by hand against an "
         "existing VAST cluster (`BACKENDS=vast`, see compat.sh).", "",
         "| Tool | Operation | " + " | ".join(backends) + " |",
         "| --- | --- | " + " | ".join("---" for _ in backends) + " |"]
total = fails = gaps = 0
for tool in tools:
    for (t, op), per in cells.items():
        if t != tool:
            continue
        row = []
        for b in backends:
            st, detail = per.get(b, ("—", ""))
            total += st in ("PASS", "FAIL", "GAP")
            fails += st == "FAIL"
            gaps += st == "GAP"
            row.append("PASS" if st == "PASS" else (f"{st}: {detail}" if st in ("FAIL", "GAP") else st))
        lines.append(f"| {tool} | {op} | " + " | ".join(row) + " |")
lines += ["", f"{total} cells, {fails} failures, {gaps} backend gaps.", ""]
for b in backends:
    note = os.path.join(run, f"note-{b}.txt")
    if os.path.exists(note):
        lines += [open(note).read().strip(), ""]
open(out, "w").write("\n".join(lines))
print(f"compat: wrote {out} ({total} cells, {fails} failures)")
PY

# docs/THROTTLE.md: one row per tool and retry mode from the throttle stage (never from a run
# against an existing cluster alone, which has no throttle stage).
if [ "$local_backends" = 1 ] && compgen -G "$RUN/results-throttle-*.tsv" > /dev/null; then
python3 - "$RUN" "$ROOT/docs/THROTTLE.md" $BACKENDS <<'PY'
import glob, os, sys
run, out = sys.argv[1], sys.argv[2]
rows = []
for f in sorted(glob.glob(os.path.join(run, "results-throttle-*.tsv"))):
    backend = os.path.basename(f)[len("results-throttle-"):-len(".tsv")]
    for line in open(f):
        p = line.rstrip("\n").split("\t")
        if len(p) < 11:
            continue
        rows.append((backend, p))
lines = ["# Client throttling behavior", "",
         "<!-- GENERATED by test/integration/compat.sh (throttle.sh) from run/compat/results-throttle-*.tsv. Do not edit. -->", "",
         "Each client tool, with its default retry settings (and, for aws cli and boto3, also in adaptive retry mode), syncs "
         "objects through tollbooth against a rule whose rate is half the tool's concurrency and whose burst equals it "
         "(test plan category 7; test/integration/throttle.sh). *Landed* is counted direct at the backend after the run; "
         "*denied* is the rule's counter from `GET /admin/limits`; *floor* is `(objects − burst) / rate`, the least time the "
         "rule allows. A tool *completes* when it exits 0 and every object landed. s3cmd uploads sequentially, so its row "
         "is 40 objects under a 1 req/s rule.", "",
         "| Backend | Tool | Retry mode | Objects | Concurrency | Rule rate / burst | Landed | Completed | Denied attempts | Seconds (floor) | Last line |",
         "| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |"]
complete = 0
for backend, p in rows:
    tool, mode, objects, conc, rate, burst, landed, rc, secs, floor, denied = p[:11]
    detail = p[11] if len(p) > 11 else ""
    done = rc == "0" and landed == objects
    complete += done
    lines.append(f"| {backend} | {tool} | {mode} | {objects} | {conc} | {rate} / {burst} | {landed} | {'yes' if done else 'no (exit ' + rc + ')'} | {denied} | {secs} ({floor}) | {detail.replace('|', '/')} |")
lines += ["", f"{len(rows)} rows, {complete} completed.", ""]
open(out, "w").write("\n".join(lines))
print(f"compat: wrote {out} ({len(rows)} rows, {complete} completed)")
PY
fi
exit $rc
