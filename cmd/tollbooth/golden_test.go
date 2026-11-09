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
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/blakegolliher/tollbooth/bench/nullbackend"
	"github.com/blakegolliher/tollbooth/internal/admin"
	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/limiter"
	"github.com/blakegolliher/tollbooth/internal/s3id"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/golden")

// goldenFixture is a deterministic admin server: fixed node, version, timestamps, counters, and
// backends, so every verb's output in every format is stable except the fields the normalizer
// masks (the ones that read the real clock).
func goldenFixture(t *testing.T) (addr string, cfgPath string, usageDir string) {
	t.Helper()
	c, err := config.Parse([]byte(rulesTestConfig))
	if err != nil {
		t.Fatal(err)
	}
	m := admin.NewMetrics()
	lim := limiter.New(&m.Limiter)
	if err := lim.Apply(c); err != nil {
		t.Fatal(err)
	}
	id := s3id.RequestIdentity{AccessKey: "AKIAREVOKED", Bucket: "b", Key: "k", Op: s3id.OpGetObject, Class: s3id.ClassRead, Level: s3id.LevelObject}
	for i := 0; i < 3; i++ {
		lim.Decide(&id, lim.Now(time.Now()), true)
	}
	m.TotalRequests.Store(1200)
	m.TotalThrottled.Store(3)
	m.InflightCount.Store(2)
	at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	reloadOK := true
	s := &admin.Server{Metrics: m, Limiter: lim, Token: "tok", NodeID: "golden", Version: "v0.0.0-golden", StartedAt: at,
		Ready: func() bool { return true },
		Config: func() admin.ConfigView {
			return admin.ConfigView{Config: c.Redacted(), Generation: 2, LoadedAt: at, LastReload: &admin.ReloadResult{OK: true, At: at, Generation: 2, Rules: 3, DurationMS: 0.5}}
		},
		Reload: func() admin.ReloadResult {
			if reloadOK {
				reloadOK = false
				return admin.ReloadResult{OK: true, At: at, Generation: 3, Rules: 3, DurationMS: 0.42}
			}
			return admin.ReloadResult{Error: "listen.addres: unknown key", At: at, Generation: 3}
		},
		Backends: func() []admin.BackendStatus {
			return []admin.BackendStatus{
				{Endpoint: "10.0.0.1:9000", Up: true, Inflight: 1, Requests: 1100, TTFBMeanMS: 1.25, TTFBLastMS: 0.9},
				{Endpoint: "10.0.0.2:9000", Up: false, Requests: 100, Errors: 4, ErrorsDial: 4, LastError: "dial tcp 10.0.0.2:9000: connect: connection refused", LastErrAt: "2026-11-09T11:58:00Z"},
			}
		},
		Showback: func() (string, int64) { return "2026-11-09T12:00:00Z", 17 },
		Usage:    &admin.Usage{Dir: usageDir},
	}
	usageDir = usageDirFixture(t)
	s.Usage.Dir = usageDir
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	cfgPath = filepath.Join(t.TempDir(), "tollbooth.yaml")
	// The file on disk differs from the running config in one key, for `config diff`.
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(rulesTestConfig, "node_count: 2", "node_count: 4", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(srv.URL, "http://"), cfgPath, usageDir
}

func usageDirFixture(t *testing.T) string {
	t.Helper()
	return usageDir(t)
}

// normalizers mask the values that come from the real clock or the environment.
var normalizers = []*regexp.Regexp{
	regexp.MustCompile(`"uptime_seconds": [0-9.e+-]+`),
	regexp.MustCompile(`uptime[\s,]+[0-9hms.]+`),
	regexp.MustCompile(`"last_denied_seconds_ago": [0-9.e+-]+`),
	regexp.MustCompile(`\d+s ago`),
	regexp.MustCompile(`127\.0\.0\.1:\d+`),
	regexp.MustCompile(`/tmp/[^"\s,]+`),
	regexp.MustCompile(`"config_file": "[^"]+"`),
}

func normalize(s string) string {
	for _, re := range normalizers {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			if i := strings.IndexAny(m, ":"); i >= 0 && (strings.HasPrefix(m, `"`) || strings.HasPrefix(m, "127")) {
				if strings.HasPrefix(m, "127") {
					return "127.0.0.1:PORT"
				}
				return m[:i+1] + " X"
			}
			if strings.HasPrefix(m, "/tmp/") {
				return "/TMP"
			}
			if strings.HasPrefix(m, "uptime") {
				sep := " "
				if strings.Contains(m, ",") {
					sep = ","
				}
				return "uptime" + sep + "X"
			}
			return "X ago"
		})
	}
	return s
}

