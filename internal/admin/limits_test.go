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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/limiter"
	"github.com/blakegolliher/tollbooth/internal/s3id"
)

const limitsConfig = `node_id: t
backend: {scheme: http, endpoints: [127.0.0.1:1]}
showback: {dir: /tmp/x}
admin: {token: secret-token}
rate_limits:
  rules:
    - id: tenant-a-writes
      match: {access_keys: [AKIAEXAMPLE1], buckets: [training-data], prefixes: [ckpt/], ops: [WRITE]}
      group_by: [access_key, bucket]
      limit: {rate: 2000, burst: 4000}
    - id: frozen
      match: {access_keys: [AKIAREVOKED]}
      limit: {rate: 0}
      on_limit: too_many_requests
`

func limitsServer(t *testing.T) (*Server, *limiter.Limiter, *config.Config) {
	t.Helper()
	c, err := config.Parse([]byte(limitsConfig))
	if err != nil {
		t.Fatal(err)
	}
	m := NewMetrics()
	lim := limiter.New(&m.Limiter)
	if err := lim.Apply(c); err != nil {
		t.Fatal(err)
	}
	return &Server{Metrics: m, Limiter: lim}, lim, c
}

func call(t *testing.T, srv *httptest.Server, method, path, body string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body)) //nolint:noctx // test
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	b, _ := io.ReadAll(resp.Body)
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("%s %s: content type %q", method, path, ct)
	}
	return resp.StatusCode, b
}

func TestLimitsRoutes(t *testing.T) {
	s, lim, _ := limitsServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// Deny a few requests so the counters move.
	id := s3id.RequestIdentity{AccessKey: "AKIAREVOKED", Bucket: "b", Key: "k", Op: s3id.OpGetObject, Class: s3id.ClassRead, Level: s3id.LevelObject}
	for i := 0; i < 3; i++ {
		lim.Decide(&id, lim.Now(time.Now()), true)
	}
	code, body := call(t, srv, http.MethodGet, "/admin/limits", "")
	var lim1 Limits
	if code != 200 || json.Unmarshal(body, &lim1) != nil {
		t.Fatalf("%d %s", code, body)
	}
	if len(lim1.Rules) != 2 || lim1.Rules[0].ID != "tenant-a-writes" || lim1.Rules[0].Rate != 2000 || lim1.Rules[0].EffectiveBurst != 4000 ||
		lim1.Rules[1].Denied != 3 || lim1.Rules[1].OnLimit != "too_many_requests" || lim1.LiveKeys != 0 {
		t.Fatalf("%s", body)
	}

	code, body = call(t, srv, http.MethodGet, "/admin/limits/top?n=5&window=30s", "")
	var top Top
	if code != 200 || json.Unmarshal(body, &top) != nil || top.WindowSeconds != 30 || len(top.Top) != 1 || top.Top[0].Rule != "frozen" || top.Top[0].Denied != 3 {
		t.Fatalf("%d %s", code, body)
	}
	if code, _ := call(t, srv, http.MethodGet, "/admin/limits/top", ""); code != 200 {
		t.Fatalf("defaults: %d", code)
	}
	for _, q := range []string{"?n=0", "?n=x", "?n=20000", "?window=0s", "?window=25h", "?window=soon"} {
		if code, body := call(t, srv, http.MethodGet, "/admin/limits/top"+q, ""); code != 400 || !strings.Contains(string(body), "error") {
			t.Errorf("%s: %d %s", q, code, body)
		}
	}

	// Dry run: a write by tenant A under ckpt/ matches the first rule and is allowed; nothing is
	// consumed; the identity is echoed.
	code, body = call(t, srv, http.MethodPost, "/admin/limits/test", `{"access_key":"AKIAEXAMPLE1","bucket":"training-data","key":"ckpt/1","ip":"10.20.1.1","op":"PutObject"}`)
	var tr TestResponse
	if code != 200 || json.Unmarshal(body, &tr) != nil {
		t.Fatalf("%d %s", code, body)
	}
	if tr.Operation != "PutObject" || tr.Class != "WRITE" || tr.Level != "key" || !tr.Result.Allowed || len(tr.Result.Matches) != 1 ||
		tr.Result.Matches[0].Rule != "tenant-a-writes" || tr.Result.Matches[0].AccessKey != "AKIAEXAMPLE1" || tr.Result.Matches[0].Bucket != "training-data" {
		t.Fatalf("%s", body)
	}
	if lim.Keys() != 0 {
		t.Fatal("the dry run consumed a token")
	}
	code, body = call(t, srv, http.MethodPost, "/admin/limits/test", `{"access_key":"AKIAREVOKED","op":"ListBuckets"}`)
	if code != 200 || json.Unmarshal(body, &tr) != nil || tr.Result.Allowed || tr.Result.DeniedBy != "frozen" || tr.Result.Mode != "too_many_requests" || tr.Level != "service" {
		t.Fatalf("%d %s", code, body)
	}
	// A listing's prefix parameter is what a prefix rule matches (ADR-0001).
	code, body = call(t, srv, http.MethodPost, "/admin/limits/test", `{"access_key":"AKIAEXAMPLE1","bucket":"training-data","prefix":"ckpt/","op":"ListObjectsV2"}`)
	if code != 200 || json.Unmarshal(body, &tr) != nil || tr.Level != "bucket" || len(tr.Result.Matches) != 0 {
		t.Fatalf("a LIST is not a WRITE, so no match: %d %s", code, body)
	}
	for _, bad := range []string{`{}`, `{"op":"Nope"}`, `{"op":"GetObject","ip":"300.1.1.1"}`, `not json`} {
		if code, body := call(t, srv, http.MethodPost, "/admin/limits/test", bad); code != 400 || !strings.Contains(string(body), "error") {
			t.Errorf("%s: %d %s", bad, code, body)
		}
	}
}

