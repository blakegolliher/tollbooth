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
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/blakegolliher/tollbooth/bench/nullbackend"
	"github.com/blakegolliher/tollbooth/internal/config"
)

// freePort reserves and releases a loopback port so serve can bind it.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func writeConfig(t *testing.T, dir string, body string) string {
	t.Helper()
	p := filepath.Join(dir, "tollbooth.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckConfig(t *testing.T) {
	dir := t.TempDir()
	good := writeConfig(t, dir, "node_id: a\nbackend: {scheme: http, endpoints: [h:1]}\nshowback: {dir: "+dir+"}\n")
	out, errOut := tempFiles(t)
	if rc := run([]string{"check-config", good}, out, errOut); rc != 0 {
		t.Fatalf("rc %d: %s", rc, read(errOut))
	}
	if !strings.Contains(read(out), ": ok (0 rules, 1 backend endpoints, 1 warnings)") || !strings.Contains(read(errOut), "warning: base_domains is empty") {
		t.Fatalf("stdout %q stderr %q", read(out), read(errOut))
	}
	badDir := filepath.Join(dir, "bad")
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bad := writeConfig(t, badDir, "node_id: a\nbackend: {scheme: ftp, endpoints: []}\nlisten: {port: x}\n")
	out, errOut = tempFiles(t)
	if rc := run([]string{"check-config", bad}, out, errOut); rc != 1 {
		t.Fatalf("rc %d for an invalid file", rc)
	}
	if e := read(errOut); !strings.Contains(e, "listen.port: unknown key") {
		t.Fatalf("stderr %q", e)
	}
	out, errOut = tempFiles(t)
	if rc := run([]string{"check-config", filepath.Join(dir, "missing.yaml")}, out, errOut); rc != 1 {
		t.Fatalf("rc %d for a missing file", rc)
	}
	// GOMEMLIMIT budget warning (D-024).
	t.Setenv("GOMEMLIMIT", "256MiB")
	out, errOut = tempFiles(t)
	if rc := run([]string{"check-config", good}, out, errOut); rc != 0 || !strings.Contains(read(errOut), "exceeds half of GOMEMLIMIT") {
		t.Fatalf("budget warning: rc %d stderr %q", rc, read(errOut))
	}
	if goMemLimit() != 256<<20 {
		t.Fatalf("goMemLimit = %d", goMemLimit())
	}
	t.Setenv("GOMEMLIMIT", "off")
	if goMemLimit() != 0 {
		t.Fatal("off must be 0")
	}
}

func TestVersionAndUsage(t *testing.T) {
	out, errOut := tempFiles(t)
	if rc := run([]string{"version"}, out, errOut); rc != 0 || !strings.HasPrefix(read(out), "tollbooth dev (commit unknown") {
		t.Fatalf("rc %d out %q", rc, read(out))
	}
	out, errOut = tempFiles(t)
	if rc := run(nil, out, errOut); rc != 2 || !strings.Contains(read(errOut), "usage:") {
		t.Fatalf("no args: rc %d", rc)
	}
	out, errOut = tempFiles(t)
	if rc := run([]string{"frobnicate"}, out, errOut); rc != 2 || !strings.Contains(read(errOut), "unknown command") {
		t.Fatalf("unknown: rc %d", rc)
	}
	out, errOut = tempFiles(t)
	if rc := run([]string{"tui", "--nope"}, out, errOut); rc != 2 {
		t.Fatalf("bad flag on tui: rc %d", rc)
	}
	out, errOut = tempFiles(t)
	if rc := run([]string{"help"}, out, errOut); rc != 0 || !strings.Contains(read(out), "tui") {
		t.Fatalf("help: rc %d", rc)
	}
}

