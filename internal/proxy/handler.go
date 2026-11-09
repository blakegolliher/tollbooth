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
// Adapted from github.com/blakegolliher/shunt internal/proxy/handler.go, passthrough path
// (ServeHTTP, preparePassthrough, roundTrip, relay, upstreamError, finish) at commit
// 9b36d09ed109add4a2bedfe1dbfae6108038ee5b (Apache-2.0, Copyright 2026 Blake Golliher).
// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, 2026-11-09: every resign,
// admission, directory, dual-delete, fallback-read, rewrite, httptrace, Via, request-id and
// User-Agent addition removed; the request-target travels in URL.Opaque (or Path/RawPath for a
// leading "//"); the inbound header map is reused; identity comes from s3id; the recover guard
// fails open and re-panics ErrAbortHandler; metrics are pre-resolved handles; the watchdog is
// armed only while body bytes are expected to flow.

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/blakegolliher/tollbooth/internal/admin"
	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/limiter"
	"github.com/blakegolliher/tollbooth/internal/s3id"
	"github.com/blakegolliher/tollbooth/internal/showback"
)

// Handler is the one pipeline. All fields are set once at startup; per-request configuration is
// read from Config at handler entry.
type Handler struct {
	Config   *atomic.Pointer[config.Config]
	Backend  *Pool
	Metrics  *admin.Metrics
	Limiter  *limiter.Limiter     // the rule set in force is read inside Decide; nil means no limiter (tests)
	Showback *showback.Aggregator // every completed request is counted here; nil means off (tests)
	Access   *AccessLogger        // nil: access logging off (the default; showback is the record)
	Log      *slog.Logger

	pool        *bufPool
	inflight    atomic.Int64
	throttleSeq atomic.Uint64 // throttle decisions seen, for the 1-in-N debug log sample
	// Errors counts handler panics that were failed open or aborted; it feeds the log, not a
	// metric of its own (the request lands in requests_total as proxy_error).
	Errors atomic.Int64
}

// New wires a Handler. copyBufferBytes is proxy.copy_buffer_bytes, fixed for the process.
func New(cfg *atomic.Pointer[config.Config], backend *Pool, m *admin.Metrics, lim *limiter.Limiter, sb *showback.Aggregator, access *AccessLogger, log *slog.Logger) *Handler {
	c := cfg.Load()
	return &Handler{Config: cfg, Backend: backend, Metrics: m, Limiter: lim, Showback: sb, Access: access, Log: log, pool: newBufPool(c.Proxy.CopyBufferBytes)}
}

// outcome is filled in as the request progresses and recorded exactly once by finish().
type outcome struct {
	start       time.Time
	id          s3id.RequestIdentity
	status      int    // backend status; 0 when none was produced
	proxyError  bool   // the proxy answered for itself or produced nothing
	throttled   bool   // the limiter denied; status holds the throttle status
	rule        string // the denying rule, for the operator-facing logs only
	endpoint    string // backend endpoint that served the request, "" when none
	ep          *Endpoint
	bytesIn     int64
	bytesOut    int64
	tSent       time.Time // the moment RoundTrip was called
	ttfb        time.Duration
	headersSent bool
	err         string
	recorded    bool
}

// emptyUA suppresses the Transport's default User-Agent when the client sent none. Shared and
// never mutated, so it costs no allocation.
var emptyUA = []string{""}

