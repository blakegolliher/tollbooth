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
// Adapted from github.com/blakegolliher/shunt internal/upstream/cluster.go (Cluster, New, Next,
// IsConnectError) at commit 9b36d09ed109add4a2bedfe1dbfae6108038ee5b (Apache-2.0, Copyright
// 2026 Blake Golliher).
// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, 2026-11-09: one pool, no
// credentials or capability profile, no Set or Registry; active health checks (TCP connect plus
// HEAD /, any HTTP status is healthy) and passive marking on dial errors; Next skips endpoints
// that are down; ResponseHeaderTimeout from config.

package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blakegolliher/tollbooth/internal/admin"
	"github.com/blakegolliher/tollbooth/internal/config"
)

// Endpoint is one backend address, its health, and the counters /admin/backends shows.
type Endpoint struct {
	Addr string // host:port
	up   atomic.Bool
	// host and port are Addr split; host is "" when Addr is an IP literal, which is dialed as it
	// is and never looked up.
	host, port string

	// lastErr is the most recent failure, for /admin/backends. addrs is what host last resolved
	// to, kept for when the resolver does not answer (ADR-0008); failingSince is when it stopped.
	mu           sync.Mutex
	lastErr      string
	lastAt       time.Time
	addrs        []netip.Addr
	failingSince time.Time
	rotate       int

	inflight  atomic.Int64
	requests  atomic.Int64    // responses received
	errors    [6]atomic.Int64 // by errorKind: dial, tls, timeout, reset, other, resolve
	ttfbSumNS atomic.Int64
	ttfbLast  atomic.Int64
}

// errorKinds indexes Endpoint.errors; the names are the backend_errors_total kind label.
// "other" stays at index 4: countError files an unknown kind there.
var errorKinds = [...]string{"dial", "tls", "timeout", "reset", "other", "resolve"}

func (e *Endpoint) countError(kind string) {
	for i, k := range errorKinds {
		if k == kind {
			e.errors[i].Add(1)
			return
		}
	}
	e.errors[4].Add(1)
}

// observe records one response's time to first header byte.
func (e *Endpoint) observe(ttfb time.Duration) {
	e.requests.Add(1)
	e.ttfbSumNS.Add(int64(ttfb))
	e.ttfbLast.Store(int64(ttfb))
}

// status is this endpoint's row.
func (e *Endpoint) status() admin.BackendStatus {
	st := admin.BackendStatus{Endpoint: e.Addr, Up: e.up.Load(), Inflight: e.inflight.Load(), Requests: e.requests.Load()}
	st.ErrorsDial, st.ErrorsTLS, st.ErrorsTime, st.ErrorsRst, st.ErrorsOth = e.errors[0].Load(), e.errors[1].Load(), e.errors[2].Load(), e.errors[3].Load(), e.errors[4].Load()
	st.ErrorsResolve = e.errors[5].Load()
	st.Errors = st.ErrorsDial + st.ErrorsTLS + st.ErrorsTime + st.ErrorsRst + st.ErrorsOth + st.ErrorsResolve
	e.mu.Lock()
	for _, a := range e.addrs {
		st.Addresses = append(st.Addresses, a.String())
	}
	if !e.failingSince.IsZero() {
		st.ResolverFailingSince = e.failingSince.UTC().Format(time.RFC3339)
	}
	e.mu.Unlock()
	if st.Requests > 0 {
		st.TTFBMeanMS = float64(e.ttfbSumNS.Load()) / float64(st.Requests) / 1e6
		st.TTFBLastMS = float64(e.ttfbLast.Load()) / 1e6
	}
	if msg, at := e.LastError(); msg != "" {
		st.LastError, st.LastErrAt = msg, at.UTC().Format(time.RFC3339)
	}
	return st
}

// Up reports the endpoint's last known health.
func (e *Endpoint) Up() bool { return e.up.Load() }

// LastError returns the most recent failure and when it happened.
func (e *Endpoint) LastError() (string, time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastErr, e.lastAt
}

func (e *Endpoint) fail(err error) {
	e.mu.Lock()
	e.lastErr, e.lastAt = err.Error(), time.Now()
	e.mu.Unlock()
}

// Pool is the backend: its endpoints, one http.Transport, and the health checker.
type Pool struct {
	Scheme    string
	Endpoints []*Endpoint
	// Transport is the http.Transport below; tests substitute a fake RoundTripper to count the
	// handler's own allocations without sockets.
	Transport http.RoundTripper
	tr        *http.Transport

	// lookup resolves a backend's name; tests replace it. byAddr finds the endpoint a dial is for.
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	byAddr map[string]*Endpoint

	metrics *admin.Metrics
	log     *slog.Logger
	check   config.HealthCheck
	dial    time.Duration
	tlsConf *tls.Config
	next    atomic.Uint64
	stop    chan struct{}
	done    sync.WaitGroup
}

