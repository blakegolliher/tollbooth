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
//
// Adapted from github.com/blakegolliher/shunt internal/telemetry/metrics.go (Metrics, the
// log-spaced bucket builder, StatusClass) at commit 9b36d09ed109add4a2bedfe1dbfae6108038ee5b
// (Apache-2.0, Copyright 2026 Blake Golliher).
// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, 2026-11-09: tollbooth's
// catalog; every per-request handle is resolved once at construction and indexed by enum on the
// hot path (docs/DESIGN.md §9.3), so an observation allocates nothing.

package admin

import (
	"math"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/blakegolliher/tollbooth/internal/limiter"
	"github.com/blakegolliher/tollbooth/internal/s3id"
	"github.com/blakegolliher/tollbooth/internal/showback"
)

// StatusClass is the showback and metrics status partition (docs/SPEC.md, Showback).
type StatusClass uint8

// Status classes.
const (
	Status2xx StatusClass = iota
	Status3xx
	Status4xx
	Status5xx
	StatusThrottled
	StatusProxyError
	numStatus
)

var statusNames = [...]string{"2xx", "3xx", "4xx", "5xx", "throttled", "proxy_error"}

func (s StatusClass) String() string {
	if int(s) < len(statusNames) {
		return statusNames[s]
	}
	return "proxy_error"
}

// ClassifyStatus maps a backend HTTP status to its class. Anything outside 200–599 is a proxy
// error (no response was produced).
func ClassifyStatus(status int) StatusClass {
	switch {
	case status >= 200 && status < 300:
		return Status2xx
	case status >= 300 && status < 400:
		return Status3xx
	case status >= 400 && status < 500:
		return Status4xx
	case status >= 500 && status < 600:
		return Status5xx
	}
	return StatusProxyError
}

// Result is what the proxy did with a request.
type Result uint8

// Results. ResultShaped is reserved for the Phase 6 hold-and-forward mode and is never
// incremented in v1 (docs/telemetry-catalog.md).
const (
	ResultForwarded Result = iota
	ResultThrottled
	ResultShaped
	ResultError
	numResult
)

var resultNames = [...]string{"forwarded", "throttled", "shaped", "error"}

func (r Result) String() string {
	if int(r) < len(resultNames) {
		return resultNames[r]
	}
	return "error"
}

// Metrics holds every catalog metric the request path and the proxy own. Names must have a row in
// docs/telemetry-catalog.md (TestEveryMetricIsInTheCatalog) with exactly these labels
// (TestLabelSetsMatchTheCatalog).
type Metrics struct {
	Registry *prometheus.Registry

	requestsTotal   *prometheus.CounterVec   // tollbooth_requests_total{operation,op_class,status_class,result}
	requestDuration *prometheus.HistogramVec // tollbooth_request_duration_seconds{op_class}
	proxyOverhead   *prometheus.HistogramVec // tollbooth_proxy_overhead_seconds{op_class}
	backendTTFB     *prometheus.HistogramVec // tollbooth_backend_ttfb_seconds{op_class}
	bytesTotal      *prometheus.CounterVec   // tollbooth_bytes_total{direction}
	connectionsOpen *prometheus.GaugeVec     // tollbooth_connections_open{side}

	// Pre-resolved handles, indexed by enum.
	requests  [s3id.NumOps][numStatus][numResult]prometheus.Counter
	duration  [s3id.NumClasses]prometheus.Observer
	overhead  [s3id.NumClasses]prometheus.Observer
	ttfb      [s3id.NumClasses]prometheus.Observer
	bytesIn   prometheus.Counter
	bytesOut  prometheus.Counter
	connsOpen [2]prometheus.Gauge

	Inflight       prometheus.Gauge       // tollbooth_inflight_requests
	BackendUp      *prometheus.GaugeVec   // tollbooth_backend_up{endpoint}
	BackendErrors  *prometheus.CounterVec // tollbooth_backend_errors_total{endpoint,kind}
	BackendRetries prometheus.Counter     // tollbooth_backend_retries_total
	ConfigReload   *prometheus.CounterVec // tollbooth_config_reload_total{result}

	// Limiter holds the five limiter rows of the catalog; internal/limiter observes them and never
	// registers anything itself (Phase 2). The shaping histogram arrives with shaping (ADR-0004).
	Limiter limiter.Metrics
	// Showback holds the five showback rows of the catalog (Phase 3); internal/showback observes.
	Showback showback.Metrics

	// Plain counters the status summary and its sampler read: every completed request, the
	// throttled ones, and the in-flight count. Incremented by the handler next to the Prometheus
	// counters; an atomic add each, nothing allocated.
	TotalRequests  atomic.Int64
	TotalThrottled atomic.Int64
	InflightCount  atomic.Int64
}