// ServeHTTP is the pipeline. It panics with http.ErrAbortHandler when a body was cut short, so
// net/http closes the client connection instead of finishing the response (shunt ADR-0003).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cfg := h.Config.Load()
	o := outcome{start: time.Now()}
	h.Metrics.Inflight.Inc()
	h.Metrics.InflightCount.Add(1)
	defer h.Metrics.Inflight.Dec()
	defer h.Metrics.InflightCount.Add(-1)
	defer h.finish(r, &o)
	defer h.guard(w, &o)

	o.id = s3id.Parse(r, cfg.Identity())

	if n := h.inflight.Add(1); n > int64(cfg.Listen.MaxInflight) {
		h.inflight.Add(-1)
		o.proxyError = true
		o.err = "inflight cap"
		writeS3Error(w, CodeSlowDown, o.id.RawPath, time.Second)
		o.headersSent = true
		return
	}
	defer h.inflight.Add(-1)

	// The limiter decision, before any body byte is read. A denial is answered here and the body
	// is never touched (docs/SPEC.md, Request identity: the decision is made before the body).
	if h.Limiter != nil && !cfg.Kill.LimiterDisable {
		if d := h.Limiter.Decide(&o.id, h.Limiter.Now(o.start), true); !d.Allowed {
			h.throttle(w, r, &o, &d, cfg)
			return
		}
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	idle := cfg.Backend.IdleBodyTimeout
	st := &reqState{}
	st.wd.arm(idle, cancel)
	defer st.wd.stop()
	rc := http.NewResponseController(w)

	var inBody *progressReader
	if r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0 {
		st.in = progressReader{r: r.Body, wd: &st.wd, rc: rc, idle: idle}
		inBody = &st.in
		st.wd.kick()
	}

	out, ok := h.build(ctx, r, &o.id, inBody)
	if !ok {
		o.proxyError = true
		o.err = "unforwardable request-target"
		writeS3Error(w, CodeInvalidURI, o.id.RawPath, 0)
		o.headersSent = true
		return
	}

	o.tSent = time.Now()
	resp, err := h.roundTrip(out, inBody, &o)
	if inBody != nil {
		o.bytesIn = inBody.count()
	}
	if err != nil {
		h.backendError(w, r, &o, err)
		return
	}
	o.ttfb = time.Since(o.tSent)
	defer resp.Body.Close() //nolint:errcheck // drained or aborted below
	h.relay(w, resp, &o, st, rc, idle)
}

// throttle answers a denied request with the denying rule's mode: 503 SlowDown or 429
// TooManyRequests, the S3 error body, Retry-After in seconds, x-amz-retry-after in milliseconds
// (D-043), and a request id. Nothing of the body is read;
// Go's server then discards up to 256 KiB of it to keep the connection, closes it past that or
// when the client sent Expect: 100-continue (D-005). The decision is logged at debug, one in
// logging.throttle_sample.
func (h *Handler) throttle(w http.ResponseWriter, r *http.Request, o *outcome, d *limiter.Decision, cfg *config.Config) {
	o.throttled = true
	o.rule = d.Rule.ID
	code := CodeSlowDown
	if d.Mode == limiter.ModeTooManyRequests {
		code = CodeTooManyRequests
	}
	rid := writeS3Error(w, code, o.id.RawPath, time.Duration(d.Wait))
	o.headersSent = true
	o.status = errorTable[code].status
	if h.Log != nil && h.Log.Enabled(r.Context(), slog.LevelDebug) {
		if n := h.throttleSeq.Add(1); cfg.Logging.ThrottleSample <= 1 || n%uint64(cfg.Logging.ThrottleSample) == 1 {
			// No access key here, even at debug: it comes from the Authorization header, and the
			// place to see who is throttled is rules top (D-042).
			h.Log.Debug("throttled", "rule", o.rule, "operation", o.id.Op.String(), "op_class", o.id.Class.String(),
				"status", o.status, "retry_after", d.RetryAfter, "request_id", rid,
				"bucket", o.id.Bucket, "source_ip", o.id.SourceIP.String())
		}
	}
}