// NewPool builds the pool from config. The transport is fixed for the life of the process
// (backend.* keys are restart-only, D-029).
func NewPool(c *config.Backend, m *admin.Metrics, log *slog.Logger) (*Pool, error) {
	if len(c.Endpoints) == 0 {
		return nil, errors.New("backend: no endpoints")
	}
	p := &Pool{Scheme: c.Scheme, metrics: m, log: log, check: c.HealthCheck, dial: c.DialTimeout, stop: make(chan struct{}), byAddr: make(map[string]*Endpoint, len(c.Endpoints))}
	p.lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	tr := &http.Transport{
		Proxy:                 nil, // never the environment's HTTP proxy
		DialContext:           p.dialer(c.DialTimeout),
		ForceAttemptHTTP2:     false, // HTTP/1.1 only, on both sides
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConns:          c.MaxIdleConnsPerHost * len(c.Endpoints),
		MaxIdleConnsPerHost:   c.MaxIdleConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: c.ResponseHeaderTimeout,
		DisableCompression:    true, // bodies are opaque; never let the transport add Accept-Encoding
	}
	var tc *tls.Config
	if c.Scheme == "https" {
		tc = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.TLS.InsecureSkipVerify} //nolint:gosec // operator opt-in, validated and warned about by check-config
		if c.TLS.CABundle != "" {
			pem, err := os.ReadFile(c.TLS.CABundle)
			if err != nil {
				return nil, fmt.Errorf("backend: read ca_bundle: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("backend: no certificates in %s", c.TLS.CABundle)
			}
			tc.RootCAs = pool
		}
		tr.TLSClientConfig = tc
	}
	p.Transport, p.tr, p.tlsConf = tr, tr, tc
	for _, a := range c.Endpoints {
		e := &Endpoint{Addr: a}
		if host, port, err := net.SplitHostPort(a); err == nil {
			if _, perr := netip.ParseAddr(host); perr != nil {
				e.host, e.port = host, port // a name: looked up on every dial, remembered for when that fails
			}
		}
		e.up.Store(true) // optimistic until the first check; a dial error marks it down at once
		p.Endpoints = append(p.Endpoints, e)
		p.byAddr[a] = e
		if m != nil {
			m.BackendUp.WithLabelValues(a).Set(1)
		}
	}
	return p, nil
}

// Next returns the next endpoint round-robin, skipping ones marked down. With every endpoint
// down it returns the next one anyway, so the request fails with a real error and the passive
// path keeps probing.
func (p *Pool) Next() *Endpoint {
	n := len(p.Endpoints)
	start := p.next.Add(1) - 1
	for i := 0; i < n; i++ {
		e := p.Endpoints[(start+uint64(i))%uint64(n)]
		if e.up.Load() {
			return e
		}
	}
	return p.Endpoints[start%uint64(n)]
}

// Status lists every endpoint's row for /admin/backends and /admin/status.
func (p *Pool) Status() []admin.BackendStatus {
	out := make([]admin.BackendStatus, 0, len(p.Endpoints))
	for _, e := range p.Endpoints {
		out = append(out, e.status())
	}
	return out
}

// Healthy reports whether at least one endpoint is up (for /readyz).
func (p *Pool) Healthy() bool {
	for _, e := range p.Endpoints {
		if e.up.Load() {
			return true
		}
	}
	return false
}

// MarkDown records a failure on e and takes it out of rotation until a health check passes.
func (p *Pool) MarkDown(e *Endpoint, err error, kind string) {
	e.fail(err)
	if e.up.CompareAndSwap(true, false) {
		if p.metrics != nil {
			p.metrics.BackendUp.WithLabelValues(e.Addr).Set(0)
		}
		if p.log != nil {
			p.log.Warn("backend endpoint marked down", "endpoint", e.Addr, "kind", kind, "err", err.Error())
		}
	}
	if p.metrics != nil && kind != "resolve" { // a failed lookup was counted where it failed (unresolved)
		p.metrics.BackendErrors.WithLabelValues(e.Addr, kind).Inc()
	}
}

func (p *Pool) markUp(e *Endpoint) {
	if e.up.CompareAndSwap(false, true) {
		if p.metrics != nil {
			p.metrics.BackendUp.WithLabelValues(e.Addr).Set(1)
		}
		if p.log != nil {
			p.log.Info("backend endpoint healthy again", "endpoint", e.Addr)
		}
	}
}

// Start runs the active health checker: every interval, each endpoint gets a TCP connect and a
// HEAD / with the check timeout; any HTTP status, 403 included, means healthy. Stop ends it.
func (p *Pool) Start() {
	for _, e := range p.Endpoints {
		p.done.Add(1)
		go p.checkLoop(e)
	}
}

// Stop ends the health checker and closes idle connections.
func (p *Pool) Stop() {
	close(p.stop)
	p.done.Wait()
	p.tr.CloseIdleConnections()
}

func (p *Pool) checkLoop(e *Endpoint) {
	defer p.done.Done()
	// A dedicated transport so a check never competes with the data path for a pooled connection
	// and never leaves one open.
	tr := &http.Transport{
		DialContext:       p.dialer(p.check.Timeout),
		TLSClientConfig:   p.tlsConf,
		DisableKeepAlives: true,
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	client := &http.Client{Transport: tr, Timeout: p.check.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t := time.NewTicker(p.check.Interval)
	defer t.Stop()
	for {
		p.checkOnce(e, client)
		select {
		case <-p.stop:
			return
		case <-t.C:
		}
	}
}

func (p *Pool) checkOnce(e *Endpoint, client *http.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), p.check.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, p.Scheme+"://"+e.Addr+"/", http.NoBody)
	if err != nil {
		p.MarkDown(e, err, "other")
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		p.MarkDown(e, err, errorKind(err))
		return
	}
	_ = resp.Body.Close() // HEAD has no body
	p.markUp(e)
}

