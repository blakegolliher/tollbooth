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
	"crypto/subtle"
	"net/http"
	"net/http/pprof"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Route is one admin HTTP route. The table is exported so the parity test (test plan category
// 12) can assert every route appears in docs/OPERATOR_SURFACE.md.
type Route struct {
	Method string
	Path   string
	// Open routes need no bearer token: health, readiness, and metrics are for load balancers and
	// Prometheus, which cannot carry one.
	Open bool
	// Phase the route lands in; routes of later phases are listed but not registered yet.
	Phase int
}

// Routes is the admin API (docs/SPEC.md, Observability and admin API; Operator surface).
var Routes = []Route{
	{Method: http.MethodGet, Path: "/healthz", Open: true, Phase: 1},
	{Method: http.MethodGet, Path: "/readyz", Open: true, Phase: 1},
	{Method: http.MethodGet, Path: "/metrics", Open: true, Phase: 1},
	{Method: http.MethodPost, Path: "/admin/reload", Phase: 2},
	{Method: http.MethodGet, Path: "/admin/config", Phase: 2},
	{Method: http.MethodGet, Path: "/admin/limits", Phase: 2},
	{Method: http.MethodGet, Path: "/admin/limits/top", Phase: 2},
	{Method: http.MethodPost, Path: "/admin/limits/test", Phase: 4},
	{Method: http.MethodGet, Path: "/admin/usage", Phase: 3},
	{Method: http.MethodGet, Path: "/admin/status", Phase: 4},
	{Method: http.MethodGet, Path: "/admin/backends", Phase: 4},
	{Method: http.MethodGet, Path: "/debug/pprof/", Phase: 1},
}

// Server is the admin listener: a second http.Server with its own mux and connection handling,
// never sharing the data listener's pool. Phase 1 serves health, readiness, metrics, and pprof;
// later phases add their routes here.
type Server struct {
	Address string
	Metrics *Metrics
	// Ready reports whether the config is loaded and at least one backend is healthy.
	Ready func() bool
	// Token, when non-empty, is required as a bearer token on every /admin/* route.
	Token string
	Pprof bool
}

// Handler builds the mux. A mux is fine here: nothing on this port is an object key.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if s.Ready != nil && !s.Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready\n"))
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.Metrics.Registry, promhttp.HandlerOpts{}))
	if s.Pprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	return s.auth(mux)
}

// auth requires the bearer token on /admin/* when one is configured. Comparison is constant time.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" && strings.HasPrefix(r.URL.Path, "/admin/") {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// HTTPServer returns the configured http.Server for Address.
func (s *Server) HTTPServer() *http.Server {
	return &http.Server{
		Addr:              s.Address,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}