// build makes the outbound request: same method, raw path and query, Host preserved, the
// inbound header map reused with hop-by-hop headers removed, no header added. ok is false only
// for a request-target that cannot be forwarded byte-identical (docs/DESIGN.md §4.4).
func (h *Handler) build(ctx context.Context, r *http.Request, id *s3id.RequestIdentity, inBody *progressReader) (*http.Request, bool) {
	u := &url.URL{Scheme: h.Backend.Scheme, RawQuery: id.RawQuery}
	if strings.HasPrefix(id.RawPath, "//") {
		// URL.RequestURI turns an Opaque that begins with "//" into absolute-form, so this one
		// case goes through Path/RawPath, which EscapedPath returns verbatim when validly encoded.
		p, err := url.PathUnescape(id.RawPath)
		if err != nil {
			return nil, false
		}
		u.Path, u.RawPath = p, id.RawPath
		if u.EscapedPath() != id.RawPath {
			return nil, false
		}
	} else {
		u.Opaque = id.RawPath
	}
	stripHopByHop(r.Header)
	if _, ok := r.Header["User-Agent"]; !ok {
		r.Header["User-Agent"] = emptyUA
	}
	if inBody == nil && r.ContentLength == 0 {
		if cl, ok := r.Header["Content-Length"]; ok && len(cl) > 0 && !methodSendsEmptyLength(r.Method) {
			// The client sent an explicit "Content-Length: 0" on a bodiless request, and some
			// clients sign it (aws cli v2 on GET and DELETE; found by the Garage client matrix:
			// "signed header content-length is not present"). Go's Transport writes Content-Length
			// itself and drops the canonical map entry, and for GET, HEAD, DELETE, and OPTIONS with
			// no body it writes none. The header is therefore carried under a non-canonical key,
			// which the request writer does not filter, so the wire carries "content-length: 0"
			// exactly as the client declared it. POST, PUT, and PATCH get it from the Transport.
			r.Header["content-length"] = cl
		}
	}
	out := http.Request{
		Method:        r.Method,
		URL:           u,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        r.Header,
		Host:          r.Host, // exactly as sent; it is in SignedHeaders
		ContentLength: r.ContentLength,
		Trailer:       r.Trailer,
	}
	if inBody != nil {
		out.Body = inBody
	}
	return out.WithContext(ctx), true
}

// methodSendsEmptyLength reports whether net/http's Transport writes "Content-Length: 0" by
// itself for a bodiless request of this method.
func methodSendsEmptyLength(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch
}

// roundTrip sends the request, moving to the next endpoint when one refuses the connection
// before any request byte was sent. Never after bytes have gone out, regardless of method.
func (h *Handler) roundTrip(out *http.Request, inBody *progressReader, o *outcome) (*http.Response, error) {
	for attempt := 1; ; attempt++ {
		ep := h.Backend.Next()
		out.URL.Host = ep.Addr
		o.endpoint = ep.Addr
		o.ep = ep
		ep.inflight.Add(1)
		t0 := time.Now()
		resp, err := h.Backend.Transport.RoundTrip(out)
		ep.inflight.Add(-1)
		if err == nil {
			ep.observe(time.Since(t0))
			return resp, nil
		}
		kind := errorKind(err)
		ep.countError(kind)
		if IsConnectError(err) {
			h.Backend.MarkDown(ep, err, kind)
			if attempt < len(h.Backend.Endpoints) && (inBody == nil || inBody.count() == 0) {
				h.Metrics.BackendRetries.Inc()
				continue
			}
			return nil, err
		}
		h.Metrics.BackendErrors.WithLabelValues(ep.Addr, kind).Inc()
		return nil, err
	}
}

// backendError answers when no backend response was produced: 503 ServiceUnavailable with the
// S3 body, counted as proxy_error. A client that went away gets nothing. If part of the client
// body was consumed, net/http closes the client connection after the handler returns.
func (h *Handler) backendError(w http.ResponseWriter, r *http.Request, o *outcome, err error) {
	o.proxyError = true
	o.err = "backend: " + err.Error()
	if r.Context().Err() != nil {
		return // the client left; no response can be delivered
	}
	writeS3Error(w, CodeServiceUnavailable, o.id.RawPath, 0)
	o.headersSent = true
}

// relay sends the backend response to the client: headers shared, nothing added, body streamed
// through the pooled buffer with every chunk flushed. A body cut short aborts the connection.
func (h *Handler) relay(w http.ResponseWriter, resp *http.Response, o *outcome, st *reqState, rc *http.ResponseController, idle time.Duration) {
	copyResponseHeaders(w.Header(), resp.Header)
	if _, ok := resp.Header["Content-Type"]; !ok {
		// Transparency: net/http sniffs a Content-Type onto any body that lacks one. The backend
		// sent none, so the client must see none. A nil value disables the sniffing.
		w.Header()["Content-Type"] = nil
	}
	o.status = resp.StatusCode
	w.WriteHeader(resp.StatusCode)
	o.headersSent = true

	buf := h.pool.get()
	defer h.pool.put(buf)
	st.dst = deadlineWriter{w: w, rc: rc, idle: idle}
	st.out = progressReader{r: resp.Body, wd: &st.wd}
	st.wd.kick()
	_, copyErr := io.CopyBuffer(&st.dst, &st.out, *buf)
	o.bytesOut = st.dst.n
	if copyErr != nil {
		o.err = "body: " + copyErr.Error()
		if resp.ContentLength >= 0 && st.dst.n == resp.ContentLength {
			return // client-side write error after a complete body; nothing to hide
		}
		// Upstream died or the client stopped reading: never let net/http finish the response
		// as if it were complete. finish() runs in the deferred chain.
		panic(http.ErrAbortHandler)
	}
}

