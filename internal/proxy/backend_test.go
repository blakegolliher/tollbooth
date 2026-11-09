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
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/blakegolliher/tollbooth/internal/admin"
	"github.com/blakegolliher/tollbooth/internal/config"
)

func poolFor(t *testing.T, endpoints []string) (*Pool, *admin.Metrics) {
	t.Helper()
	c := testConfig(t, endpoints, time.Second)
	c.Backend.HealthCheck.Interval = 50 * time.Millisecond
	c.Backend.HealthCheck.Timeout = 300 * time.Millisecond
	m := admin.NewMetrics()
	p, err := NewPool(&c.Backend, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p, m
}

// Active checks: a 403 from HEAD / is healthy (any HTTP status is); a closed port is down and
// comes back when the port answers again.
func TestHealthChecks(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.URL.Path != "/" {
			t.Errorf("health check sent %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusForbidden) // an unsigned HEAD / is 403 on every real backend
	}))
	defer healthy.Close()
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := dead.Addr().String()
	dead.Close()
	p, m := poolFor(t, []string{strings.TrimPrefix(healthy.URL, "http://"), deadAddr})
	p.Start()
	defer p.Stop()
	deadline := time.Now().Add(3 * time.Second)
	for p.Endpoints[1].Up() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !p.Endpoints[0].Up() || p.Endpoints[1].Up() {
		t.Fatalf("health: %v %v", p.Endpoints[0].Up(), p.Endpoints[1].Up())
	}
	if msg, at := p.Endpoints[1].LastError(); msg == "" || at.IsZero() {
		t.Error("dead endpoint has no recorded error")
	}
	if !p.Healthy() {
		t.Error("pool with one live endpoint must be healthy")
	}
	// Next skips the dead endpoint every time.
	for i := 0; i < 10; i++ {
		if e := p.Next(); e != p.Endpoints[0] {
			t.Fatalf("Next returned the dead endpoint")
		}
	}
	// The dead port comes back: the checker brings it up.
	revived, err := net.Listen("tcp", deadAddr)
	if err != nil {
		t.Skipf("could not rebind %s: %v", deadAddr, err)
	}
	rs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }), ReadHeaderTimeout: time.Second}
	go rs.Serve(revived) //nolint:errcheck // test server
	defer rs.Close()
	deadline = time.Now().Add(3 * time.Second)
	for !p.Endpoints[1].Up() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !p.Endpoints[1].Up() {
		t.Fatal("revived endpoint never marked up")
	}
	fams, _ := m.Registry.Gather()
	for _, f := range fams {
		if f.GetName() == "tollbooth_backend_up" && len(f.GetMetric()) != 2 {
			t.Errorf("backend_up has %d series, want 2", len(f.GetMetric()))
		}
	}
}

// With every endpoint down, Next still returns one so the request fails with a real error.
func TestNextWithEveryEndpointDown(t *testing.T) {
	p, _ := poolFor(t, []string{"127.0.0.1:1", "127.0.0.1:2"})
	p.MarkDown(p.Endpoints[0], errors.New("x"), "dial")
	p.MarkDown(p.Endpoints[1], errors.New("x"), "dial")
	if p.Healthy() {
		t.Fatal("healthy with every endpoint down")
	}
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		seen[p.Next().Addr] = true
	}
	if len(seen) != 2 {
		t.Fatalf("Next with all down must still rotate, saw %v", seen)
	}
}

func TestErrorKinds(t *testing.T) {
	cases := map[string]error{
		"dial":    &net.OpError{Op: "dial", Err: errors.New("refused")},
		"timeout": context.DeadlineExceeded,
		"reset":   context.Canceled,
		"other":   errors.New("something else"),
	}
	for want, err := range cases {
		if got := errorKind(err); got != want {
			t.Errorf("errorKind(%v) = %s, want %s", err, got, want)
		}
	}
	if !IsConnectError(cases["dial"]) || IsConnectError(cases["other"]) {
		t.Error("IsConnectError")
	}
}

func TestNewPoolRejectsEmptyAndBadCA(t *testing.T) {
	if _, err := NewPool(&config.Backend{Scheme: "http"}, nil, nil); err == nil {
		t.Error("no endpoints accepted")
	}
	if _, err := NewPool(&config.Backend{Scheme: "https", Endpoints: []string{"h:1"}, TLS: config.BackendTLS{CABundle: "/nope.pem"}}, nil, nil); err == nil {
		t.Error("missing CA bundle accepted")
	}
}