// Side of a connection gauge.
type Side uint8

// Sides.
const (
	SideClient Side = iota
	SideBackend
)

// logBuckets returns n log-spaced bucket boundaries from lo to hi seconds, rounded to 1 ns.
func logBuckets(lo, hi float64, n int) []float64 {
	out := make([]float64, n)
	l, h := math.Log(lo), math.Log(hi)
	for i := range out {
		v := math.Exp(l + (h-l)*float64(i)/float64(n-1))
		out[i] = math.Round(v*1e9) / 1e9
	}
	return out
}

// NewMetrics registers the request-path, backend, reload, and limiter catalog metrics plus the Go
// and process collectors on a private registry and resolves every per-request handle.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tollbooth_requests_total", Help: "Requests completed, by operation, class, response status class, and what the proxy did (forwarded, throttled, shaped, error).",
		}, []string{"operation", "op_class", "status_class", "result"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "tollbooth_request_duration_seconds", Help: "Client-observed duration, first header byte in to last body byte out.",
			Buckets: logBuckets(100e-6, 600, 20),
		}, []string{"op_class"}),
		proxyOverhead: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "tollbooth_proxy_overhead_seconds", Help: "Handler entry to the moment RoundTrip is called (identity, classify, limiter, build).",
			Buckets: logBuckets(10e-6, 0.1, 16),
		}, []string{"op_class"}),
		backendTTFB: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "tollbooth_backend_ttfb_seconds", Help: "RoundTrip call to response headers received.",
			Buckets: logBuckets(1e-3, 600, 20),
		}, []string{"op_class"}),
		bytesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tollbooth_bytes_total", Help: "Body bytes read from clients (in) and written to clients (out); headers excluded.",
		}, []string{"direction"}),
		connectionsOpen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tollbooth_connections_open", Help: "Open TCP connections on each side.",
		}, []string{"side"}),
		Inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tollbooth_inflight_requests", Help: "Requests between handler entry and completion.",
		}),
		BackendUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tollbooth_backend_up", Help: "1 when the endpoint passed its last health check, else 0.",
		}, []string{"endpoint"}),
		BackendErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tollbooth_backend_errors_total", Help: "Backend request failures by endpoint and kind (dial, tls, timeout, reset, other).",
		}, []string{"endpoint", "kind"}),
		BackendRetries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tollbooth_backend_retries_total", Help: "Requests moved to another endpoint after a connect or TLS error before any request byte was sent.",
		}),
		ConfigReload: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tollbooth_config_reload_total", Help: "Reload attempts via SIGHUP or POST /admin/reload, by result (ok, error).",
		}, []string{"result"}),
	}
	m.Limiter = limiter.Metrics{
		Decision: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "tollbooth_limiter_decision_seconds", Help: "Time for the whole rule pass (match plus GCRA for every matching rule).",
			Buckets: logBuckets(100e-9, 1e-3, 14),
		}),
		Keys: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tollbooth_limiter_keys", Help: "Live GCRA keys across all rules.",
		}),
		Overflow: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tollbooth_limiter_keys_overflow_total", Help: "Requests allowed without state because limiter.max_keys was reached (fail open).",
		}),
		Denied: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tollbooth_limiter_denied_total", Help: "Requests denied, by the rule that denied them.",
		}, []string{"rule"}),
		Errors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tollbooth_limiter_errors_total", Help: "Panics or internal errors inside the limiter, each failed open.",
		}),
	}
	m.Showback = showback.Metrics{
		Tuples: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tollbooth_showback_tuples", Help: "Distinct tuples in the open hour.",
		}),
		FlushDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "tollbooth_showback_flush_duration_seconds", Help: "Time to snapshot or seal one hour file (encode, fsync, rename).",
			Buckets: logBuckets(1e-3, 60, 16),
		}),
		FlushErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tollbooth_showback_flush_errors_total", Help: "Snapshot or seal attempts that failed (disk full, read-only, I/O error); counting continued in memory.",
		}),
		Collapsed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tollbooth_showback_collapsed_total", Help: "Requests folded into the __overflow__ row because showback.max_tuples_per_hour was reached.",
		}),
		ClockSkew: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tollbooth_showback_clock_skew_total", Help: "Requests whose completion hour was earlier than the open hour (wall clock stepped back); counted in the open hour.",
		}),
	}
	reg.MustRegister(m.requestsTotal, m.requestDuration, m.proxyOverhead, m.backendTTFB, m.bytesTotal, m.connectionsOpen,
		m.Inflight, m.BackendUp, m.BackendErrors, m.BackendRetries, m.ConfigReload,
		m.Limiter.Decision.(prometheus.Collector), m.Limiter.Keys, m.Limiter.Overflow, m.Limiter.Denied, m.Limiter.Errors,
		m.Showback.Tuples, m.Showback.FlushDuration.(prometheus.Collector), m.Showback.FlushErrors, m.Showback.Collapsed, m.Showback.ClockSkew,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	// Resolve every per-request handle once. The cardinality is fixed by construction:
	// 113 × 6 × 4 request counters, 6 × 3 histogram children, 2 byte counters, 2 gauges.
	for o := range m.requests {
		op := s3id.Op(o)
		for sc := range m.requests[o] {
			for res := range m.requests[o][sc] {
				m.requests[o][sc][res] = m.requestsTotal.WithLabelValues(op.String(), op.Class().String(), StatusClass(sc).String(), Result(res).String())
			}
		}
	}
	for c := range m.duration {
		name := s3id.Class(c).String()
		m.duration[c] = m.requestDuration.WithLabelValues(name)
		m.overhead[c] = m.proxyOverhead.WithLabelValues(name)
		m.ttfb[c] = m.backendTTFB.WithLabelValues(name)
	}
	m.bytesIn = m.bytesTotal.WithLabelValues("in")
	m.bytesOut = m.bytesTotal.WithLabelValues("out")
	m.connsOpen[SideClient] = m.connectionsOpen.WithLabelValues("client")
	m.connsOpen[SideBackend] = m.connectionsOpen.WithLabelValues("backend")
	// The two reload results exist from the start so a dashboard sees zeros, not absence.
	m.ConfigReload.WithLabelValues("ok")
	m.ConfigReload.WithLabelValues("error")
	return m
}