// dialer returns the DialContext both transports use (ADR-0008). An endpoint written as an
// address is dialed as it is. A named one is looked up here, within half of timeout so that a
// resolver that hangs leaves time to connect; the answer is remembered and dialed, and when the
// lookup fails the last answer is dialed instead. Only with no answer ever does the dial fail for
// want of a name, as a connect error of kind "resolve".
func (p *Pool) dialer(timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		e := p.byAddr[addr]
		if e == nil || e.host == "" {
			return d.DialContext(ctx, network, addr)
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		lctx, lcancel := context.WithTimeout(ctx, timeout/2)
		ips, err := p.lookup(lctx, e.host)
		lcancel()
		if err != nil && ctx.Err() != nil {
			return nil, ctx.Err() // the caller gave up; that says nothing about the resolver
		}
		if err == nil && len(ips) == 0 {
			err = &net.DNSError{Err: "no addresses", Name: e.host, IsNotFound: true}
		}
		if err == nil {
			p.resolved(e, ips)
		} else if ips = p.unresolved(e, err); len(ips) == 0 {
			return nil, &net.OpError{Op: "dial", Net: network, Err: err}
		}
		var last error
		for i, ip := range ips {
			actx, acancel := ctx, context.CancelFunc(func() {})
			if dl, ok := ctx.Deadline(); ok && i < len(ips)-1 {
				// Divide what is left among the addresses still to try, as net.Dialer does.
				actx, acancel = context.WithTimeout(ctx, time.Until(dl)/time.Duration(len(ips)-i))
			}
			c, derr := d.DialContext(actx, network, net.JoinHostPort(ip.String(), e.port))
			acancel()
			if derr == nil {
				return c, nil
			}
			last = derr
		}
		return nil, last
	}
}

// resolved remembers what the endpoint's name resolved to and ends a resolver failure.
func (p *Pool) resolved(e *Endpoint, ips []netip.Addr) {
	e.mu.Lock()
	recovered := !e.failingSince.IsZero()
	e.addrs, e.failingSince = append(e.addrs[:0], ips...), time.Time{}
	e.mu.Unlock()
	if recovered && p.log != nil {
		p.log.Info("resolver answering again for backend endpoint", "endpoint", e.Addr)
	}
}

// unresolved records a failed lookup and returns the addresses to dial instead: the last answer,
// starting at a rotating position, or none when the name has never resolved. The failure is
// counted and shown and does not change the endpoint's health; the dial that follows decides that.
// This is the one place a failed lookup is counted, for the endpoint's row and for
// backend_errors_total{kind="resolve"}, whichever path dialed and whether or not an address was
// known; the callers that count other kinds skip this one.
func (p *Pool) unresolved(e *Endpoint, err error) []netip.Addr {
	e.mu.Lock()
	first := e.failingSince.IsZero()
	if first {
		e.failingSince = time.Now()
	}
	n := len(e.addrs)
	out := make([]netip.Addr, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, e.addrs[(e.rotate+i)%n])
	}
	if n > 0 {
		e.rotate = (e.rotate + 1) % n
		e.lastErr, e.lastAt = fmt.Sprintf("resolver: %v; using the last known address %s", err, out[0]), time.Now()
	}
	e.mu.Unlock()
	e.countError("resolve")
	if p.metrics != nil {
		p.metrics.BackendErrors.WithLabelValues(e.Addr, "resolve").Inc()
	}
	if first && p.log != nil {
		if n > 0 {
			p.log.Warn("resolver failing for backend endpoint; using its last known addresses", "endpoint", e.Addr, "addresses", fmt.Sprint(out), "err", err.Error())
		} else {
			p.log.Warn("resolver failing for backend endpoint and no address is known", "endpoint", e.Addr, "err", err.Error())
		}
	}
	return out
}

// IsConnectError reports whether err is a failure to connect to an endpoint (dial or TLS
// handshake): no request byte was sent, so the request may be retried on another endpoint.
func IsConnectError(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var rec *tls.RecordHeaderError
	var cv *tls.CertificateVerificationError
	var ae *tls.AlertError
	return errors.As(err, &rec) || errors.As(err, &cv) || errors.As(err, &ae)
}

// errorKind labels a backend failure for tollbooth_backend_errors_total{kind}.
func errorKind(err error) string {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "resolve" // the name did not resolve and no address was known (ADR-0008)
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return "dial"
	}
	var rec *tls.RecordHeaderError
	var cv *tls.CertificateVerificationError
	var ae *tls.AlertError
	if errors.As(err, &rec) || errors.As(err, &cv) || errors.As(err, &ae) {
		return "tls"
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return "timeout"
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
		return "reset"
	}
	return "other"
}