// TestGoldens runs every verb in table, json, and csv against the fixture and compares with
// testdata/golden/<name>.txt (test plan category 12). -update rewrites them.
func TestGoldens(t *testing.T) {
	addr, cfgPath, dir := goldenFixture(t)
	common := []string{"--admin", addr, "--token", "tok", "--config", cfgPath}
	cases := []struct {
		name string
		args []string
	}{
		{"status", []string{"status"}},
		{"rules-list", []string{"rules", "list"}},
		{"rules-show", []string{"rules", "show", "list-storm"}},
		{"rules-test", []string{"rules", "test", "--op", "ListObjectsV2", "--bucket", "b", "--ip", "10.0.0.9"}},
		{"rules-top", []string{"rules", "top", "--n", "5"}},
		{"config-show", []string{"config", "show"}},
		{"config-diff", []string{"config", "diff"}},
		{"backends-list", []string{"backends", "list"}},
		{"usage-query", []string{"usage", "query", "--group-by", "access_key,status_class", dir}},
		{"usage-top", []string{"usage", "top", "--by", "bucket", dir}},
		{"usage-verify", []string{"usage", "verify", dir}},
	}
	for _, c := range cases {
		for _, format := range []string{"table", "json", "csv"} {
			name := c.name + "-" + format
			t.Run(name, func(t *testing.T) {
				args := append([]string{}, c.args...)
				if c.args[0] == "usage" {
					args = append(args, "--config", cfgPath) // file verbs take no admin flags
				} else {
					args = append(args, common...)
				}
				args = append(args, "--format", format)
				rc, out, errOut := runOut(t, args...)
				if rc != 0 {
					t.Fatalf("rc %d: %s", rc, errOut)
				}
				compareGolden(t, name, normalize(out))
			})
		}
	}
	// config reload: first call ok, second rejected (exit 1), both golden in json.
	t.Run("config-reload-ok", func(t *testing.T) {
		rc, out, errOut := runOut(t, append([]string{"config", "reload", "--format", "json"}, common...)...)
		if rc != 0 {
			t.Fatalf("rc %d: %s", rc, errOut)
		}
		compareGolden(t, "config-reload-ok-json", normalize(out))
	})
	t.Run("config-reload-rejected", func(t *testing.T) {
		rc, out, errOut := runOut(t, append([]string{"config", "reload"}, common...)...)
		if rc != 1 || !strings.Contains(errOut, "rejected: listen.addres: unknown key") {
			t.Fatalf("rc %d: %s", rc, errOut)
		}
		compareGolden(t, "config-reload-rejected-table", normalize(out))
	})
}

