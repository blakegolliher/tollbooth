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
	"bufio"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/limiter"
)

// TestOperatorSurfaceParity (test plan category 12): every route in Routes appears in
// docs/OPERATOR_SURFACE.md with a CLI verb and a TUI screen. A cell of "—" is an explicit
// exemption and must say why in the same cell.
func TestOperatorSurfaceParity(t *testing.T) {
	f, err := os.Open("../../docs/OPERATOR_SURFACE.md")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()               //nolint:errcheck // read-only
	rows := map[string][]string{} // "METHOD /path" → cells
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 4 {
			continue
		}
		// The cell is `METHOD /path[?query|*]` possibly followed by a parenthetical note.
		route := strings.TrimSpace(strings.SplitN(strings.TrimSpace(cells[0]), " (", 2)[0])
		route = strings.Trim(route, "`")
		if i := strings.Index(route, "?"); i >= 0 {
			route = route[:i]
		}
		route = strings.TrimSuffix(route, "*")
		rows[route] = cells
	}
	for _, r := range Routes {
		key := r.Method + " " + r.Path
		cells, ok := rows[key]
		if !ok {
			t.Errorf("route %s has no row in docs/OPERATOR_SURFACE.md", key)
			continue
		}
		cli, tui := strings.TrimSpace(cells[1]), strings.TrimSpace(cells[2])
		if cli == "" || (strings.HasPrefix(cli, "—") && !strings.Contains(cli, "(")) {
			t.Errorf("route %s: CLI cell %q needs a verb or an exemption with a reason", key, cli)
		}
		if tui == "" || (strings.HasPrefix(tui, "—") && !strings.Contains(tui, "(")) {
			t.Errorf("route %s: TUI cell %q needs a screen or an exemption with a reason", key, tui)
		}
	}
	// Every served route is registered: build a handler and probe each path for a non-404.
	s := &Server{Metrics: NewMetrics(), Ready: func() bool { return true }, Pprof: true,
		Reload: func() ReloadResult { return ReloadResult{OK: true} }, Config: func() ConfigView { return ConfigView{} },
		Usage: &Usage{Dir: t.TempDir()}, Backends: func() []BackendStatus { return nil }}
	c, _ := parseLimits(t)
	s.Limiter = c
	h := s.Handler()
	for _, r := range Routes {
		rec := newRecorder()
		req := newRequest(r.Method, r.Path)
		h.ServeHTTP(rec, req)
		if rec.code == 404 || rec.code == 405 {
			t.Errorf("route %s is in the table but not registered (status %d)", r.Method+" "+r.Path, rec.code)
		}
	}
}

// Small helpers so the parity probe does not pull in httptest's recorder semantics for bodies.
type recorder struct {
	code int
	hdr  http.Header
}

func newRecorder() *recorder                    { return &recorder{code: 200, hdr: http.Header{}} }
func (r *recorder) Header() http.Header         { return r.hdr }
func (r *recorder) Write(p []byte) (int, error) { return len(p), nil }
func (r *recorder) WriteHeader(c int)           { r.code = c }

func newRequest(method, path string) *http.Request {
	body := ""
	if method == http.MethodPost && strings.HasSuffix(path, "/test") {
		body = `{"op":"GetObject"}`
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	return req
}

func parseLimits(t *testing.T) (*limiter.Limiter, *config.Config) {
	t.Helper()
	c, err := config.Parse([]byte(limitsConfig))
	if err != nil {
		t.Fatal(err)
	}
	l := limiter.New(nil)
	if err := l.Apply(c); err != nil {
		t.Fatal(err)
	}
	return l, c
}
