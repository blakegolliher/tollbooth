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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/pprof"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/limiter"
	"github.com/blakegolliher/tollbooth/internal/s3id"
	"github.com/blakegolliher/tollbooth/internal/showback"
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
	{Method: http.MethodPost, Path: "/admin/limits/test", Phase: 2}, // moved from Phase 4: `rules test` is a Phase 2 verb (D-039)
	{Method: http.MethodGet, Path: "/admin/usage", Phase: 3},        // served from Phase 3
	{Method: http.MethodGet, Path: "/admin/status", Phase: 4},       // served from Phase 4
	{Method: http.MethodGet, Path: "/admin/backends", Phase: 4},     // served from Phase 4
	{Method: http.MethodGet, Path: "/debug/pprof/", Phase: 1},
}

// ReloadResult is what POST /admin/reload returns and GET /admin/config repeats as the last
// reload. Generation counts the configs published since start; the startup load is 1.
type ReloadResult struct {
	OK         bool      `json:"ok"`
	Error      string    `json:"error,omitempty"`
	At         time.Time `json:"at"`
	Generation int       `json:"generation"`
	Rules      int       `json:"rules"`
	DurationMS float64   `json:"duration_ms"`
}

// ConfigView is GET /admin/config: the effective config with secrets redacted, when it was
// published, and the last reload attempt (absent before the first).
type ConfigView struct {
	Config     *config.Config `json:"config"`
	Generation int            `json:"generation"`
	LoadedAt   time.Time      `json:"loaded_at"`
	LastReload *ReloadResult  `json:"last_reload,omitempty"`
}

// Server is the admin listener: a second http.Server with its own mux and connection handling,
// never sharing the data listener's pool.
type Server struct {
	Address string
	Metrics *Metrics
	// Ready reports whether the config is loaded and at least one backend is healthy.
	Ready func() bool
	// Token, when non-empty, is required as a bearer token on every /admin/* route.
	Token string
	Pprof bool

	// Limiter answers /admin/limits, /admin/limits/top, and /admin/limits/test. nil: not served.
	Limiter *limiter.Limiter
	// Reload runs one reload (the same path as SIGHUP) and reports the result. nil: not served.
	Reload func() ReloadResult
	// Config returns the effective config and reload state. nil: not served.
	Config func() ConfigView
	// Usage answers GET /admin/usage over this node's showback directory. nil: not served.
	Usage *Usage
	// Backends lists the pool's endpoints with their counters. nil: /admin/backends and the
	// status summary's backend rows are not served.
	Backends func() []BackendStatus
	// Status facts the server does not own: node id, version, start time, the showback hour and
	// tuple count. Showback may be nil.
	NodeID    string
	Version   string
	StartedAt time.Time
	Showback  func() (hour string, tuples int64)

	sampler sampler
}

// Usage is the showback query surface: this node's directory only, the same code path as
// `tollbooth usage query`.
type Usage struct {
	Dir string
}

// BackendStatus is one endpoint's row in GET /admin/backends and GET /admin/status, filled by the
// backend pool (internal/proxy owns the counters; this package only renders them).
type BackendStatus struct {
	Endpoint   string `json:"endpoint"`
	Up         bool   `json:"up"`
	Inflight   int64  `json:"inflight"`
	Requests   int64  `json:"requests"`
	Errors     int64  `json:"errors"`
	ErrorsDial int64  `json:"errors_dial"`
	ErrorsTLS  int64  `json:"errors_tls"`
	ErrorsTime int64  `json:"errors_timeout"`
	ErrorsRst  int64  `json:"errors_reset"`
	ErrorsOth  int64  `json:"errors_other"`
	// ErrorsResolve counts lookups of the endpoint's name that failed; Addresses is what the name
	// last resolved to and ResolverFailingSince when lookups began to fail (ADR-0008). A failing
	// resolver does not by itself take an endpoint down.
	ErrorsResolve        int64    `json:"errors_resolve"`
	Addresses            []string `json:"addresses,omitempty"`
	ResolverFailingSince string   `json:"resolver_failing_since,omitempty"`
	TTFBMeanMS           float64  `json:"ttfb_mean_ms"` // mean over every response since start
	TTFBLastMS           float64  `json:"ttfb_last_ms"`
	LastError            string   `json:"last_error,omitempty"`
	LastErrAt            string   `json:"last_error_at,omitempty"`
}