// Request returns the pre-resolved counter for one completed request. Zero allocations.
func (m *Metrics) Request(op s3id.Op, sc StatusClass, res Result) prometheus.Counter {
	if op >= s3id.Op(s3id.NumOps) {
		op = s3id.OpUnknown
	}
	if sc >= numStatus {
		sc = StatusProxyError
	}
	if res >= numResult {
		res = ResultError
	}
	return m.requests[op][sc][res]
}

// Observe records the three per-request latencies for a class, in seconds. A zero ttfb means
// no backend round trip happened and is not observed.
func (m *Metrics) Observe(class s3id.Class, duration, overhead, ttfb float64) {
	if int(class) >= s3id.NumClasses {
		class = s3id.ClassUnknown
	}
	m.duration[class].Observe(duration)
	m.overhead[class].Observe(overhead)
	if ttfb > 0 {
		m.ttfb[class].Observe(ttfb)
	}
}

// AddBytes adds body bytes in each direction.
func (m *Metrics) AddBytes(in, out int64) {
	if in > 0 {
		m.bytesIn.Add(float64(in))
	}
	if out > 0 {
		m.bytesOut.Add(float64(out))
	}
}

// Connections returns the open-connection gauge for a side.
func (m *Metrics) Connections(side Side) prometheus.Gauge {
	if side > SideBackend {
		side = SideBackend
	}
	return m.connsOpen[side]
}
