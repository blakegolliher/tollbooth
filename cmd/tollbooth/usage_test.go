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

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blakegolliher/tollbooth/internal/showback"
)

// usageDir writes two nodes' sealed hours and one open snapshot by hand.
func usageDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(path, body string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := hourOf(2026, 11, 9, 11) // 2026-11-09T11:00:00Z
	hdr := func(node string, hour int64, rows, reqs, in, out int, partial bool) string {
		return `{"schema_version":1,"node_id":"` + node + `","hour_utc":` + itoa(hour) + `,"rows":` + itoa(int64(rows)) + `,"requests":` + itoa(int64(reqs)) + `,"bytes_in":` + itoa(int64(in)) + `,"bytes_out":` + itoa(int64(out)) + `,"partial":` + boolStr(partial) + `,"recovered":false,"written_at":0}` + "\n"
	}
	row := func(node string, hour int64, ak, bucket, op, sc string, reqs, in, out int) string {
		return `{"hour_utc":` + itoa(hour) + `,"node_id":"` + node + `","access_key":"` + ak + `","bucket":"` + bucket + `","prefix":"","operation":"` + op + `","status_class":"` + sc + `","requests":` + itoa(int64(reqs)) + `,"bytes_in":` + itoa(int64(in)) + `,"bytes_out":` + itoa(int64(out)) + "}\n"
	}
	write(showback.SealedPath(dir, "a", h), hdr("a", h, 2, 30, 300, 3000, false)+row("a", h, "AKIA1", "b1", "GetObject", "2xx", 20, 0, 3000)+row("a", h, "AKIA2", "b2", "PutObject", "2xx", 10, 300, 0))
	write(showback.SealedPath(dir, "a", h+1), hdr("a", h+1, 1, 5, 0, 0, false)+row("a", h+1, "AKIA1", "b1", "GetObject", "throttled", 5, 0, 0))
	write(showback.SealedPath(dir, "b", h), hdr("b", h, 1, 7, 0, 700, false)+row("b", h, "AKIA1", "b1", "GetObject", "2xx", 7, 0, 700))
	write(showback.CurrentPath(dir, "b", h+2), hdr("b", h+2, 1, 1, 0, 1, true)+row("b", h+2, "AKIA3", "b1", "GetObject", "2xx", 1, 0, 1))
	return dir
}

func itoa(i int64) string { return strconv.FormatInt(i, 10) }

func hourOf(y int, m time.Month, d, hh int) int64 {
	return time.Date(y, m, d, hh, 0, 0, 0, time.UTC).Unix() / 3600
}