// Routes with no provider are not registered: a server without a limiter or reloader answers 404.
func TestPhase2RoutesAbsentWithoutProviders(t *testing.T) {
	s := &Server{Metrics: NewMetrics()}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	for _, r := range []struct{ m, p string }{{"GET", "/admin/limits"}, {"GET", "/admin/limits/top"}, {"POST", "/admin/limits/test"}, {"POST", "/admin/reload"}, {"GET", "/admin/config"}} {
		req, _ := http.NewRequest(r.m, srv.URL+r.p, nil) //nolint:noctx // test
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close() //nolint:errcheck // test
		if resp.StatusCode != 404 {
			t.Errorf("%s %s: %d", r.m, r.p, resp.StatusCode)
		}
	}
}

func TestReloadAndConfigRoutes(t *testing.T) {
	s, _, c := limitsServer(t)
	s.Token = c.AdminToken()
	fail := false
	s.Reload = func() ReloadResult {
		if fail {
			return ReloadResult{Error: errors.New("listen.addres: unknown key").Error(), At: time.Now(), Generation: 2}
		}
		return ReloadResult{OK: true, At: time.Now(), Generation: 2, Rules: 2, DurationMS: 1.5}
	}
	s.Config = func() ConfigView { return ConfigView{Config: c.Redacted(), Generation: 2, LoadedAt: time.Now()} }
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	authed := func(method, path string) (int, []byte) {
		req, _ := http.NewRequest(method, srv.URL+path, nil) //nolint:noctx // test
		req.Header.Set("Authorization", "Bearer secret-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck // test
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	code, body := authed(http.MethodPost, "/admin/reload")
	var res ReloadResult
	if code != 200 || json.Unmarshal(body, &res) != nil || !res.OK || res.Generation != 2 || res.Rules != 2 {
		t.Fatalf("%d %s", code, body)
	}
	fail = true
	code, body = authed(http.MethodPost, "/admin/reload")
	if code != http.StatusUnprocessableEntity || json.Unmarshal(body, &res) != nil || res.OK || !strings.Contains(res.Error, "unknown key") {
		t.Fatalf("%d %s", code, body)
	}
	if code, _ := authed(http.MethodGet, "/admin/reload"); code != 405 {
		t.Fatalf("GET reload: %d", code)
	}
	code, body = authed(http.MethodGet, "/admin/config")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var view map[string]any
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	cfgMap, _ := view["config"].(map[string]any)
	adminMap, _ := cfgMap["admin"].(map[string]any)
	if cfgMap["node_id"] != "t" || adminMap["token"] != "<redacted>" || strings.Contains(string(body), "secret-token") {
		t.Fatalf("config view must use the file's keys and redact the token:\n%s", body)
	}
	if view["generation"] != float64(2) {
		t.Fatalf("generation: %v", view["generation"])
	}
	// Without the token every /admin/* route is 401, including the new ones.
	for _, r := range []struct{ m, p string }{{"POST", "/admin/reload"}, {"GET", "/admin/config"}, {"GET", "/admin/limits"}, {"POST", "/admin/limits/test"}} {
		req, _ := http.NewRequest(r.m, srv.URL+r.p, nil) //nolint:noctx // test
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close() //nolint:errcheck // test
		if resp.StatusCode != 401 {
			t.Errorf("%s %s without token: %d", r.m, r.p, resp.StatusCode)
		}
	}
}

func TestIdentityFor(t *testing.T) {
	id, err := identityFor(&TestRequest{Op: "GetObject", Bucket: "b", Key: "k", IP: "::ffff:10.0.0.1"})
	if err != nil || id.Level != s3id.LevelObject || id.SourceIP.String() != "10.0.0.1" || id.Class != s3id.ClassRead {
		t.Fatalf("%+v %v", id, err)
	}
	if id, err := identityFor(&TestRequest{Op: "HeadBucket", Bucket: "b"}); err != nil || id.Level != s3id.LevelBucket {
		t.Fatalf("%+v %v", id, err)
	}
	if _, err := identityFor(&TestRequest{}); err == nil {
		t.Fatal("op is required")
	}
}