// serve end to end: a request flows through, the admin endpoints answer, SIGHUP reloads (and an
// invalid file keeps the running config), and canceling the context drains and returns nil.
func TestServeLifecycle(t *testing.T) {
	nb := nullbackend.New(16)
	be := httptest.NewServer(nb)
	defer be.Close()
	dir := t.TempDir()
	listen, adminAddr := freePort(t), freePort(t)
	body := func(level string) string {
		return "node_id: a\nlisten: {address: " + listen + "}\nadmin: {address: " + adminAddr + ", token: tok}\nbackend: {scheme: http, endpoints: [" + strings.TrimPrefix(be.URL, "http://") + "], health_check: {interval: 50ms, timeout: 200ms}}\nshowback: {dir: " + dir + "}\nlogging: {level: " + level + ", access: true}\n"
	}
	path := writeConfig(t, dir, body("info"))
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

	waitHTTP := func(url string, want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			resp, err := http.Get(url) //nolint:noctx // test
			if err == nil {
				resp.Body.Close() //nolint:errcheck // test
				if resp.StatusCode == want {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never answered %d (last err %v)", url, want, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitHTTP("http://"+adminAddr+"/healthz", 200)
	waitHTTP("http://"+adminAddr+"/readyz", 200)
	resp, err := http.Get("http://" + listen + "/b/k") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close() //nolint:errcheck // test
	if resp.StatusCode != 200 || len(b) != 16 {
		t.Fatalf("proxied GET: %d %d bytes", resp.StatusCode, len(b))
	}
	if n := nb.Served(); n < 1 {
		t.Fatalf("backend served %d", n)
	}
	// Metrics are open; /admin/* needs the token.
	resp, _ = http.Get("http://" + adminAddr + "/metrics") //nolint:noctx // test
	m, _ := io.ReadAll(resp.Body)
	resp.Body.Close() //nolint:errcheck // test
	if !strings.Contains(string(m), `tollbooth_requests_total{op_class="READ",operation="GetObject",result="forwarded",status_class="2xx"} 1`) {
		t.Fatalf("metrics missing the request:\n%s", firstLines(string(m), "tollbooth_requests_total{op_class=\"READ\",operation=\"GetObject\""))
	}
	resp, _ = http.Get("http://" + adminAddr + "/admin/limits") //nolint:noctx // test
	resp.Body.Close()                                           //nolint:errcheck // test
	if resp.StatusCode != 401 {
		t.Fatalf("admin without token: %d", resp.StatusCode)
	}

	// SIGHUP with a changed log level: reload ok. Then an invalid file: reload error, config kept.
	if err := os.WriteFile(path, []byte(body("debug")), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGHUP)
	waitLog(t, logBuf, `"msg":"config reloaded"`)
	if err := os.WriteFile(path, []byte("node_id: a\nbogus: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGHUP)
	waitLog(t, logBuf, `"msg":"config reload failed; running config kept"`)
	// A restart-only change is refused too.
	if err := os.WriteFile(path, []byte(strings.Replace(body("info"), "node_id: a", "node_id: b", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGHUP)
	waitLog(t, logBuf, `node_id: cannot change on reload`)
	resp, _ = http.Get("http://" + adminAddr + "/metrics") //nolint:noctx // test
	m, _ = io.ReadAll(resp.Body)
	resp.Body.Close() //nolint:errcheck // test
	if !strings.Contains(string(m), `tollbooth_config_reload_total{result="error"} 2`) || !strings.Contains(string(m), `tollbooth_config_reload_total{result="ok"} 1`) {
		t.Fatalf("reload counters:\n%s", firstLines(string(m), "tollbooth_config_reload_total"))
	}
	// The access log went to stdout.
	if s := read(stdout); !strings.Contains(s, `"operation":"GetObject"`) {
		t.Fatalf("access log missing: %q", s)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not drain")
	}
	waitLog(t, logBuf, `"msg":"tollbooth stopped"`)
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func waitLog(t *testing.T, b *lockedBuffer, substr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(b.String(), substr) {
		if time.Now().After(deadline) {
			t.Fatalf("log never contained %q:\n%s", substr, b.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func tempFiles(t *testing.T) (stdout, stderr *os.File) {
	t.Helper()
	o, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	e, err := os.CreateTemp(t.TempDir(), "err")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o.Close(); e.Close() })
	return o, e
}

func read(f *os.File) string {
	b, _ := os.ReadFile(f.Name())
	return string(b)
}

func firstLines(s, prefix string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// runServe: flag and config failures exit non-zero with one line; a real run stops on SIGTERM
// with exit 0 after draining.
func TestRunServe(t *testing.T) {
	out, errOut := tempFiles(t)
	if rc := runServe([]string{"-bogus"}, out, errOut); rc != 2 {
		t.Fatalf("bad flag: rc %d", rc)
	}
	out, errOut = tempFiles(t)
	if rc := runServe([]string{"--config", filepath.Join(t.TempDir(), "missing.yaml")}, out, errOut); rc != 1 || !strings.Contains(read(errOut), "tollbooth serve:") {
		t.Fatalf("missing config: rc %d stderr %q", rc, read(errOut))
	}
	// A listener address already in use is a startup failure, not a hang.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dir := t.TempDir()
	busy := writeConfig(t, dir, "node_id: a\nlisten: {address: "+ln.Addr().String()+"}\nadmin: {address: "+freePort(t)+"}\nbackend: {scheme: http, endpoints: [127.0.0.1:1]}\nshowback: {dir: "+dir+"}\n")
	out, errOut = tempFiles(t)
	if rc := runServe([]string{"--config", busy}, out, errOut); rc != 1 {
		t.Fatalf("busy listener: rc %d stdout %q", rc, read(out))
	}
	if !strings.Contains(read(out), `"msg":"serve failed"`) {
		t.Fatalf("startup failure not logged: %q", read(out))
	}

	// The real thing: start, wait for /healthz, SIGTERM ourselves, expect exit 0.
	nb := nullbackend.New(16)
	be := httptest.NewServer(nb)
	defer be.Close()
	listen, adminAddr := freePort(t), freePort(t)
	goodDir := filepath.Join(dir, "good")
	if err := os.MkdirAll(goodDir, 0o700); err != nil {
		t.Fatal(err)
	}
	good := writeConfig(t, goodDir, "node_id: a\nlisten: {address: "+listen+"}\nadmin: {address: "+adminAddr+"}\nbackend: {scheme: http, endpoints: ["+strings.TrimPrefix(be.URL, "http://")+"]}\nshowback: {dir: "+dir+"}\nlogging: {level: debug}\n")
	out, errOut = tempFiles(t)
	done := make(chan int, 1)
	go func() { done <- runServe([]string{"--config", good}, out, errOut) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + adminAddr + "/healthz") //nolint:noctx // test
		if err == nil {
			resp.Body.Close() //nolint:errcheck // test
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve never became healthy")
		}
		time.Sleep(20 * time.Millisecond)
	}
	resp, err := http.Get("http://" + listen + "/b/k") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck // test
	resp.Body.Close()              //nolint:errcheck // test
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	select {
	case rc := <-done:
		if rc != 0 {
			t.Fatalf("serve exited %d after SIGTERM; stdout:\n%s", rc, read(out))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit after SIGTERM")
	}
	if s := read(out); !strings.Contains(s, `"msg":"tollbooth starting"`) || !strings.Contains(s, `"signal":"terminated"`) || !strings.Contains(s, `"msg":"tollbooth stopped"`) {
		t.Fatalf("startup/stop lines missing:\n%s", s)
	}
	for _, lvl := range []string{"debug", "info", "warn", "error", "bogus"} {
		if newLogger(out, lvl) == nil {
			t.Fatalf("newLogger(%s) returned nil", lvl)
		}
	}
}