func fmtInt(i int64) string { return strconv.FormatInt(i, 10) }

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestUsageQuery(t *testing.T) {
	dir := usageDir(t)
	rc, out, errOut := runOut(t, "usage", "query", dir)
	if rc != 0 {
		t.Fatalf("rc %d: %s", rc, errOut)
	}
	if !strings.Contains(out, "REQUESTS") || !strings.Contains(out, "4 files, 1 groups, 43 requests, 300 bytes in, 3701 bytes out; includes the open hour (partial)") {
		t.Fatalf("%s", out)
	}
	rc, out, _ = runOut(t, "usage", "query", "--group-by", "access_key,status_class", "--where", "bucket=b1", dir)
	if rc != 0 || !strings.Contains(out, "ACCESS_KEY  STATUS_CLASS") || !strings.Contains(out, "AKIA1       2xx") || !strings.Contains(out, "AKIA1       throttled") || !strings.Contains(out, "AKIA3") || strings.Contains(out, "AKIA2") {
		t.Fatalf("rc %d\n%s", rc, out)
	}
	rc, out, _ = runOut(t, "usage", "query", "--from", "2026-11-09T11:00:00Z", "--to", "2026-11-09T12:00:00Z", "--group-by", "node_id", "--format", "json", dir)
	var res showback.Result
	if rc != 0 || json.Unmarshal([]byte(out), &res) != nil {
		t.Fatalf("json rc %d\n%s", rc, out)
	}
	// Only the hours named by the range: the two "a"/"b" files of hour h land if h is 11:00Z.
	if res.Files == 0 || res.Partial {
		t.Fatalf("%+v", res)
	}
	rc, out, _ = runOut(t, "usage", "query", "--group-by", "bucket", "--format", "csv", dir)
	if rc != 0 || !strings.HasPrefix(out, "bucket,requests,bytes_in,bytes_out,partial\nb1,33,0,3701,true\nb2,10,300,0,false\n") {
		t.Fatalf("csv rc %d\n%s", rc, out)
	}
	if rc, _, errOut := runOut(t, "usage", "query", "--group-by", "prefix,source_ip", dir); rc != 1 || !strings.Contains(errOut, `"source_ip" was not recorded`) {
		t.Fatalf("absent dimension: rc %d %q", rc, errOut)
	}
	if rc, _, errOut := runOut(t, "usage", "query", "--from", "yesterday", dir); rc != 1 || !strings.Contains(errOut, "RFC 3339") {
		t.Fatalf("bad time: rc %d %q", rc, errOut)
	}
	if rc, _, _ := runOut(t, "usage", "query", filepath.Join(dir, "nope")); rc != 1 {
		t.Fatal("missing path")
	}
	// No path: showback.dir from the config file.
	cfg := filepath.Join(t.TempDir(), "tollbooth.yaml")
	if err := os.WriteFile(cfg, []byte("node_id: a\nbackend: {scheme: http, endpoints: [h:1]}\nshowback: {dir: "+dir+"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rc, out, _ := runOut(t, "usage", "query", "--config", cfg); rc != 0 || !strings.Contains(out, "43 requests") {
		t.Fatalf("config path: rc %d\n%s", rc, out)
	}
	if rc, _, _ := runOut(t, "usage", "query", "--config", filepath.Join(dir, "missing.yaml")); rc != 1 {
		t.Fatal("no path and no config")
	}
}

func TestUsageTop(t *testing.T) {
	dir := usageDir(t)
	rc, out, _ := runOut(t, "usage", "top", "--by", "access_key", "--n", "2", dir)
	if rc != 0 {
		t.Fatalf("rc %d\n%s", rc, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.HasPrefix(lines[1], "AKIA1") || !strings.HasPrefix(lines[2], "AKIA2") || strings.Contains(out, "AKIA3") {
		t.Fatalf("order and cut:\n%s", out)
	}
	rc, out, _ = runOut(t, "usage", "top", "--by", "bucket", "--format", "json", dir)
	var res showback.Result
	if rc != 0 || json.Unmarshal([]byte(out), &res) != nil || res.Groups[0].Keys["bucket"] != "b1" || res.Groups[0].Requests != 33 {
		t.Fatalf("rc %d\n%s", rc, out)
	}
}

func TestUsageVerify(t *testing.T) {
	dir := usageDir(t)
	rc, out, errOut := runOut(t, "usage", "verify", dir)
	if rc != 0 || !strings.Contains(out, "4 files, 2 nodes, 0 problems") {
		t.Fatalf("rc %d\n%s%s", rc, out, errOut)
	}
	// a has hours h and h+1; remove h+1 and add h+2 to create a hole.
	h := hourOf(2026, 11, 9, 11)
	if err := os.Remove(showback.SealedPath(dir, "a", h+1)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(showback.SealedPath(dir, "a", h+2), []byte(`{"schema_version":1,"node_id":"a","hour_utc":`+fmtInt(h+2)+`,"rows":0,"requests":0,"bytes_in":0,"bytes_out":0,"partial":false,"recovered":true,"written_at":0}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rc, out, errOut = runOut(t, "usage", "verify", dir)
	if rc != 1 || !strings.Contains(out, "1 problems") || !strings.Contains(out, showback.HourString(h+1)) || !strings.Contains(errOut, "1 problems") {
		t.Fatalf("rc %d\n%s%s", rc, out, errOut)
	}
	rc, out, _ = runOut(t, "usage", "verify", "--format", "json", dir)
	var rep showback.VerifyReport
	if rc != 1 || json.Unmarshal([]byte(out), &rep) != nil || rep.Problems != 1 || len(rep.Nodes[0].Missing) != 1 || len(rep.Nodes[0].Recovered) != 1 {
		t.Fatalf("rc %d\n%s", rc, out)
	}
	if rc, _, _ := runOut(t, "usage"); rc != 2 {
		t.Fatal("usage alone is usage")
	}
	if rc, _, _ := runOut(t, "usage", "bogus"); rc != 2 {
		t.Fatal("unknown verb")
	}
	if rc, out, _ := runOut(t, "usage", "help"); rc != 0 || !strings.Contains(out, "verbs:") {
		t.Fatal("help")
	}
}
