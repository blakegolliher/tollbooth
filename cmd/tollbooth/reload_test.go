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
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blakegolliher/tollbooth/bench/nullbackend"
	"github.com/blakegolliher/tollbooth/internal/admin"
	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/showback"
)

// Reload under load (docs/SPEC.md, Rate limiting, Hot reload): with clients hammering the proxy,
// twenty reloads through POST /admin/reload each complete in under 100 ms, every client request
// succeeds, the new rule set is in force after each, and an invalid file is refused with 422 and
// keeps the running config.
func TestReloadUnderLoad(t *testing.T) {
	nb := nullbackend.New(1024)
	be := httptest.NewServer(nb)
	defer be.Close()
	dir := t.TempDir()
	listen, adminAddr := freePort(t), freePort(t)
	body := func(rule string) string {
		return "node_id: a\nlisten: {address: " + listen + "}\nadmin: {address: " + adminAddr + ", token: tok}\nbackend: {scheme: http, endpoints: [" + strings.TrimPrefix(be.URL, "http://") + "], health_check: {interval: 50ms, timeout: 200ms}}\nshowback: {dir: " + dir + "}\n" +
			"rate_limits:\n  rules:\n    - id: " + rule + "\n      match: {buckets: [never]}\n      limit: {rate: 0}\n    - id: wide\n      match: {ops: [READ]}\n      group_by: [access_key]\n      limit: {rate: 1000000000, burst: 1000000000}\n"
	}
	path := writeConfig(t, dir, body("gen-0"))
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	logBuf := &lockedBuffer{}
	log := slog.New(slog.NewJSONHandler(logBuf, nil))
	ctx, cancel := context.WithCancel(context.Background())
	stdout, _ := tempFiles(t)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, path, cfg, log, stdout) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve returned %v", err)
		}
	}()
	waitReady(t, "http://"+adminAddr+"/readyz")

	// Load: four clients, keep-alive, as fast as the box allows.
	var stop atomic.Bool
	var total, failed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4}}
			for !stop.Load() {
				resp, err := c.Get("http://" + listen + "/b/k") //nolint:noctx // test
				total.Add(1)
				if err != nil {
					failed.Add(1)
					continue
				}
				io.Copy(io.Discard, resp.Body) //nolint:errcheck // test
				resp.Body.Close()              //nolint:errcheck // test
				if resp.StatusCode != 200 {
					failed.Add(1)
				}
			}
		}()
	}
	time.Sleep(200 * time.Millisecond) // let the load ramp before the first reload

	post := func(path string) (int, admin.ReloadResult) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+adminAddr+path, nil) //nolint:noctx // test
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck // test
		var res admin.ReloadResult
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, res
	}
	var slowest float64
	for i := 1; i <= 20; i++ {
		rule := "gen-" + strings.Repeat("x", i%3) + string(rune('a'+i))
		if err := os.WriteFile(path, []byte(body(rule)), 0o600); err != nil {
			t.Fatal(err)
		}
		code, res := post("/admin/reload")
		if code != 200 || !res.OK || res.Generation != i+1 || res.Rules != 2 {
			t.Fatalf("reload %d: %d %+v", i, code, res)
		}
		slowest = max(slowest, res.DurationMS)
		if res.DurationMS > 100 {
			t.Errorf("reload %d took %.1f ms; the bound is 100 ms", i, res.DurationMS)
		}
		// The new set is in force: the first rule carries the new id.
		req, _ := http.NewRequest(http.MethodGet, "http://"+adminAddr+"/admin/limits", nil) //nolint:noctx // test
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var lim admin.Limits
		_ = json.NewDecoder(resp.Body).Decode(&lim)
		resp.Body.Close() //nolint:errcheck // test
		if len(lim.Rules) != 2 || lim.Rules[0].ID != rule {
			t.Fatalf("after reload %d rules are %+v", i, lim.Rules)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// An invalid file: 422, running config kept, generation unchanged.
	if err := os.WriteFile(path, []byte("node_id: a\nbogus: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, res := post("/admin/reload")
	if code != http.StatusUnprocessableEntity || res.OK || !strings.Contains(res.Error, "bogus: unknown key") || res.Generation != 21 {
		t.Fatalf("invalid reload: %d %+v", code, res)
	}
	stop.Store(true)
	wg.Wait()
	t.Logf("reload under load: 20 reloads, slowest %.2f ms; %d client requests, %d failed", slowest, total.Load(), failed.Load())
	if failed.Load() != 0 {
		t.Fatalf("%d of %d client requests failed during reloads", failed.Load(), total.Load())
	}
	if total.Load() < 200 {
		t.Fatalf("only %d requests were made; the load was not load", total.Load())
	}
	// GET /admin/config shows the kept config (generation 21) and the failed attempt.
	req, _ := http.NewRequest(http.MethodGet, "http://"+adminAddr+"/admin/config", nil) //nolint:noctx // test
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close() //nolint:errcheck // test
	var view struct {
		Config     map[string]any      `json:"config"`
		Generation int                 `json:"generation"`
		LastReload *admin.ReloadResult `json:"last_reload"`
	}
	if err := json.Unmarshal(b, &view); err != nil {
		t.Fatal(err)
	}
	if view.Generation != 21 || view.LastReload == nil || view.LastReload.OK || view.Config["node_id"] != "a" || view.Config["admin"].(map[string]any)["token"] != "<redacted>" {
		t.Fatalf("%s", b)
	}
	if !strings.Contains(logBuf.String(), `"msg":"config reloaded"`) || !strings.Contains(logBuf.String(), `"generation":21`) {
		t.Fatalf("log:\n%s", logBuf.String())
	}
}

func waitReady(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(url) //nolint:noctx // test
		if err == nil {
			resp.Body.Close() //nolint:errcheck // test
			if resp.StatusCode == 200 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never answered 200 (last err %v)", url, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// serve snapshots the open hour every flush_interval and takes a final snapshot on drain, so a
// stop loses nothing that completed, and the files read back with the usage verbs.
func TestServeShowbackSnapshotAndDrain(t *testing.T) {
	nb := nullbackend.New(64)
	be := httptest.NewServer(nb)
	defer be.Close()
	dir := t.TempDir()
	listen, adminAddr := freePort(t), freePort(t)
	path := writeConfig(t, dir, "node_id: sb\nlisten: {address: "+listen+"}\nadmin: {address: "+adminAddr+"}\nbackend: {scheme: http, endpoints: ["+strings.TrimPrefix(be.URL, "http://")+"], health_check: {interval: 50ms, timeout: 200ms}}\nshowback: {dir: "+dir+"/sb, flush_interval: 200ms}\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	logBuf := &lockedBuffer{}
	log := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	stdout, _ := tempFiles(t)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, path, cfg, log, stdout) }()
	waitReady(t, "http://"+adminAddr+"/readyz")
	for i := 0; i < 3; i++ {
		resp, err := http.Get("http://" + listen + "/b/k") //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // test
		resp.Body.Close()              //nolint:errcheck // test
	}
	// The periodic snapshot appears within a flush interval or two.
	cur := showback.CurrentPath(dir+"/sb", "sb", time.Now().Unix()/3600)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(cur); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no snapshot at %s", cur)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// GET /admin/usage sees them (open hour, partial).
	resp, err := http.Get("http://" + adminAddr + "/admin/usage?group_by=operation") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	var res showback.Result
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close() //nolint:errcheck // test
	if !res.Partial || res.Requests < 1 {
		t.Fatalf("admin usage: %+v", res)
	}
	resp, _ = http.Get("http://" + listen + "/b/k2") //nolint:noctx // test
	io.Copy(io.Discard, resp.Body)                   //nolint:errcheck // test
	resp.Body.Close()                                //nolint:errcheck // test
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// The final snapshot on drain holds all four.
	out, errOut := tempFiles(t)
	if rc := run([]string{"usage", "query", "--group-by", "operation", dir + "/sb"}, out, errOut); rc != 0 {
		t.Fatalf("rc %d: %s", rc, read(errOut))
	}
	if s := read(out); !strings.Contains(s, "4 requests") || !strings.Contains(s, "GetObject") {
		t.Fatalf("usage query after drain:\n%s\nserve log:\n%s", s, logBuf.String())
	}
}
