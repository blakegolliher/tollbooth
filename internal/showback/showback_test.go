// Copyright 2026 Blake Golliher
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package showback

import (
	"bytes"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/s3id"
)

// clock is the injected wall clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time)     { c.mu.Lock(); c.t = t; c.mu.Unlock() }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// t0 is 2026-11-09T10:59:59.9Z; hour 10 of that day is bucket h10.
var t0 = time.Date(2026, 11, 9, 10, 59, 59, 900_000_000, time.UTC)

var h10 = t0.Unix() / 3600

func cfg(t testing.TB, dir, tail string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte("node_id: n1\nbackend: {scheme: http, endpoints: [127.0.0.1:1]}\nshowback: {dir: " + dir + tail + "}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func testMetrics() *Metrics {
	return &Metrics{
		Tuples:        prometheus.NewGauge(prometheus.GaugeOpts{Name: "t"}),
		FlushDuration: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "d"}),
		FlushErrors:   prometheus.NewCounter(prometheus.CounterOpts{Name: "e"}),
		Collapsed:     prometheus.NewCounter(prometheus.CounterOpts{Name: "c"}),
		ClockSkew:     prometheus.NewCounter(prometheus.CounterOpts{Name: "s"}),
	}
}

func value(m prometheus.Metric) float64 {
	var out dto.Metric
	_ = m.Write(&out)
	if out.Counter != nil {
		return out.Counter.GetValue()
	}
	return out.Gauge.GetValue()
}

// rig is an aggregator and store on a temp dir with the injected clock at t0.
type rig struct {
	dir string
	cl  *clock
	agg *Aggregator
	st  *Store
	m   *Metrics
	cfg *config.Config
}

func newRig(t testing.TB, tail string) *rig {
	t.Helper()
	dir := t.TempDir()
	return newRigAt(t, dir, "n1", tail, &clock{t: t0})
}

func newRigAt(t testing.TB, dir, node, tail string, cl *clock) *rig {
	t.Helper()
	c := cfg(t, dir, tail)
	c.NodeID = node
	m := testMetrics()
	a := &Aggregator{now: cl.now}
	*a = *NewAggregator(c, m)
	a.now = cl.now
	a.cur.Store(newTable(cl.now().Unix() / 3600))
	st := NewStore(a, dir, node, c.Showback.FlushInterval, c.Showback.Retention(), m, nil)
	st.now = cl.now
	return &rig{dir: dir, cl: cl, agg: a, st: st, m: m, cfg: c}
}

func ident(ak, bucket, key string, op s3id.Op, ip string) *s3id.RequestIdentity {
	id := &s3id.RequestIdentity{AccessKey: ak, Bucket: bucket, Key: key, Op: op, Class: op.Class()}
	switch {
	case key != "":
		id.Level = s3id.LevelObject
	case bucket != "":
		id.Level = s3id.LevelBucket
	}
	if ip != "" {
		id.SourceIP = netip.MustParseAddr(ip)
	}
	return id
}

func get(ak, bucket, key string) *s3id.RequestIdentity {
	return ident(ak, bucket, key, s3id.OpGetObject, "10.0.0.1")
}

// total sums the rows of a file.
func total(t *testing.T, path string) (Header, int64, int64, int64) {
	t.Helper()
	h, rows, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r, in, out int64
	for i := range rows {
		r += rows[i].requests
		in += rows[i].bytesIn
		out += rows[i].bytesOut
	}
	return h, r, in, out
}

// Attribution is by completion: the clock is read inside Record (D-025). A request that started at
// 10:59:59.9 and completes at 11:00:00.1 lands in hour 11, the hour the sealer is working in.
func TestHourIsTheCompletionHour(t *testing.T) {
	r := newRig(t, "")
	r.cl.set(t0.Add(200 * time.Millisecond)) // 11:00:00.1
	r.agg.Record(get("AK", "b", "k"), Status2xx, 10, 20)
	if r.agg.Hour() != h10+1 || r.agg.Tuples() != 1 {
		t.Fatalf("hour %d (want %d), tuples %d", r.agg.Hour(), h10+1, r.agg.Tuples())
	}
	if n := value(r.m.ClockSkew); n != 0 {
		t.Fatalf("skew %v", n)
	}
}

