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

package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/blakegolliher/tollbooth/internal/admin"
	"github.com/blakegolliher/tollbooth/internal/showback"
)

// showback.Status mirrors admin.StatusClass value for value (D-048).
func TestShowbackStatusMirrorsStatusClass(t *testing.T) {
	for i := 0; i < 7; i++ {
		if got, want := showback.Status(i).String(), admin.StatusClass(i).String(); got != want {
			t.Errorf("status %d: showback %q, admin %q", i, got, want)
		}
	}
}

// Every completed request is counted once at completion with the bytes actually moved: a
// forwarded GET under 2xx with its body as bytes_out, a denied request under throttled with zero
// bytes, a PUT with the body it sent as bytes_in. Showback off by kill switch counts nothing.
func TestShowbackCountsAtCompletion(t *testing.T) {
	r := rigWithRules(t, frozenRule)
	c := fresh()
	resp, err := c.Get(r.front.URL + "/open/k") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)               //nolint:errcheck // test
	resp.Body.Close()                            //nolint:errcheck // test
	resp, err = c.Get(r.front.URL + "/frozen/k") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()                                                                                                    //nolint:errcheck // test
	req, _ := http.NewRequest(http.MethodPut, r.front.URL+"/open/obj", bytes.NewReader(bytes.Repeat([]byte("x"), 1000))) //nolint:noctx // test
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() //nolint:errcheck // test
	r.waitLog(t, `"operation":"PutObject"`)
	if r.sb.Tuples() != 3 {
		t.Fatalf("tuples %d, want 3 (GET 2xx, GET throttled, PUT 2xx)", r.sb.Tuples())
	}
	dir := t.TempDir()
	st := showback.NewStore(r.sb, dir, "test", 0, 0, nil, nil)
	st.Flush()
	_, rows, err := showback.ReadFile(showback.CurrentPath(dir, "test", r.sb.Hour()))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][3]int64{}
	for i := range rows {
		op, _ := rows[i].Dimension("operation")
		sc, _ := rows[i].Dimension("status_class")
		b, _ := rows[i].Dimension("bucket")
		got[op+"/"+sc+"/"+b] = [3]int64{rows[i].Requests(), rows[i].BytesIn(), rows[i].BytesOut()}
	}
	want := map[string][3]int64{"GetObject/2xx/open": {1, 0, 4096}, "GetObject/throttled/frozen": {1, 0, 0}, "PutObject/2xx/open": {1, 1000, 0}}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: %v, want %v (all rows: %v)", k, got[k], w, got)
		}
	}
	// Kill switch: nothing more is counted.
	cfg := *r.cfg.Load()
	cfg.Kill.ShowbackDisable = true
	r.cfg.Store(&cfg)
	resp, _ = c.Get(r.front.URL + "/open/k2") //nolint:noctx // test
	io.Copy(io.Discard, resp.Body)            //nolint:errcheck // test
	resp.Body.Close()                         //nolint:errcheck // test
	waitLines(t, r, 4)                        // object keys are never logged, so wait for the fourth record
	if r.sb.Tuples() != 3 {
		t.Fatalf("kill switch on, tuples %d", r.sb.Tuples())
	}
}

// The forwarded path with showback on stays at the seven allocations: the increment allocates
// nothing for an existing tuple.
func TestHandlerAllocationsWithShowback(t *testing.T) {
	h, w, req := allocRig(t, "")
	h.Showback = showback.NewAggregator(h.Config.Load(), nil)
	h.ServeHTTP(w, req)
	allocs := testing.AllocsPerRun(2000, func() {
		clear(w.h)
		h.ServeHTTP(w, req)
	})
	t.Logf("handler allocations per forwarded GetObject with showback on: %.2f", allocs)
	if allocs > 7 {
		t.Fatalf("handler allocates %.2f per request with showback; the budget is 7", allocs)
	}
	if h.Showback.Tuples() != 1 {
		t.Fatalf("tuples %d", h.Showback.Tuples())
	}
}

// waitLines waits until the access log holds n records.
func waitLines(t *testing.T, r *rig, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for strings.Count(r.log.String(), `"method"`) < n {
		if time.Now().After(deadline) {
			t.Fatalf("access log never reached %d records:\n%s", n, r.log.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
