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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// GET /admin/status assembles the summary from the providers; GET /admin/backends lists the rows.
// Both are registered only when their providers are set.
func TestStatusAndBackendsRoutes(t *testing.T) {
	s, lim, c := limitsServer(t)
	s.Token = c.AdminToken()
	s.NodeID, s.Version, s.StartedAt = "n1", "test", time.Now().Add(-90*time.Second)
	s.Ready = func() bool { return true }
	s.Config = func() ConfigView {
		return ConfigView{Config: c.Redacted(), Generation: 3, LoadedAt: time.Now(), LastReload: &ReloadResult{OK: true, Generation: 3, Rules: 2}}
	}
	s.Backends = func() []BackendStatus {
		return []BackendStatus{{Endpoint: "127.0.0.1:1", Up: true, Requests: 10, TTFBMeanMS: 1.5}, {Endpoint: "127.0.0.1:2", Up: false, Errors: 2, ErrorsDial: 2, LastError: "dial tcp: refused", LastErrAt: "2026-11-09T12:00:00Z"}}
	}
	s.Showback = func() (string, int64) { return "2026-11-09T12:00:00Z", 42 }
	s.Metrics.TotalRequests.Store(1000)
	s.Metrics.TotalThrottled.Store(7)
	s.Metrics.InflightCount.Store(3)
	_ = lim
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	authed := func(path string) (int, []byte) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil) //nolint:noctx // test
		req.Header.Set("Authorization", "Bearer secret-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck // test
		var buf []byte
		b := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(b)
			buf = append(buf, b[:n]...)
			if err != nil {
				break
			}
		}
		return resp.StatusCode, buf
	}
	code, body := authed("/admin/status")
	var v StatusView
	if code != 200 || json.Unmarshal(body, &v) != nil {
		t.Fatalf("%d %s", code, body)
	}
	if v.NodeID != "n1" || v.Version != "test" || v.UptimeSeconds < 89 || !v.Ready || v.Inflight != 3 || v.Requests != 1000 || v.Throttled != 7 ||
		v.Rules != 2 || v.Generation != 3 || v.LastReload == nil || v.BackendsUp != 1 || len(v.Backends) != 2 || v.Tuples != 42 || v.ShowbackHour == "" {
		t.Fatalf("%s", body)
	}
	code, body = authed("/admin/backends")
	var bl struct {
		Backends []BackendStatus `json:"backends"`
	}
	if code != 200 || json.Unmarshal(body, &bl) != nil || len(bl.Backends) != 2 || bl.Backends[1].LastError == "" {
		t.Fatalf("%d %s", code, body)
	}
	// Without a backends provider neither route exists.
	bare := &Server{Metrics: NewMetrics(), Config: s.Config}
	srv2 := httptest.NewServer(bare.Handler())
	defer srv2.Close()
	for _, p := range []string{"/admin/status", "/admin/backends"} {
		resp, err := http.Get(srv2.URL + p) //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close() //nolint:errcheck // test
		if resp.StatusCode != 404 {
			t.Errorf("%s without providers: %d", p, resp.StatusCode)
		}
	}
}

// The sampler turns the plain counters into per-second rates over 1, 10, and 60 samples.
func TestSamplerRates(t *testing.T) {
	m := NewMetrics()
	s := &Server{Metrics: m}
	if r := s.sampler.rates(); r.Requests1s != 0 {
		t.Fatal("no samples yet")
	}
	base := time.Now()
	for i := 0; i < 70; i++ {
		m.TotalRequests.Store(int64(i * 100))
		m.TotalThrottled.Store(int64(i * 5))
		s.sampler.take(m)
		s.sampler.ring[(s.sampler.n-1)%len(s.sampler.ring)].at = base.Add(time.Duration(i) * time.Second)
	}
	r := s.sampler.rates()
	if r.Requests1s != 100 || r.Requests10s != 100 || r.Requests60s != 100 || r.Throttled1s != 5 || r.Throttled60s != 5 {
		t.Fatalf("%+v", r)
	}
	// Run returns when the context ends and refuses a second start.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
	s.Run(ctx) // already started: returns at once
}