// Crossing the hour seals the old table (no writer can land in it), writes the final file with
// partial false, and the new hour starts empty; an hour with no requests still gets a file.
func TestRolloverSealsExactly(t *testing.T) {
	r := newRig(t, "")
	for i := 0; i < 5; i++ {
		r.agg.Record(get("AK", "b", "k"), Status2xx, 100, 0)
	}
	r.st.Flush() // snapshot of hour 10
	cur := CurrentPath(r.dir, "n1", h10)
	if h, reqs, in, _ := total(t, cur); !h.Partial || reqs != 5 || in != 500 || h.Rows != 1 {
		t.Fatalf("snapshot header %+v reqs %d in %d", h, reqs, in)
	}
	old := r.agg.cur.Load()
	r.cl.set(t0.Add(time.Second)) // 11:00:00.9
	r.st.Flush()                  // seals hour 10, opens hour 11
	sealed := SealedPath(r.dir, "n1", h10)
	if h, reqs, _, _ := total(t, sealed); h.Partial || h.Recovered || reqs != 5 || h.NodeID != "n1" || h.Hour != h10 {
		t.Fatalf("sealed header %+v reqs %d", h, reqs)
	}
	if _, err := os.Stat(cur); !os.IsNotExist(err) {
		t.Fatal("current snapshot of the sealed hour must be removed")
	}
	if r.agg.Hour() != h10+1 || r.agg.Tuples() != 0 {
		t.Fatalf("new hour %d tuples %d", r.agg.Hour(), r.agg.Tuples())
	}
	// A late Record against the old table (clock still says hour 11) cannot land in it.
	for i := range old.shards {
		if !old.shards[i].sealed {
			t.Fatal("old table not sealed")
		}
	}
	// Hour 11 with no requests: cross into hour 12 and the file for 11 exists with zero rows.
	r.cl.set(t0.Add(time.Hour + time.Second))
	r.st.Flush()
	if h, reqs, _, _ := total(t, SealedPath(r.dir, "n1", h10+1)); h.Rows != 0 || reqs != 0 || h.Partial {
		t.Fatalf("empty hour: %+v", h)
	}
}

// The snapshot is the state: a restart in the same hour loads it and continues with no double
// count; a kill between snapshots loses exactly what was not yet snapshotted.
func TestRestartAndKillBetweenSnapshots(t *testing.T) {
	r := newRig(t, "")
	for i := 0; i < 7; i++ {
		r.agg.Record(get("AK", "b", "k"), Status2xx, 1, 2)
	}
	r.st.Flush()
	for i := 0; i < 3; i++ { // after the snapshot, before the next one: lost on a crash
		r.agg.Record(get("AK", "b", "k"), Status2xx, 1, 2)
	}
	// "Crash": a fresh process on the same directory, same hour.
	r2 := newRigAt(t, r.dir, "n1", "", r.cl)
	if err := r2.st.Recover(); err != nil {
		t.Fatal(err)
	}
	if r2.agg.Tuples() != 1 {
		t.Fatalf("tuples after recover %d", r2.agg.Tuples())
	}
	r2.agg.Record(get("AK", "b", "k"), Status2xx, 1, 2)
	r2.agg.Record(get("AK", "b", "other"), Status4xx, 0, 0)
	r2.st.Flush()
	if _, reqs, in, out := total(t, CurrentPath(r.dir, "n1", h10)); reqs != 7+2 || in != 8 || out != 16 {
		t.Fatalf("after restart: requests %d (7 snapshotted + 2 new; the 3 unflushed are lost), in %d out %d", reqs, in, out)
	}
	// Recover twice must not double count either.
	r3 := newRigAt(t, r.dir, "n1", "", r.cl)
	if err := r3.st.Recover(); err != nil {
		t.Fatal(err)
	}
	r3.st.Flush()
	if _, reqs, _, _ := total(t, CurrentPath(r.dir, "n1", h10)); reqs != 9 {
		t.Fatalf("second recover: %d", reqs)
	}
}