// guard is the one recover point. ErrAbortHandler passes through so a truncated body is never
// completed; any other panic is failed open: a 503 if nothing was written yet, an abort if the
// response had started.
func (h *Handler) guard(w http.ResponseWriter, o *outcome) {
	p := recover()
	if p == nil {
		return
	}
	if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
		panic(p)
	}
	h.Errors.Add(1)
	o.proxyError = true
	o.err = fmt.Sprintf("panic: %v", p)
	if h.Log != nil {
		h.Log.Error("handler panic, failing open", "operation", o.id.Op.String(), "err", o.err)
	}
	if o.headersSent {
		panic(http.ErrAbortHandler)
	}
	writeS3Error(w, CodeServiceUnavailable, o.id.RawPath, 0)
	o.headersSent = true
}

// finish records the request exactly once: the pre-resolved counters, the three latencies, the
// byte counters, the showback increment, and the access log line when enabled.
func (h *Handler) finish(r *http.Request, o *outcome) {
	if o.recorded {
		return
	}
	o.recorded = true
	total := time.Since(o.start)
	overhead := time.Duration(0)
	if !o.tSent.IsZero() {
		overhead = o.tSent.Sub(o.start)
	} else {
		overhead = total
	}
	sc, res := admin.StatusProxyError, admin.ResultError
	switch {
	case o.throttled:
		sc, res = admin.StatusThrottled, admin.ResultThrottled
	case !o.proxyError && o.status != 0:
		sc, res = admin.ClassifyStatus(o.status), admin.ResultForwarded
	}
	h.Metrics.Request(o.id.Op, sc, res).Inc()
	h.Metrics.Observe(o.id.Class, total.Seconds(), overhead.Seconds(), o.ttfb.Seconds())
	h.Metrics.AddBytes(o.bytesIn, o.bytesOut)
	h.Metrics.TotalRequests.Add(1)
	if o.throttled {
		h.Metrics.TotalThrottled.Add(1)
	}
	if h.Showback != nil && !h.Config.Load().Kill.ShowbackDisable {
		// Once, at completion, with the bytes actually moved; the hour is read inside Record (D-025).
		h.Showback.Record(&o.id, showback.Status(sc), o.bytesIn, o.bytesOut)
	}
	if h.Access != nil {
		h.Access.Log(&Access{
			Client: o.id.SourceIP, Method: r.Method, Host: r.Host, Addressing: o.id.Addressing.String(),
			Bucket: o.id.Bucket, Op: o.id.Op.String(), Class: o.id.Class.String(), Status: o.status,
			StatusClass: sc.String(), Result: res.String(), BytesIn: o.bytesIn, BytesOut: o.bytesOut,
			Duration: total, TTFB: o.ttfb, Endpoint: o.endpoint, TLS: tlsVersion(r), Error: o.err, Rule: o.rule,
		})
	}
}

func tlsVersion(r *http.Request) string {
	if r.TLS == nil {
		return ""
	}
	switch r.TLS.Version {
	case 0x0303:
		return "1.2"
	case 0x0304:
		return "1.3"
	}
	return "other"
}

// NewServer builds the data http.Server around h: the handler itself, never a mux (docs/DESIGN.md
// §4.1), HTTP/1.1 only, timeouts from config, open connections counted.
func NewServer(h *Handler, c *config.Listen, m *admin.Metrics, log *slog.Logger) *http.Server {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: c.ReadHeaderTimeout,
		IdleTimeout:       c.IdleTimeout,
		MaxHeaderBytes:    c.MaxHeaderBytes,
		ConnState: func(_ net.Conn, st http.ConnState) {
			switch st {
			case http.StateNew:
				m.Connections(admin.SideClient).Inc()
			case http.StateClosed, http.StateHijacked:
				m.Connections(admin.SideClient).Dec()
			}
		},
	}
	if log != nil {
		srv.ErrorLog = slog.NewLogLogger(log.Handler(), slog.LevelWarn)
	}
	return srv
}
