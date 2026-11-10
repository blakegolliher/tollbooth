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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
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
		"resolve": &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "b.test", IsNotFound: true}},
		"timeout": context.DeadlineExceeded,
		"reset":   context.Canceled,
		"other":   errors.New("something else"),
	}
	for want, err := range cases {
		if got := errorKind(err); got != want {
			t.Errorf("errorKind(%v) = %s, want %s", err, got, want)
		}
	}
	if !IsConnectError(cases["resolve"]) {
		t.Error("a name that does not resolve, with no address known, is a connect error: no byte was sent")
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

// ---- ADR-0008: backends through a resolver outage

// fakeResolver stands in for DNS: it answers with ips, fails with err, or hangs until the
// lookup's own deadline.
type fakeResolver struct {
	mu    sync.Mutex
	ips   []netip.Addr
	err   error
	hang  bool
	calls int
}

func (f *fakeResolver) set(ips []netip.Addr, err error, hang bool) {
	f.mu.Lock()
	f.ips, f.err, f.hang = ips, err, hang
	f.mu.Unlock()
}

func (f *fakeResolver) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	f.calls++
	ips, err, hang := f.ips, f.err, f.hang
	f.mu.Unlock()
	if hang {
		<-ctx.Done()
		return nil, &net.DNSError{Err: "i/o timeout", Name: host, IsTimeout: true}
	}
	return ips, err
}

var errNoResolver = &net.DNSError{Err: "server misbehaving", Name: "backend.test", IsTemporary: true}

// namedBackend serves on 127.0.0.1 and is reached as backend.test:<port> through fr. served
// counts the requests of get, not the health probe's HEAD /.
func namedBackend(t *testing.T, fr *fakeResolver) (pool *Pool, m *admin.Metrics, served func() int, stop func()) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			mu.Lock()
			n++
			mu.Unlock()
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	pool, m = poolFor(t, []string{"backend.test:" + port})
	pool.lookup = fr.lookup
	return pool, m, func() int { mu.Lock(); defer mu.Unlock(); return n }, srv.Close
}

