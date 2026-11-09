# Showback file schema

Storage is flat files under `showback.dir`, one directory per node, no database. Every file is
NDJSON: a first line of metadata, then one row per line. Files are additive across nodes and
across hours; a cross-node merge is a sum. DuckDB and any warehouse read them directly:

```
duckdb -c "select access_key, bucket, sum(requests) from read_ndjson('usage/**/*.ndjson', union_by_name=true) where operation is not null group by 1, 2"
```

`union_by_name=true` lets the header line (different keys) and the rows share one scan; filtering
on `operation is not null` keeps only rows, since the header has no `operation` key.

## Layout

```
<showback.dir>/
  current/<hour_utc>-<node_id>.ndjson         the open hour; rewritten every flush_interval by atomic rename
  usage/YYYY/MM/DD/HH-<node_id>.ndjson         sealed hours; written once, never reopened
```

`hour_utc` in the `current/` name is the integer hour bucket; `YYYY/MM/DD/HH` in `usage/` is that
same hour rendered in UTC. The node id is in the file name so N nodes can share one directory (or
have their files copied into one) without collision.

## Header (line 1)

```json
{"schema":1,"node_id":"tb-a01","hour_utc":498243,"rows":1842,"requests":9211034,"bytes_in":73108211422,"bytes_out":912034112233,"partial":false,"recovered":false,"written_at":1793494800}
```

| Field | Type | Meaning |
| --- | --- | --- |
| `schema` | int | Schema version, `1`. Bumped only by an ADR. |
| `node_id` | string | The writing node. |
| `hour_utc` | int64 | `floor(unix_seconds / 3600)` of the hour this file covers. |
| `rows` | int64 | Number of row lines that follow. `verify` recomputes it. |
| `requests` | int64 | `sum(requests)` over the rows. `verify` recomputes it. |
| `bytes_in`, `bytes_out` | int64 | Sums over the rows. `verify` recomputes them. |
| `partial` | bool | `true` for a `current/` snapshot (the hour is still open). `false` in every sealed file. |
| `recovered` | bool | `true` when a sealed file was produced at startup from a `current/` snapshot of an earlier hour (the node was down when the hour ended, so up to `flush_interval` of that hour is missing). `verify` lists these hours as recovered. |
| `written_at` | int64 | Unix seconds when the file was written. |

The header has no `operation` key and rows have no `schema` key, so a reader can tell them apart
by either. A sealed hour with no requests is a header with `rows: 0` and no further lines; a
missing file means the node was not running, which `verify` reports as downtime.

## Rows (lines 2..N)

```json
{"hour_utc":498243,"node_id":"tb-a01","access_key":"AKIAEXAMPLE1","bucket":"checkpoints","prefix":"ckpt/","operation":"PutObject","status_class":"2xx","requests":41233,"bytes_in":17099113472,"bytes_out":0}
```

| Column | Type | Present | Meaning |
| --- | --- | --- | --- |
| `hour_utc` | int64 | always | Hour the request **completed** in. Repeated on every row so a row is self-describing once files are concatenated. |
| `node_id` | string | always | Repeated for the same reason. |
| `access_key` | string | when in `showback.dimensions` | From identity; `""` for anonymous. |
| `bucket` | string | when in `dimensions` | `""` for service-level operations. |
| `prefix` | string | when in `dimensions` | Longest matching configured showback prefix for the bucket, or `""` (bucket root). For list operations, matched against the request's `prefix` query parameter (ADR-0001). |
| `source_ip` | string | when in `dimensions` | Text form of the identity's address. |
| `operation` | string | always | Classifier output, including `Unknown`. |
| `status_class` | string | always | `2xx`, `3xx`, `4xx`, `5xx` from the backend; `throttled` (proxy denied); `proxy_error` (backend unreachable, in-flight cap, or proxy fault). |
| `requests` | int64 | always | Count. Each `UploadPart` is one request. |
| `bytes_in` | int64 | always | Request body bytes actually read from the client. Headers excluded. |
| `bytes_out` | int64 | always | Response body bytes actually written to the client. Headers excluded. |

Dimensions not configured are absent from the row, not empty, so a query that groups by an
absent dimension fails loudly instead of returning one `""` group.

### The overflow row

When the open hour reaches `showback.max_tuples_per_hour`, every further *new* tuple folds into
one row per `(operation, status_class)` whose other configured dimensions are the literal string
`__overflow__`. Existing tuples keep counting normally. `showback_collapsed_total` increments once
per folded request; one log line per hour.

## Durability promise

The snapshot is the state. Every `flush_interval` the whole in-memory hour is encoded to
`current/<hour>-<node>.ndjson.tmp` and renamed over the real name, so a crash loses at most one
interval and a restart reads the file back into memory and continues with no double count. At the
hour boundary the old hour has no more writers (attribution is by completion time) and is sealed
at once: final encode, rename into `usage/`, delete from `current/`. No grace period, no reopen
path, no delta merge, nothing to make idempotent. A wall-clock step backward never reopens or
overwrites a sealed file: late rows go to the current hour and `showback_clock_skew_total`
increments.

## Reading

- `tollbooth usage query --from --to --group-by --where --format` filters, groups, and sums over a directory or glob of files from one or many nodes. The open hour is included and marked `partial` in the output.
- `GET /admin/usage?...` does the same over this node's directory, same code path.
- `tollbooth usage verify <dir>` recomputes each header and reports, per node, hours that are missing, partial, recovered, or whose header does not match the rows.