// A snapshot from an earlier hour found at startup is sealed as recovered.
func TestRecoverSealsEarlierHour(t *testing.T) {
	r := newRig(t, "")
	r.agg.Record(get("AK", "b", "k"), Status2xx, 5, 0)
	r.st.Flush()
	r.cl.set(t0.Add(2 * time.Hour))
	r2 := newRigAt(t, r.dir, "n1", "", r.cl)
	if err := r2.st.Recover(); err != nil {
		t.Fatal(err)
	}
	h, reqs, in, _ := total(t, SealedPath(r.dir, "n1", h10))
	if h.Partial || !h.Recovered || reqs != 1 || in != 5 {
		t.Fatalf("%+v %d %d", h, reqs, in)
	}
	if _, err := os.Stat(CurrentPath(r.dir, "n1", h10)); !os.IsNotExist(err) {
		t.Fatal("recovered snapshot must be removed from current/")
	}
	if r2.agg.Tuples() != 0 {
		t.Fatal("the open hour must start empty")
	}
	// Another node's snapshot is left alone.
	other := CurrentPath(r.dir, "n2", h10)
	if err := os.WriteFile(other, []byte(`{"schema_version":1,"node_id":"n2","hour_utc":`+fmt.Sprint(h10)+`,"rows":0,"requests":0,"bytes_in":0,"bytes_out":0,"partial":true,"recovered":false,"written_at":0}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r2.st.Recover(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("n2's snapshot was touched")
	}
}

// Past max_tuples_per_hour, new tuples fold into one __overflow__ row per (operation, status);
// existing tuples keep counting; the collapsed counter moves once per folded request.
func TestOverflowFolds(t *testing.T) {
	r := newRig(t, ", max_tuples_per_hour: 2, dimensions: [access_key, bucket, prefix, source_ip, operation, status_class]")
	r.agg.Record(get("A", "b", "k"), Status2xx, 1, 0)
	r.agg.Record(get("B", "b", "k"), Status2xx, 1, 0)
	for i := 0; i < 5; i++ {
		r.agg.Record(ident(fmt.Sprintf("C%d", i), "b", "k", s3id.OpGetObject, "10.0.0.9"), Status2xx, 1, 0)
	}
	r.agg.Record(ident("D", "b", "k", s3id.OpPutObject, "10.0.0.9"), Status5xx, 1, 0) // a different (op, status) overflow row
	r.agg.Record(get("A", "b", "k"), Status2xx, 1, 0)                                 // existing tuple still counts normally
	if n := value(r.m.Collapsed); n != 6 {
		t.Fatalf("collapsed %v, want 6", n)
	}
	if r.agg.Tuples() != 4 {
		t.Fatalf("tuples %d, want 2 real + 2 overflow", r.agg.Tuples())
	}
	r.st.Flush()
	_, rows, err := ReadFile(CurrentPath(r.dir, "n1", h10))
	if err != nil {
		t.Fatal(err)
	}
	seen := make([]string, 0, len(rows))
	for i := range rows {
		ak, _ := rows[i].Dimension("access_key")
		ip, _ := rows[i].Dimension("source_ip")
		op, _ := rows[i].Dimension("operation")
		seen = append(seen, fmt.Sprintf("%s/%s/%s/%d", ak, ip, op, rows[i].requests))
	}
	want := []string{"A/10.0.0.1/GetObject/2", "B/10.0.0.1/GetObject/1", "__overflow__/__overflow__/GetObject/5", "__overflow__/__overflow__/PutObject/1"}
	if strings.Join(seen, " ") != strings.Join(want, " ") {
		t.Fatalf("rows %v\nwant %v", seen, want)
	}
}

// A wall-clock step backward never reopens a sealed hour: the request lands in the open hour and
// the skew counter moves.
func TestClockStepBack(t *testing.T) {
	r := newRig(t, "")
	r.cl.set(t0.Add(time.Second))
	r.st.Flush() // hour 11 open
	r.cl.set(t0) // back to 10:59:59.9
	r.agg.Record(get("AK", "b", "k"), Status2xx, 1, 0)
	if r.agg.Hour() != h10+1 || r.agg.Tuples() != 1 || value(r.m.ClockSkew) != 1 {
		t.Fatalf("hour %d tuples %d skew %v", r.agg.Hour(), r.agg.Tuples(), value(r.m.ClockSkew))
	}
	r.st.Flush()
	// Hour 10 was sealed empty when the clock crossed into 11; the stepped-back clock must not
	// reopen or overwrite it.
	if h, reqs, _, _ := total(t, SealedPath(r.dir, "n1", h10)); h.Rows != 0 || reqs != 0 || h.Partial {
		t.Fatalf("sealed hour 10 changed after the clock stepped back: %+v", h)
	}
	if _, reqs, _, _ := total(t, CurrentPath(r.dir, "n1", h10+1)); reqs != 1 {
		t.Fatalf("the late request must be in the open hour: %d", reqs)
	}
}

// The increment allocates nothing for an existing tuple (docs/DESIGN.md §9 stage 9).
func TestRecordAllocatesZero(t *testing.T) {
	r := newRig(t, ", dimensions: [access_key, bucket, prefix, source_ip, operation, status_class], prefixes: {bkt: [ckpt/, logs/]}")
	id := get("AK", "bkt", "ckpt/x")
	r.agg.Record(id, Status2xx, 1, 1)
	if n := testing.AllocsPerRun(1000, func() { r.agg.Record(id, Status2xx, 1, 1) }); n != 0 {
		t.Fatalf("Record allocates %.2f per call; the budget is 0", n)
	}
}

// Showback prefixes: per bucket, longest match, the listing's prefix parameter for lists
// (ADR-0001), "" for the bucket root, unconfigured buckets, and non-object operations.
func TestPrefixes(t *testing.T) {
	r := newRig(t, ", prefixes: {bkt: [ckpt/, ckpt/deep/, logs/]}")
	lst := ident("A", "bkt", "", s3id.OpListObjectsV2, "")
	lst.ListPrefix = "logs/2026"
	cases := []struct {
		id   *s3id.RequestIdentity
		want string
	}{
		{lst, "logs/"},
		{get("A", "bkt", "ckpt/deep/1"), "ckpt/deep/"},
		{get("A", "bkt", "ckpt/1"), "ckpt/"},
		{get("A", "bkt", "model/1"), ""},
		{get("A", "other", "ckpt/1"), ""},
		{ident("A", "bkt", "", s3id.OpHeadBucket, ""), ""},
	}
	for _, c := range cases {
		if got := r.agg.Prefix(c.id); got != c.want {
			t.Errorf("%s %s/%s prefix %q, want %q", c.id.Op, c.id.Bucket, c.id.Key, got, c.want)
		}
	}
}

// Encode and read round-trip; dimensions not configured are absent, not empty.
func TestEncodeReadRoundTrip(t *testing.T) {
	r := newRig(t, ", dimensions: [bucket, operation, status_class]")
	r.agg.Record(get("AK", "b", "k"), Status2xx, 3, 4)
	r.agg.Record(ident("", "", "", s3id.OpListBuckets, ""), StatusThrottled, 0, 0)
	r.st.Flush()
	b, err := os.ReadFile(CurrentPath(r.dir, "n1", h10))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(`"access_key"`)) || bytes.Contains(b, []byte(`"source_ip"`)) || bytes.Contains(b, []byte(`"prefix"`)) {
		t.Fatalf("unconfigured dimensions must be absent:\n%s", b)
	}
	h, rows, err := Read(bytes.NewReader(b))
	if err != nil || h.Rows != 2 || h.Requests != 2 || h.BytesIn != 3 || h.BytesOut != 4 || !h.Partial || h.SchemaVersion != 1 {
		t.Fatalf("%+v %v", h, err)
	}
	if _, ok := rows[0].Dimension("access_key"); ok {
		t.Fatal("access_key must read as absent")
	}
	// Rows sort by dimensions: the service-level ListBuckets (bucket "") comes first.
	if v, ok := rows[0].Dimension("status_class"); !ok || v != "throttled" {
		t.Fatalf("status %q %v", v, ok)
	}
	if op, _ := rows[0].Dimension("operation"); op != "ListBuckets" {
		t.Fatalf("row order: %s", op)
	}
	if b2 := encode(nil, "n1", h.Hour, rows, r.agg.dims, true, false, time.Unix(h.WrittenAt, 0)); !bytes.Equal(b, b2) {
		t.Fatalf("re-encode differs:\n%s\n%s", b, b2)
	}
	for _, bad := range []string{"", "not json\n", `{"node_id":"x"}` + "\n", `{"schema_version":2,"node_id":"n","hour_utc":1}` + "\n", `{"schema_version":1,"node_id":"n","hour_utc":1}` + "\n" + `{"operation":"Nope","status_class":"2xx"}` + "\n", `{"schema_version":1,"node_id":"n","hour_utc":1}` + "\n" + `{"operation":"GetObject","status_class":"2xx","source_ip":"300.1.1.1"}` + "\n", `{"schema_version":1,"node_id":"n","hour_utc":1}` + "\n" + `{"operation":"GetObject","status_class":"2xx","requests":-1}` + "\n"} {
		if _, _, err := Read(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// Query filters, groups, sums, marks partial, and refuses a dimension a file did not record.
func TestQuery(t *testing.T) {
	r := newRig(t, ", dimensions: [access_key, bucket, operation, status_class]")
	r.agg.Record(get("A", "b1", "k"), Status2xx, 10, 100)
	r.agg.Record(get("A", "b2", "k"), Status2xx, 10, 100)
	r.agg.Record(get("B", "b1", "k"), StatusThrottled, 0, 0)
	r.cl.set(t0.Add(time.Second))
	r.st.Flush() // seals hour 10
	r.agg.Record(get("A", "b1", "k"), Status2xx, 1, 1)
	r.st.Flush() // snapshot of hour 11 (partial)
	files, err := Files(r.dir)
	if err != nil || len(files) != 2 {
		t.Fatalf("%v %v", files, err)
	}
	res, err := Query{GroupBy: []string{"access_key"}}.Run(files)
	if err != nil {
		t.Fatal(err)
	}
	if res.Requests != 4 || res.BytesIn != 21 || !res.Partial || len(res.Groups) != 2 || res.Groups[0].Keys["access_key"] != "A" || res.Groups[0].Requests != 3 || !res.Groups[0].Partial || res.Groups[1].Partial {
		t.Fatalf("%+v", res)
	}
	res, err = Query{From: t0.Truncate(time.Hour), To: t0.Truncate(time.Hour).Add(time.Hour), GroupBy: []string{"bucket", "status_class"}, Where: map[string]string{"access_key": "A"}}.Run(files)
	if err != nil || res.Files != 1 || res.Requests != 2 || len(res.Groups) != 2 || res.Groups[0].Keys["bucket"] != "b1" || res.Partial {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := (Query{GroupBy: []string{"prefix"}}).Run(files); err == nil || !strings.Contains(err.Error(), `"prefix" was not recorded`) {
		t.Fatalf("absent dimension must fail loudly: %v", err)
	}
	if _, err := (Query{GroupBy: []string{"key"}}).Run(files); err == nil {
		t.Fatal("unknown dimension")
	}
	if _, err := (Query{Where: map[string]string{"nope": "x"}}).Run(files); err == nil {
		t.Fatal("unknown where dimension")
	}
	w, err := ParseWhere("bucket=b1, access_key=A")
	if err != nil || w["bucket"] != "b1" || w["access_key"] != "A" {
		t.Fatal(w, err)
	}
	if _, err := ParseWhere("bucket"); err == nil {
		t.Fatal("bad where")
	}
	if _, err := Files(filepath.Join(r.dir, "nope")); err == nil {
		t.Fatal("missing path")
	}
}

// Three nodes' files in one directory sum exactly to the three generators' totals.
func TestMultiNodeSum(t *testing.T) {
	dir := t.TempDir()
	cl := &clock{t: t0}
	var want int64
	for n, count := range map[string]int{"n1": 5, "n2": 7, "n3": 11} {
		r := newRigAt(t, dir, n, "", cl)
		for i := 0; i < count; i++ {
			r.agg.Record(get("A", "b", "k"), Status2xx, 1, 0)
		}
		want += int64(count)
		r.cl.set(t0.Add(time.Second))
		r.st.Flush()
		r.cl.set(t0)
	}
	files, _ := Files(dir)
	res, err := Query{GroupBy: []string{"node_id"}}.Run(files)
	if err != nil || res.Requests != want || len(res.Groups) != 3 {
		t.Fatalf("%+v %v", res, err)
	}
	rep := Verify(files)
	if len(rep.Nodes) != 3 || rep.Problems != 0 {
		t.Fatalf("%+v", rep)
	}
}

// Verify recomputes headers and reports a removed hour as missing, a tampered header as a
// mismatch, and partial and recovered hours.
func TestVerifyDetectsRemovedHour(t *testing.T) {
	r := newRig(t, "")
	for i := 0; i < 3; i++ {
		r.agg.Record(get("A", "b", "k"), Status2xx, 1, 0)
		r.cl.add(time.Hour)
		r.st.Flush()
	}
	files, _ := Files(r.dir)
	rep := Verify(files)
	if rep.Problems != 0 || rep.Nodes[0].Sealed != 3 || len(rep.Nodes[0].Partial) != 1 {
		t.Fatalf("%+v", rep)
	}
	if err := os.Remove(SealedPath(r.dir, "n1", h10+1)); err != nil {
		t.Fatal(err)
	}
	files, _ = Files(r.dir)
	rep = Verify(files)
	if rep.Problems != 1 || len(rep.Nodes[0].Missing) != 1 || rep.Nodes[0].Missing[0] != HourString(h10+1) {
		t.Fatalf("%+v", rep.Nodes[0])
	}
	// Tamper with a header.
	p := SealedPath(r.dir, "n1", h10)
	b, _ := os.ReadFile(p)
	b = bytes.Replace(b, []byte(`"requests":1,`), []byte(`"requests":9,`), 1)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	rep = Verify(files)
	if len(rep.Nodes[0].Mismatch) != 1 || rep.Problems != 2 {
		t.Fatalf("%+v", rep.Nodes[0])
	}
	// An unreadable file is reported, not fatal.
	if err := os.WriteFile(filepath.Join(r.dir, "current", "junk-n1.ndjson"), []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, _ = Files(r.dir)
	rep = Verify(files)
	if len(rep.Nodes[0].Unreadable) != 1 {
		t.Fatalf("%+v", rep.Nodes[0])
	}
}

// A sealed file is never overwritten; retention removes files older than retention_days.
func TestSealedNeverOverwrittenAndRetention(t *testing.T) {
	r := newRig(t, ", retention_days: 1")
	r.agg.Record(get("A", "b", "k"), Status2xx, 1, 0)
	r.st.Flush()
	rows := r.st.rows
	if err := r.st.sealRows(h10, rows, false); err != nil {
		t.Fatal(err)
	}
	if err := r.st.sealRows(h10, rows, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(SealedPath(r.dir, "n1", h10) + ".dup"); err != nil {
		t.Fatal("second seal must land beside the first, not over it")
	}
	// 30 hours later the first file is older than one day.
	r.cl.set(t0.Add(30 * time.Hour))
	r.st.lastSwep = time.Time{}
	r.st.Flush()
	if _, err := os.Stat(SealedPath(r.dir, "n1", h10)); !os.IsNotExist(err) {
		t.Fatal("retention did not delete the old file")
	}
}

// A flush failure counts, logs, and counting continues.
func TestFlushErrorKeepsCounting(t *testing.T) {
	r := newRig(t, "")
	r.st.Dir = filepath.Join(r.dir, "missing-and-unwritable")
	if err := os.WriteFile(r.st.Dir, []byte("a file where the dir should be"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.agg.Record(get("A", "b", "k"), Status2xx, 1, 0)
	r.st.Flush()
	r.agg.Record(get("A", "b", "k"), Status2xx, 1, 0)
	if value(r.m.FlushErrors) != 1 || r.agg.Tuples() != 1 {
		t.Fatalf("errors %v tuples %d", value(r.m.FlushErrors), r.agg.Tuples())
	}
}

// Writers racing a seal: 32 goroutines record across the hour boundary; nothing is lost or
// double counted between the sealed file and the next snapshot.
func TestConcurrentRecordAcrossSeal(t *testing.T) {
	r := newRig(t, "")
	const per = 2000
	var wg sync.WaitGroup
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			id := get(fmt.Sprintf("K%d", g), "b", "k")
			for i := 0; i < per; i++ {
				if i == per/2 && g == 0 {
					r.cl.set(t0.Add(time.Second))
				}
				r.agg.Record(id, Status2xx, 1, 0)
			}
		}(g)
	}
	wg.Wait()
	r.cl.set(t0.Add(time.Second))
	r.st.Flush()
	_, a, _, _ := total(t, SealedPath(r.dir, "n1", h10))
	_, b, _, _ := total(t, CurrentPath(r.dir, "n1", h10+1))
	if a+b != 32*per {
		t.Fatalf("sealed %d + open %d = %d, want %d", a, b, a+b, 32*per)
	}
}

func FuzzRead(f *testing.F) {
	r := newRig(f, ", dimensions: [access_key, bucket, prefix, source_ip, operation, status_class]")
	r.agg.Record(get("AK", "b", "k"), Status2xx, 3, 4)
	r.agg.Record(ident("", "", "", s3id.OpListBuckets, ""), StatusThrottled, 0, 0)
	r.st.Flush()
	b, _ := os.ReadFile(CurrentPath(r.dir, "n1", h10))
	f.Add(b)
	f.Add([]byte(`{"schema_version":1,"node_id":"n","hour_utc":1,"rows":0,"requests":0,"bytes_in":0,"bytes_out":0,"partial":false,"recovered":false,"written_at":0}` + "\n"))
	f.Add([]byte("\n"))
	f.Add([]byte(`{"schema_version":1}` + "\n" + `{"operation":"GetObject","status_class":"2xx","requests":1,"source_ip":"::1"}` + "\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		h, rows, err := Read(bytes.NewReader(data))
		if err != nil {
			return
		}
		// Whatever parsed re-encodes and parses again to the same rows.
		b := encode(nil, h.NodeID, h.Hour, rows, dimAccessKey|dimBucket|dimPrefix|dimSourceIP, h.Partial, h.Recovered, time.Unix(h.WrittenAt, 0))
		h2, rows2, err := Read(bytes.NewReader(b))
		if err != nil || len(rows2) != len(rows) || h2.Hour != h.Hour {
			t.Fatalf("re-read: %v", err)
		}
	})
}

func BenchmarkRecord(b *testing.B) {
	r := newRig(b, ", dimensions: [access_key, bucket, prefix, source_ip, operation, status_class], prefixes: {bkt: [ckpt/, logs/]}")
	id := get("AK", "bkt", "ckpt/x")
	b.ReportAllocs()
	for b.Loop() {
		r.agg.Record(id, Status2xx, 4096, 0)
	}
}

func BenchmarkRecordParallel(b *testing.B) {
	r := newRig(b, "")
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		id := get(fmt.Sprintf("AK%d", b.N%64), "b", "k")
		for pb.Next() {
			r.agg.Record(id, Status2xx, 1, 1)
		}
	})
}

func BenchmarkSnapshot10kTuples(b *testing.B) {
	r := newRig(b, "")
	for i := 0; i < 10_000; i++ {
		r.agg.Record(get(fmt.Sprintf("AK%d", i), "b", "k"), Status2xx, 1, 1)
	}
	b.ReportAllocs()
	for b.Loop() {
		r.st.snapshot(r.agg.cur.Load())
	}
}

// TestTupleMemoryAtCap is the memory row's per-tuple cost (docs/SPEC.md, Performance targets:
// "expect 350 to 450 bytes, not 200") at the default cap of 250,000 tuples, and the snapshot time
// at that size. Runs with TOLLBOOTH_BIG=1; the gate report quotes it.
func TestTupleMemoryAtCap(t *testing.T) {
	if os.Getenv("TOLLBOOTH_BIG") == "" {
		t.Skip("set TOLLBOOTH_BIG=1 to run the 250,000-tuple memory measurement")
	}
	r := newRig(t, ", dimensions: [access_key, bucket, prefix, source_ip, operation, status_class]")
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	const n = 250_000
	for i := 0; i < n; i++ {
		id := ident(fmt.Sprintf("AKIA%06d", i%50_000), fmt.Sprintf("bucket-%d", i%500), "k", s3id.OpGetObject, "")
		id.SourceIP = netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
		r.agg.Record(id, Status2xx, 1, 1)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	perTuple := float64(after.HeapAlloc-before.HeapAlloc) / float64(r.agg.Tuples())
	start := time.Now()
	r.st.Flush()
	flush := time.Since(start)
	st, _ := os.Stat(CurrentPath(r.dir, "n1", h10))
	t.Logf("%d tuples: heap delta %.1f MiB, %.0f bytes per tuple; snapshot (copy, sort, encode, fsync, rename) %v, file %.1f MiB",
		r.agg.Tuples(), float64(after.HeapAlloc-before.HeapAlloc)/(1<<20), perTuple, flush.Round(time.Millisecond), float64(st.Size())/(1<<20))
	if r.agg.Tuples() != n {
		t.Fatalf("tuples %d", r.agg.Tuples())
	}
}
