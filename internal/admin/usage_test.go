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

package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blakegolliher/tollbooth/internal/showback"
)

func TestUsageRoute(t *testing.T) {
	dir := t.TempDir()
	hour := time.Date(2026, 11, 9, 11, 0, 0, 0, time.UTC).Unix() / 3600
	p := showback.SealedPath(dir, "n", hour)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	body := `{"schema_version":1,"node_id":"n","hour_utc":` + itoa(hour) + `,"rows":2,"requests":12,"bytes_in":100,"bytes_out":200,"partial":false,"recovered":false,"written_at":0}` + "\n" +
		`{"hour_utc":` + itoa(hour) + `,"node_id":"n","access_key":"A","bucket":"b1","operation":"GetObject","status_class":"2xx","requests":10,"bytes_in":0,"bytes_out":200}` + "\n" +
		`{"hour_utc":` + itoa(hour) + `,"node_id":"n","access_key":"B","bucket":"b2","operation":"PutObject","status_class":"2xx","requests":2,"bytes_in":100,"bytes_out":0}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{Metrics: NewMetrics(), Usage: &Usage{Dir: dir}}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	code, b := call(t, srv, http.MethodGet, "/admin/usage?group_by=access_key&where=bucket=b1", "")
	var res showback.Result
	if code != 200 || json.Unmarshal(b, &res) != nil || res.Requests != 10 || len(res.Groups) != 1 || res.Groups[0].Keys["access_key"] != "A" {
		t.Fatalf("%d %s", code, b)
	}
	code, b = call(t, srv, http.MethodGet, "/admin/usage?from=2026-11-09T11:00:00Z&to=2026-11-09T12:00:00Z", "")
	if code != 200 || json.Unmarshal(b, &res) != nil || res.Requests != 12 || res.Files != 1 {
		t.Fatalf("%d %s", code, b)
	}
	code, b = call(t, srv, http.MethodGet, "/admin/usage?from=2026-11-09T12:00:00Z", "")
	if code != 200 || json.Unmarshal(b, &res) != nil || res.Requests != 0 {
		t.Fatalf("%d %s", code, b)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin/usage?group_by=bucket&format=csv", nil) //nolint:noctx // test
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	resp.Body.Close() //nolint:errcheck // test
	if resp.Header.Get("Content-Type") != "text/csv" || sb.String() != "bucket,requests,bytes_in,bytes_out,partial\nb1,10,0,200,false\nb2,2,100,0,false\n" {
		t.Fatalf("csv: %q", sb.String())
	}
	for _, bad := range []string{"?from=yesterday", "?to=x", "?where=bucket", "?group_by=key", "?group_by=prefix", "?format=xml"} {
		if code, b := call(t, srv, http.MethodGet, "/admin/usage"+bad, ""); code != 400 || !strings.Contains(string(b), "error") {
			t.Errorf("%s: %d %s", bad, code, b)
		}
	}
}

func itoa(i int64) string {
	b, _ := json.Marshal(i)
	return string(b)
}