// get sends one request through the pool's transport on a connection of its own.
func get(t *testing.T, p *Pool) error {
	t.Helper()
	p.tr.CloseIdleConnections() // the next request must dial
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+p.Endpoints[0].Addr+"/b/k", http.NoBody)
	resp, err := p.Transport.RoundTrip(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

func resolveCount(m *admin.Metrics) float64 {
	fams, _ := m.Registry.Gather()
	for _, f := range fams {
		if f.GetName() != "tollbooth_backend_errors_total" {
			continue
		}
		for _, mt := range f.GetMetric() {
			for _, l := range mt.GetLabel() {
				if l.GetName() == "kind" && l.GetValue() == "resolve" {
					return mt.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// The outage of 2026-11-10: the resolver stops answering while the backend is fine. New
// connections go to the address the name last resolved to, the probe passes there, the endpoint
// stays up, and the failure is counted and shown instead of hidden.
func TestResolverOutageKeepsServing(t *testing.T) {
	fr := &fakeResolver{ips: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	p, m, served, stop := namedBackend(t, fr)
	defer stop()
	p.Start()
	defer p.Stop()
	if err := get(t, p); err != nil {
		t.Fatalf("with the resolver answering: %v", err)
	}
	if st := p.Endpoints[0].status(); len(st.Addresses) != 1 || st.Addresses[0] != "127.0.0.1" || st.ResolverFailingSince != "" || st.ErrorsResolve != 0 {
		t.Fatalf("status before the outage: %+v", st)
	}

	for name, fail := range map[string]func(){
		"the resolver errors":               func() { fr.set(nil, errNoResolver, false) },
		"the resolver hangs":                func() { fr.set(nil, nil, true) },
		"the resolver answers with nothing": func() { fr.set(nil, nil, false) },
	} {
		fail()
		before := served()
		start := time.Now()
		if err := get(t, p); err != nil {
			t.Fatalf("%s: a request that needs a new connection failed: %v", name, err)
		}
		if took := time.Since(start); took > p.dial*3/4 {
			t.Errorf("%s: the request took %v; a lookup may use half of the %v dial timeout, leaving the rest to connect", name, took, p.dial)
		}
		if served() != before+1 {
			t.Fatalf("%s: the backend did not see the request", name)
		}
	}
	// Several probe intervals into the outage the endpoint is still up and the pool ready.
	time.Sleep(4 * p.check.Interval)
	if !p.Endpoints[0].Up() || !p.Healthy() {
		t.Fatalf("a failed lookup alone marked the endpoint down (last error %q)", first(p.Endpoints[0].LastError()))
	}
	st := p.Endpoints[0].status()
	if !st.Up || st.ErrorsResolve == 0 || st.ResolverFailingSince == "" || !strings.Contains(st.LastError, "resolver:") || !strings.Contains(st.LastError, "using the last known address 127.0.0.1") {
		t.Fatalf("status during the outage does not say so: %+v", st)
	}
	if st.Errors != st.ErrorsResolve {
		t.Errorf("errors %d, of which resolve %d: nothing else failed", st.Errors, st.ErrorsResolve)
	}
	if resolveCount(m) == 0 {
		t.Error(`tollbooth_backend_errors_total{kind="resolve"} did not move`)
	}

	// The resolver returns: the next dial ends the failure.
	fr.set([]netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil, false)
	if err := get(t, p); err != nil {
		t.Fatal(err)
	}
	if st := p.Endpoints[0].status(); st.ResolverFailingSince != "" || !st.Up {
		t.Fatalf("status after the resolver returned: %+v", st)
	}
}

func first(s string, _ time.Time) string { return s }

// A start during the outage: the name has never resolved, so there is nothing to fall back on.
// The dial is a connect error of kind resolve, the probe marks the endpoint down, and the pool is
// not ready, which is true: it cannot reach a backend it has never located.
func TestStartDuringResolverOutage(t *testing.T) {
	fr := &fakeResolver{err: errNoResolver}
	p, m, served, stop := namedBackend(t, fr)
	defer stop()
	err := get(t, p)
	if err == nil || !IsConnectError(err) || errorKind(err) != "resolve" {
		t.Fatalf("err %v (connect error %v, kind %s); want a connect error of kind resolve", err, err != nil && IsConnectError(err), errorKind(err))
	}
	if served() != 0 {
		t.Fatal("the backend was reached without an address")
	}
	// One lookup failed: it is counted once, in the endpoint's row and in the metric alike.
	p.MarkDown(p.Endpoints[0], err, errorKind(err)) // what the request path does with a connect error
	if st := p.Endpoints[0].status(); st.ErrorsResolve != 1 || resolveCount(m) != 1 {
		t.Fatalf("one failed lookup counted %d times in the row and %v in the metric", st.ErrorsResolve, resolveCount(m))
	}
	p.Start()
	defer p.Stop()
	deadline := time.Now().Add(3 * time.Second)
	for p.Endpoints[0].Up() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if p.Endpoints[0].Up() || p.Healthy() {
		t.Fatal("an endpoint that never resolved is reported up")
	}
	if st := p.Endpoints[0].status(); st.ErrorsResolve == 0 || len(st.Addresses) != 0 || st.ResolverFailingSince == "" {
		t.Fatalf("status: %+v", st)
	}
	if resolveCount(m) == 0 {
		t.Error("resolve failures not counted")
	}
	// The resolver comes up: the probe finds the backend and the endpoint with it.
	fr.set([]netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil, false)
	deadline = time.Now().Add(3 * time.Second)
	for !p.Endpoints[0].Up() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !p.Endpoints[0].Up() {
		t.Fatal("the endpoint never came up after the resolver did")
	}
}

// The remembered address is only a way to reach the backend, not a verdict on it: when the
// backend itself goes away during a resolver outage, the probe says down.
func TestRememberedAddressThatNoLongerAnswers(t *testing.T) {
	fr := &fakeResolver{ips: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	p, _, _, stop := namedBackend(t, fr)
	if err := get(t, p); err != nil {
		t.Fatal(err)
	}
	stop() // the backend is gone
	fr.set(nil, errNoResolver, false)
	p.Start()
	defer p.Stop()
	deadline := time.Now().Add(3 * time.Second)
	for p.Endpoints[0].Up() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if p.Endpoints[0].Up() {
		t.Fatal("the endpoint stayed up with nothing listening at its last known address")
	}
	if err := get(t, p); err == nil || !IsConnectError(err) || errorKind(err) != "dial" {
		t.Fatalf("err %v; want a connect error of kind dial (the address is known, it does not answer)", err)
	}
}

// DNS is still asked on every new connection: a new answer replaces the remembered one, and an
// address that refuses is passed over for the next in the answer.
func TestNewAnswerReplacesTheRememberedOne(t *testing.T) {
	fr := &fakeResolver{ips: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	p, _, servedA, stop := namedBackend(t, fr)
	defer stop()
	// A second backend on 127.0.0.2, same port.
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.2", p.Endpoints[0].port))
	if err != nil {
		t.Skipf("cannot listen on 127.0.0.2: %v", err)
	}
	var mu sync.Mutex
	nB := 0
	srvB := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			mu.Lock()
			nB++
			mu.Unlock()
		}
		w.WriteHeader(http.StatusForbidden)
	})}
	go srvB.Serve(ln) //nolint:errcheck // test server
	defer srvB.Close()
	servedB := func() int { mu.Lock(); defer mu.Unlock(); return nB }

	if err := get(t, p); err != nil || servedA() != 1 || servedB() != 0 {
		t.Fatalf("first answer: err %v, A %d, B %d", err, servedA(), servedB())
	}
	// The cluster moved: the resolver now answers 127.0.0.2 only.
	fr.set([]netip.Addr{netip.MustParseAddr("127.0.0.2")}, nil, false)
	if err := get(t, p); err != nil || servedA() != 1 || servedB() != 1 {
		t.Fatalf("second answer: err %v, A %d, B %d", err, servedA(), servedB())
	}
	if st := p.Endpoints[0].status(); len(st.Addresses) != 1 || st.Addresses[0] != "127.0.0.2" {
		t.Fatalf("remembered %v, want the new answer", st.Addresses)
	}
	// The resolver fails: the remembered address is the new one, not the first.
	fr.set(nil, errNoResolver, false)
	if err := get(t, p); err != nil || servedA() != 1 || servedB() != 2 {
		t.Fatalf("during the outage: err %v, A %d, B %d", err, servedA(), servedB())
	}
	// An answer whose first address refuses: the next one is tried.
	fr.set([]netip.Addr{netip.MustParseAddr("127.0.0.3"), netip.MustParseAddr("127.0.0.1")}, nil, false)
	if err := get(t, p); err != nil || servedA() != 2 {
		t.Fatalf("first address refusing: err %v, A %d", err, servedA())
	}
}

// An endpoint written as an address never touches the resolver.
func TestAddressEndpointIsNeverLookedUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	p, _ := poolFor(t, []string{strings.TrimPrefix(srv.URL, "http://")})
	fr := &fakeResolver{err: errNoResolver}
	p.lookup = fr.lookup
	p.Start()
	defer p.Stop()
	if err := get(t, p); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * p.check.Interval)
	if fr.calls != 0 || !p.Endpoints[0].Up() {
		t.Fatalf("lookups %d, up %v", fr.calls, p.Endpoints[0].Up())
	}
	if st := p.Endpoints[0].status(); len(st.Addresses) != 0 || st.ErrorsResolve != 0 {
		t.Fatalf("status: %+v", st)
	}
}

// A caller that gives up during the lookup has learned nothing about the resolver.
func TestAbandonedLookupIsNotAResolverFailure(t *testing.T) {
	fr := &fakeResolver{hang: true}
	p, _, _, stop := namedBackend(t, fr)
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, err := p.dialer(time.Second)(ctx, "tcp", p.Endpoints[0].Addr); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	if st := p.Endpoints[0].status(); st.ErrorsResolve != 0 || st.ResolverFailingSince != "" {
		t.Fatalf("an abandoned lookup was recorded as a resolver failure: %+v", st)
	}
}

func BenchmarkDialNamedEndpoint(b *testing.B) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck // bench
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	c, _ := config.Parse([]byte("node_id: b\nbackend: {scheme: http, endpoints: [\"backend.test:" + port + "\"]}\nshowback: {dir: /tmp/x}\n"))
	p, _ := NewPool(&c.Backend, nil, nil)
	fr := &fakeResolver{ips: []netip.Addr{netip.MustParseAddr("127.0.0.1")}}
	p.lookup = fr.lookup
	dial := p.dialer(time.Second)
	b.ReportAllocs()
	for b.Loop() {
		conn, derr := dial(context.Background(), "tcp", p.Endpoints[0].Addr)
		if derr != nil {
			b.Fatal(derr)
		}
		_ = conn.Close()
	}
}