// TestDoctorGolden runs doctor against a config whose backend is a live null backend and whose
// showback.dir is a temp directory; environment-specific values are masked.
func TestDoctorGolden(t *testing.T) {
	be := httptest.NewServer(nullbackend.New(16))
	defer be.Close()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "tollbooth.yaml")
	body := "node_id: doc\nbackend: {scheme: http, endpoints: [" + strings.TrimPrefix(be.URL, "http://") + ", 127.0.0.1:1]}\nshowback: {dir: " + dir + "/sb}\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	mask := func(s string) string {
		s = normalize(s)
		for _, re := range []*regexp.Regexp{
			regexp.MustCompile(`soft \d+, hard \d+; about \d+ needed`),
			regexp.MustCompile(`\d+ \(NumCPU \d+\)`),
			regexp.MustCompile(`tcp [0-9.µmsn]+, HEAD / 200 in [0-9.µmsn]+`),
			regexp.MustCompile(`[0-9.]+ GiB free`),
			regexp.MustCompile(`max error \d+ µs`),
			regexp.MustCompile(`"detail": "[^"]*"`),
			regexp.MustCompile(`\(adjtimex status 0x[0-9a-f]+\)`),
		} {
			s = re.ReplaceAllString(s, "X")
		}
		// The clock, GOMEMLIMIT, and tmpfs checks depend on the machine: keep their status column
		// out of the comparison too.
		s = regexp.MustCompile(`(?m)^(clock|GOMEMLIMIT|showback\.dir|ulimit nofile)\s+(ok|warn|fail)\s+.*$`).ReplaceAllString(s, "$1  X")
		s = regexp.MustCompile(`"status": "(ok|warn)"`).ReplaceAllString(s, `"status": "X"`)
		s = regexp.MustCompile(`(?m)^(clock|GOMEMLIMIT|showback\.dir|ulimit nofile),(ok|warn),.*$`).ReplaceAllString(s, "$1,X")
		return s
	}
	for _, format := range []string{"table", "json", "csv"} {
		rc, out, errOut := runOut(t, "doctor", "--config", cfgPath, "--format", format)
		if rc != 1 { // 127.0.0.1:1 refuses, so one backend check fails
			t.Fatalf("rc %d: %s\n%s", rc, errOut, out)
		}
		if !strings.Contains(out, "backend 127.0.0.1:1") || !strings.Contains(out, "connection refused") {
			t.Fatalf("%s", out)
		}
		compareGolden(t, "doctor-"+format, mask(out))
	}
	// A config that does not load is one fail line.
	rc, out, _ := runOut(t, "doctor", "--config", filepath.Join(dir, "nope.yaml"))
	if rc != 1 || !strings.Contains(out, "config  fail") {
		t.Fatalf("rc %d\n%s", rc, out)
	}
}

func compareGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".txt")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no golden %s (run with -update): %v", path, err)
	}
	if string(want) != got {
		t.Errorf("%s differs from golden:\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}

// The help texts and usage paths of the new verbs.
func TestPhase4VerbUsage(t *testing.T) {
	for _, c := range []struct {
		args []string
		rc   int
		want string
	}{
		{[]string{"config"}, 2, "verbs:"},
		{[]string{"config", "help"}, 0, "verbs:"},
		{[]string{"config", "bogus"}, 2, "unknown verb"},
		{[]string{"backends"}, 2, "usage: tollbooth backends list"},
		{[]string{"backends", "bogus"}, 2, "unknown verb"},
		{[]string{"backends", "help"}, 0, "usage: tollbooth backends list"},
		{[]string{"status", "--nope"}, 2, ""},
		{[]string{"doctor", "--format", "yaml"}, 1, "--format must be"},
	} {
		rc, out, errOut := runOut(t, c.args...)
		if rc != c.rc || (c.want != "" && !strings.Contains(out+errOut, c.want)) {
			t.Errorf("%v: rc %d\n%s%s", c.args, rc, out, errOut)
		}
	}
	// Unreachable admin: one line on stderr, exit 1, for every admin verb.
	for _, args := range [][]string{{"status"}, {"backends", "list"}, {"config", "show"}, {"config", "reload"}, {"config", "diff"}} {
		rc, _, errOut := runOut(t, append(args, "--admin", "127.0.0.1:9", "--config", filepath.Join(t.TempDir(), "none.yaml"))...)
		if rc != 1 || strings.Count(strings.TrimSpace(errOut), "\n") != 0 {
			t.Errorf("%v: rc %d %q", args, rc, errOut)
		}
	}
	_ = http.StatusOK
}
