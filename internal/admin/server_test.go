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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthReadyMetrics(t *testing.T) {
	ready := false
	s := &Server{Metrics: NewMetrics(), Ready: func() bool { return ready }, Token: "tok", Pprof: true}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	get := func(path, token string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck // test
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/healthz", ""); code != 200 || body != "ok\n" {
		t.Errorf("healthz: %d %q", code, body)
	}
	if code, _ := get("/readyz", ""); code != 503 {
		t.Errorf("readyz before ready: %d", code)
	}
	ready = true
	if code, body := get("/readyz", ""); code != 200 || body != "ready\n" {
		t.Errorf("readyz: %d %q", code, body)
	}
	s.Metrics.Inflight.Set(3)
	if code, body := get("/metrics", ""); code != 200 || !strings.Contains(body, "tollbooth_inflight_requests 3") || !strings.Contains(body, "go_goroutines") {
		t.Errorf("metrics: %d\n%s", code, body[:min(300, len(body))])
	}
	// pprof is on the admin listener but outside /admin/, so it needs no token (it is gated by admin.pprof).
	if code, _ := get("/debug/pprof/", ""); code != 200 {
		t.Errorf("pprof: %d", code)
	}
	// /admin/* needs the token when one is configured; a wrong token is 401 even for an unknown route.
	if code, _ := get("/admin/anything", ""); code != 401 {
		t.Errorf("admin without token: %d", code)
	}
	if code, _ := get("/admin/anything", "wrong"); code != 401 {
		t.Errorf("admin with wrong token: %d", code)
	}
	if code, _ := get("/admin/anything", "tok"); code != 404 {
		t.Errorf("admin with token (no such route yet): %d", code)
	}
	if code, _ := get("/healthz", "wrong"); code != 200 {
		t.Errorf("open routes ignore the token: %d", code)
	}
	off := &Server{Metrics: NewMetrics()}
	srv2 := httptest.NewServer(off.Handler())
	defer srv2.Close()
	resp, err := http.Get(srv2.URL + "/debug/pprof/") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() //nolint:errcheck // test
	if resp.StatusCode != 404 {
		t.Errorf("pprof off must be 404, got %d", resp.StatusCode)
	}
	if hs := s.HTTPServer(); hs.ReadHeaderTimeout == 0 || hs.Handler == nil {
		t.Error("HTTPServer must carry timeouts and the handler")
	}
}

// Every Phase 1 route in the table is served; later-phase routes are listed for the parity test
// but not yet registered.
func TestRouteTable(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Routes {
		key := r.Method + " " + r.Path
		if seen[key] {
			t.Errorf("duplicate route %s", key)
		}
		seen[key] = true
		if r.Phase < 1 || r.Phase > 4 {
			t.Errorf("route %s has phase %d", key, r.Phase)
		}
		if r.Open && strings.HasPrefix(r.Path, "/admin/") {
			t.Errorf("route %s is open but under /admin/", key)
		}
	}
}
