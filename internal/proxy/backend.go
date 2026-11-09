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
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blakegolliher/tollbooth/internal/admin"
	"github.com/blakegolliher/tollbooth/internal/config"
)

// Endpoint is one backend address and its health.
type Endpoint struct {
	Addr string // host:port
	up   atomic.Bool
	// lastErr is the most recent failure, for /admin/backends (Phase 4).
	mu      sync.Mutex
	lastErr string
	lastAt  time.Time
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
	tr := &http.Transport{
		Proxy:                 nil, // never the environment's HTTP proxy
		DialContext:           (&net.Dialer{Timeout: c.DialTimeout, KeepAlive: 30 * time.Second}).DialContext,
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
	p := &Pool{Scheme: c.Scheme, Transport: tr, tr: tr, metrics: m, log: log, check: c.HealthCheck, dial: c.DialTimeout, tlsConf: tc, stop: make(chan struct{})}
	for _, a := range c.Endpoints {
		e := &Endpoint{Addr: a}
		e.up.Store(true) // optimistic until the first check; a dial error marks it down at once
		p.Endpoints = append(p.Endpoints, e)
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
	if p.metrics != nil {
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
		DialContext:       (&net.Dialer{Timeout: p.check.Timeout}).DialContext,
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