// Rates is per-second request and throttle rates over the last 1, 10, and 60 seconds, from the
// sampler's one-second ring.
type Rates struct {
	Requests1s   float64 `json:"requests_1s"`
	Requests10s  float64 `json:"requests_10s"`
	Requests60s  float64 `json:"requests_60s"`
	Throttled1s  float64 `json:"throttled_1s"`
	Throttled10s float64 `json:"throttled_10s"`
	Throttled60s float64 `json:"throttled_60s"`
}

// StatusView is GET /admin/status: the one-screen summary `tollbooth status` prints.
type StatusView struct {
	NodeID        string          `json:"node_id"`
	Version       string          `json:"version"`
	StartedAt     time.Time       `json:"started_at"`
	UptimeSeconds float64         `json:"uptime_seconds"`
	Ready         bool            `json:"ready"`
	Inflight      int64           `json:"inflight"`
	Requests      int64           `json:"requests_total"`
	Throttled     int64           `json:"throttled_total"`
	Rates         Rates           `json:"rates"`
	Rules         int             `json:"rules"`
	LiveKeys      int64           `json:"limiter_live_keys"`
	Denied        int64           `json:"limiter_denied_total"`
	ShowbackHour  string          `json:"showback_hour"`
	Tuples        int64           `json:"showback_tuples"`
	Generation    int             `json:"config_generation"`
	LoadedAt      time.Time       `json:"config_loaded_at"`
	LastReload    *ReloadResult   `json:"last_reload,omitempty"`
	BackendsUp    int             `json:"backends_up"`
	Backends      []BackendStatus `json:"backends"`
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
	if s.Reload != nil {
		mux.HandleFunc("POST /admin/reload", func(w http.ResponseWriter, _ *http.Request) {
			res := s.Reload()
			status := http.StatusOK
			if !res.OK {
				status = http.StatusUnprocessableEntity // the file was read and rejected; the running config is kept
			}
			writeJSON(w, status, res)
		})
	}
	if s.Config != nil {
		mux.HandleFunc("GET /admin/config", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, s.Config())
		})
	}
	if s.Usage != nil {
		mux.HandleFunc("GET /admin/usage", s.usage)
	}
	if s.Backends != nil {
		mux.HandleFunc("GET /admin/backends", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"backends": s.Backends()})
		})
	}
	if s.Config != nil && s.Backends != nil {
		mux.HandleFunc("GET /admin/status", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, s.status())
		})
	}
	if s.Limiter != nil {
		mux.HandleFunc("GET /admin/limits", s.limits)
		mux.HandleFunc("GET /admin/limits/top", s.limitsTop)
		mux.HandleFunc("POST /admin/limits/test", s.limitsTest)
	}
	if s.Pprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	return s.auth(mux)
}

// status assembles the summary from the parts the process already holds.
func (s *Server) status() StatusView {
	now := time.Now()
	v := StatusView{NodeID: s.NodeID, Version: s.Version, StartedAt: s.StartedAt, UptimeSeconds: now.Sub(s.StartedAt).Seconds(), Backends: []BackendStatus{}}
	if s.Ready != nil {
		v.Ready = s.Ready()
	}
	if s.Metrics != nil {
		v.Inflight = s.Metrics.InflightCount.Load()
		v.Requests = s.Metrics.TotalRequests.Load()
		v.Throttled = s.Metrics.TotalThrottled.Load()
	}
	v.Rates = s.sampler.rates()
	if s.Limiter != nil {
		st := s.Limiter.Status()
		v.Rules = len(st)
		for i := range st {
			v.Denied += st[i].Denied
		}
		v.LiveKeys = s.Limiter.Keys()
	}
	if s.Showback != nil {
		v.ShowbackHour, v.Tuples = s.Showback()
	}
	if s.Config != nil {
		cv := s.Config()
		v.Generation, v.LoadedAt, v.LastReload = cv.Generation, cv.LoadedAt, cv.LastReload
	}
	if s.Backends != nil {
		v.Backends = s.Backends()
		for i := range v.Backends {
			if v.Backends[i].Up {
				v.BackendsUp++
			}
		}
	}
	return v
}

// Limits is GET /admin/limits.
type Limits struct {
	Rules    []limiter.RuleStatus `json:"rules"`
	LiveKeys int64                `json:"live_keys"`
}

func (s *Server) limits(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Limits{Rules: s.Limiter.Status(), LiveKeys: s.Limiter.Keys()})
}

// Top is GET /admin/limits/top?n=20&window=60s.
type Top struct {
	WindowSeconds float64            `json:"window_seconds"`
	Top           []limiter.TopEntry `json:"top"`
}

func (s *Server) limitsTop(w http.ResponseWriter, r *http.Request) {
	n := 20
	if v := r.URL.Query().Get("n"); v != "" {
		i, err := strconv.Atoi(v)
		if err != nil || i < 1 || i > 10_000 {
			writeError(w, http.StatusBadRequest, "n must be an integer from 1 to 10000")
			return
		}
		n = i
	}
	window := 60 * time.Second
	if v := r.URL.Query().Get("window"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 || d > 24*time.Hour {
			writeError(w, http.StatusBadRequest, "window must be a duration from 1s to 24h, like 60s")
			return
		}
		window = d
	}
	writeJSON(w, http.StatusOK, Top{WindowSeconds: window.Seconds(), Top: s.Limiter.Top(n, int64(window), s.Limiter.Now(time.Now()))})
}

// TestRequest is the body of POST /admin/limits/test: the identity fields of a hypothetical
// request. Op is an operation name from docs/OPERATIONS_TABLE.md. Prefix is a listing's `prefix`
// parameter, for the four bucket listings (ADR-0001).
type TestRequest struct {
	AccessKey string `json:"access_key"`
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	Prefix    string `json:"prefix"`
	IP        string `json:"ip"`
	Op        string `json:"op"`
}

// TestResponse echoes the identity the limiter evaluated and the dry-run explanation.
type TestResponse struct {
	Operation string              `json:"operation"`
	Class     string              `json:"op_class"`
	Level     string              `json:"level"`
	Result    limiter.Explanation `json:"result"`
}

func (s *Server) limitsTest(w http.ResponseWriter, r *http.Request) {
	var req TestRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "body must be JSON: "+err.Error())
		return
	}
	id, err := identityFor(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, TestResponse{Operation: id.Op.String(), Class: id.Class.String(), Level: id.Level.String(), Result: s.Limiter.Explain(&id, s.Limiter.Now(time.Now()))})
}

// identityFor builds the identity a real request with these fields would carry. The operation
// decides the class; the key and bucket decide the level.
func identityFor(req *TestRequest) (s3id.RequestIdentity, error) {
	var id s3id.RequestIdentity
	if req.Op == "" {
		return id, errors.New("op is required (an operation name from docs/OPERATIONS_TABLE.md)")
	}
	op, ok := s3id.ParseOp(req.Op)
	if !ok {
		return id, errors.New("unknown operation " + strconv.Quote(req.Op) + " (see docs/OPERATIONS_TABLE.md)")
	}
	id.Op, id.Class = op, op.Class()
	id.AccessKey, id.Bucket, id.Key, id.ListPrefix = req.AccessKey, req.Bucket, req.Key, req.Prefix
	switch {
	case req.Key != "":
		id.Level = s3id.LevelObject
	case req.Bucket != "":
		id.Level = s3id.LevelBucket
	}
	if req.IP != "" {
		a, err := netip.ParseAddr(req.IP)
		if err != nil {
			return id, errors.New("ip: " + err.Error())
		}
		id.SourceIP = a.Unmap()
	}
	return id, nil
}

// usage is GET /admin/usage?from=&to=&group_by=&where=&format=json|csv: filter, group, and sum
// over this node's files (docs/SPEC.md, Showback, Query interface). from and to are RFC 3339;
// group_by and where are comma-separated.
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var qry showback.Query
	var err error
	if v := q.Get("from"); v != "" {
		if qry.From, err = time.Parse(time.RFC3339, v); err != nil {
			writeError(w, http.StatusBadRequest, "from: "+err.Error())
			return
		}
	}
	if v := q.Get("to"); v != "" {
		if qry.To, err = time.Parse(time.RFC3339, v); err != nil {
			writeError(w, http.StatusBadRequest, "to: "+err.Error())
			return
		}
	}
	if v := q.Get("group_by"); v != "" {
		qry.GroupBy = strings.Split(v, ",")
	}
	if qry.Where, err = showback.ParseWhere(q.Get("where")); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	files, err := showback.Files(s.Usage.Dir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	res, err := qry.Run(files)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	switch q.Get("format") {
	case "", "json":
		writeJSON(w, http.StatusOK, res)
	case "csv":
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write(showback.CSV(res))
	default:
		writeError(w, http.StatusBadRequest, "format must be json or csv")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
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
