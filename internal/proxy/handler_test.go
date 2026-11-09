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
// Adapted from github.com/blakegolliher/shunt internal/proxy/handler_test.go at commit
// 9b36d09ed109add4a2bedfe1dbfae6108038ee5b (Apache-2.0, Copyright 2026 Blake Golliher).
// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, 2026-11-09: the rig runs
// the null backend behind a Pool; the invariants of docs/DESIGN.md §4.9 (raw path, Host, no
// added headers, hop-by-hop, no invented Content-Type, Expect: 100-continue, short body never
// complete, trickle flush, retry before bytes, all backends down, exact allocation bound) each
// have a test; shunt's resign, metadata-deadline, and slow-ring cases are dropped.

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blakegolliher/tollbooth/bench/nullbackend"
	"github.com/blakegolliher/tollbooth/internal/admin"
	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/s3id"
)

// syncBuf is a bytes.Buffer safe for the handler goroutine to write while a test reads it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// rig wires a handler in front of backend servers and serves it on a plain httptest server.
type rig struct {
	backends []*httptest.Server
	null     *nullbackend.Server
	front    *httptest.Server
	h        *Handler
	pool     *Pool
	cfg      *atomic.Pointer[config.Config]
	log      *syncBuf
}

func testConfig(t *testing.T, endpoints []string, idle time.Duration) *config.Config {
	t.Helper()
	y := "node_id: test\nbase_domains: [tb.example.com]\nbackend:\n  scheme: http\n  idle_body_timeout: " + idle.String() + "\n  endpoints:\n"
	for _, e := range endpoints {
		y += "    - " + e + "\n"
	}
	y += "showback: {dir: /tmp/tb-test}\nlisten: {trusted_proxies: [127.0.0.0/8]}\n"
	c, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// newRig starts n null backends (the first is returned as r.null) with the given idle timeout.
func newRig(t *testing.T, n int, idle time.Duration, opts ...func(*nullbackend.Server)) *rig {
	t.Helper()
	r := &rig{log: &syncBuf{}}
	var eps []string
	for i := 0; i < n; i++ {
		nb := nullbackend.New(4096)
		nb.Record = true
		for _, o := range opts {
			o(nb)
		}
		be := httptest.NewServer(nb)
		t.Cleanup(be.Close)
		r.backends = append(r.backends, be)
		eps = append(eps, strings.TrimPrefix(be.URL, "http://"))
		if i == 0 {
			r.null = nb
		}
	}
	return r.start(t, testConfig(t, eps, idle))
}

func (r *rig) start(t *testing.T, c *config.Config) *rig {
	t.Helper()
	r.cfg = &atomic.Pointer[config.Config]{}
	r.cfg.Store(c)
	m := admin.NewMetrics()
	log := slog.New(slog.NewJSONHandler(r.log, nil))
	pool, err := NewPool(&c.Backend, m, log)
	if err != nil {
		t.Fatal(err)
	}
	r.pool = pool
	r.h = New(r.cfg, pool, m, NewAccessLogger(r.log), log)
	r.front = httptest.NewUnstartedServer(r.h)
	r.front.Config = NewServer(r.h, &c.Listen, m, log) // the real server settings: no mux, timeouts, connection gauge
	r.front.Start()
	t.Cleanup(r.front.Close)
	return r
}

// waitLog waits for the access log to contain substr: the handler finishes its record after the
// client has already seen the response (or the abort).
func (r *rig) waitLog(t *testing.T, substr string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if s := r.log.String(); strings.Contains(s, substr) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("access log never contained %q:\n%s", substr, r.log.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fresh returns a client with its own connection pool so tests never share a keep-alive
// connection that a previous test's proxy aborted.
func fresh() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true, DisableCompression: true}}
}

// rawDial opens a raw TCP connection to srv for hand-written HTTP.
func rawDial(t *testing.T, srv *httptest.Server) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestRoundTripPreservesEverything(t *testing.T) {
	r := newRig(t, 1, time.Second)
	body := strings.NewReader("hello body")
	req, _ := http.NewRequest(http.MethodPut, r.front.URL+"/b/dir/k%20ey%2Fx+y?partNumber=1&uploadId=u", body)
	req.Host = "tb.example.com:8443"
	req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAX/20261109/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=x")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("User-Agent", "") // a client that sends no UA; the proxy must not invent one
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()        //nolint:errcheck // test
	io.Copy(io.Discard, resp.Body) //nolint:errcheck // test
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	reqs := r.null.Requests()
	if len(reqs) != 1 {
		t.Fatalf("backend saw %d requests", len(reqs))
	}
	seen := reqs[0]
	if seen.Host != "tb.example.com:8443" {
		t.Errorf("host not preserved: %q", seen.Host)
	}
	if seen.RequestURI != "/b/dir/k%20ey%2Fx+y?partNumber=1&uploadId=u" {
		t.Errorf("raw path/query not preserved: %q", seen.RequestURI)
	}
	if string(seen.Body) != "hello body" {
		t.Errorf("body: %q", seen.Body)
	}
	if seen.Header.Get("Authorization") == "" || seen.Header.Get("X-Amz-Content-Sha256") != "UNSIGNED-PAYLOAD" {
		t.Errorf("signed headers not forwarded: %v", seen.Header)
	}
	for _, k := range []string{"Keep-Alive", "Accept-Encoding", "Via", "X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Real-Ip", "X-Request-Id", "Forwarded"} {
		if _, ok := seen.Header[k]; ok {
			t.Errorf("proxy added or leaked %s: %v", k, seen.Header[k])
		}
	}
	if ua, ok := seen.Header["User-Agent"]; ok && ua[0] != "" {
		t.Errorf("proxy invented User-Agent %q", ua)
	}
	if seen.Op != s3id.OpUploadPart {
		t.Errorf("backend classified %s", seen.Op)
	}
	// Response: the backend's headers, nothing added.
	if resp.Header.Get("X-Amz-Request-Id") != "NULLBACKEND000001" || resp.Header.Get("ETag") != `"null"` {
		t.Errorf("response headers not passed back: %v", resp.Header)
	}
	for _, k := range []string{"Via", "Server", "X-Request-Id", "X-Proxy", "Retry-After"} {
		if _, ok := resp.Header[k]; ok {
			t.Errorf("proxy added response header %s", k)
		}
	}
	if l := r.waitLog(t, `"operation":"UploadPart"`); strings.Contains(l, "k ey") || strings.Contains(l, "AKIAX") {
		t.Errorf("access log leaks the key or access key: %s", l)
	}
}

// The handler is not a mux: "//" and ".." reach the backend verbatim, in both addressing styles,
// and a leading "//" (an empty bucket segment, or a vhost key "/k") is forwarded, never 400'd
// (D-004 as revised at Gate 0).
func TestRawPathSurvivesHandler(t *testing.T) {
	r := newRig(t, 1, time.Second)
	cases := []struct{ host, target string }{
		{"h", "/b//k"},
		{"h", "/b/a/../k"},
		{"h", "/b/./k/"},
		{"h", "/b/%2F%25%7E%C3%A9"},
		{"h", "//k"},
		{"b.tb.example.com", "//k"},
		{"h", "/b/k?x-id=GetObject&versionId=%2F"},
		{"h", "/%2F%22"},
	}
	for _, c := range cases {
		r.null.Clear()
		conn := rawDial(t, r.front)
		fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", c.target, c.host)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("%s: %v", c.target, err)
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // test
		resp.Body.Close()              //nolint:errcheck // test
		reqs := r.null.Requests()
		if len(reqs) != 1 {
			t.Fatalf("%s: backend saw %d requests (status %d)", c.target, len(reqs), resp.StatusCode)
		}
		if reqs[0].RequestURI != c.target {
			t.Errorf("target %q reached the backend as %q", c.target, reqs[0].RequestURI)
		}
	}
	// Absolute-form is reduced to origin-form (D-028); "*" too.
	r.null.Clear()
	conn := rawDial(t, r.front)
	fmt.Fprintf(conn, "GET http://tb.example.com/b/k HTTP/1.1\r\nHost: tb.example.com\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() //nolint:errcheck // test
	if reqs := r.null.Requests(); len(reqs) != 1 || reqs[0].RequestURI != "/b/k" {
		t.Errorf("absolute-form: %+v", reqs)
	}
}

func TestLargeBodyBothWays(t *testing.T) {
	const n = 3<<20 + 12345
	payload := bytes.Repeat([]byte("0123456789abcdef"), n/16+1)[:n]
	sum := sha256.Sum256(payload)
	r := newRig(t, 1, time.Second)
	resp, err := fresh().Post(r.front.URL+"/b/k", "application/octet-stream", bytes.NewReader(payload)) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() //nolint:errcheck // test
	if reqs := r.null.Requests(); len(reqs) != 1 || sha256.Sum256(reqs[0].Body) != sum {
		t.Fatalf("upload corrupted or lost")
	}
	// A large download: the null backend's GetObject body is fixed, so use a bigger one.
	big := nullbackend.New(n)
	be := httptest.NewServer(big)
	defer be.Close()
	r2 := (&rig{log: &syncBuf{}}).start(t, testConfig(t, []string{strings.TrimPrefix(be.URL, "http://")}, time.Second))
	resp, err = fresh().Get(r2.front.URL + "/b/k") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || len(got) != n || sha256.Sum256(got) != sum {
		t.Fatalf("download: status %d len %d", resp.StatusCode, len(got))
	}
	r2.waitLog(t, `"bytes_out":`+fmt.Sprint(n))
}

// A backend response without Content-Type must reach the client without one: net/http would
// otherwise sniff "text/xml; charset=utf-8" onto XML bodies.
func TestNoContentTypeIsNotInvented(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Content-Type"] = nil // the backend sends none
		_, _ = w.Write([]byte(`<?xml version="1.0"?><InitiateMultipartUploadResult/>`))
	}))
	defer be.Close()
	r := (&rig{log: &syncBuf{}}).start(t, testConfig(t, []string{strings.TrimPrefix(be.URL, "http://")}, time.Second))
	req, _ := http.NewRequest(http.MethodPost, r.front.URL+"/b/k?uploads", nil)
	resp, err := fresh().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	if v, ok := resp.Header["Content-Type"]; ok {
		t.Fatalf("proxy invented Content-Type %v", v)
	}
}

func TestUnknownOpIsProxied(t *testing.T) {
	r := newRig(t, 1, time.Second)
	req, _ := http.NewRequest("PATCH", r.front.URL+"/b?frobnicate", nil)
	resp, err := fresh().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() //nolint:errcheck // test
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	r.waitLog(t, `"operation":"Unknown"`)
}

type countingBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (c countingBody) Read(p []byte) (int, error) { c.n.Add(1); return c.ReadCloser.Read(p) }

// A request answered by the proxy itself (here: the in-flight cap) with Expect: 100-continue
// is refused before the client sends a body byte, well inside the 1 s expect timeout, and net/http
// never sends 100 Continue.
func TestExpectContinueRefusedBeforeBody(t *testing.T) {
	r := newRig(t, 1, time.Second)
	c := *r.cfg.Load()
	c.Listen.MaxInflight = 1
	r.cfg.Store(&c)
	var bodyReads atomic.Int64
	inner := r.h
	r.front.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.Body = countingBody{req.Body, &bodyReads}
		inner.ServeHTTP(w, req)
	})
	// Occupy the one slot with a stalled request.
	r.null.Stall.Store(true)
	hold := rawDial(t, r.front)
	fmt.Fprintf(hold, "GET /b/k HTTP/1.1\r\nHost: h\r\n\r\n")
	time.Sleep(50 * time.Millisecond)

	conn := rawDial(t, r.front)
	start := time.Now()
	fmt.Fprintf(conn, "PUT /b/huge HTTP/1.1\r\nHost: tb.example.com\r\nContent-Length: 5368709120\r\nExpect: 100-continue\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var e ErrorResponse
	if xml.Unmarshal(body, &e) != nil || e.Code != CodeSlowDown || e.Resource != "/b/huge" || e.RequestID == "" {
		t.Fatalf("body %q", body)
	}
	if resp.Header.Get("Retry-After") != "1" || resp.Header.Get("x-amz-request-id") != e.RequestID {
		t.Fatalf("headers %v", resp.Header)
	}
	if bodyReads.Load() != 0 {
		t.Fatalf("client body was read %d times before the 503", bodyReads.Load())
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("503 took %v", elapsed)
	}
	r.waitLog(t, `"status_class":"proxy_error"`)
}

// The happy path: backend sends 100, client sends the body, gets 200.
func TestExpectContinueAccepted(t *testing.T) {
	r := newRig(t, 1, time.Second)
	c := rawDial(t, r.front)
	fmt.Fprintf(c, "PUT /b/k HTTP/1.1\r\nHost: h\r\nContent-Length: 5\r\nExpect: 100-continue\r\n\r\n")
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "HTTP/1.1 100") {
		t.Fatalf("expected 100 Continue, got %q %v", line, err)
	}
	for { // skip the empty line(s) after the 100
		l, _ := br.ReadString('\n')
		if l == "\r\n" {
			break
		}
	}
	fmt.Fprint(c, "hello")
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if reqs := r.null.Requests(); len(reqs) != 1 || string(reqs[0].Body) != "hello" {
		t.Fatalf("backend saw %+v", reqs)
	}
}

// hijackAfter writes k bytes with the given framing, then kills the connection.
func hijackAfter(k int, contentLength bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentLength {
			w.Header().Set("Content-Length", "1000000")
		}
		w.WriteHeader(200)
		_, _ = w.Write(bytes.Repeat([]byte("x"), k))
		http.NewResponseController(w).Flush() //nolint:errcheck // test
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			conn.Close()
		}
	})
}

func rigFor(t *testing.T, h http.Handler, idle time.Duration) *rig {
	t.Helper()
	be := httptest.NewServer(h)
	t.Cleanup(be.Close)
	return (&rig{log: &syncBuf{}}).start(t, testConfig(t, []string{strings.TrimPrefix(be.URL, "http://")}, idle))
}

// The gate: a backend killed mid-GET never yields a silent short read.
func TestBackendDiesMidGetContentLength(t *testing.T) {
	r := rigFor(t, hijackAfter(70000, true), time.Second)
	resp, err := fresh().Get(r.front.URL + "/b/k") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	got, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("silent short read: got %d bytes and clean EOF", len(got))
	}
	r.waitLog(t, `"error":"body:`)
}

func TestBackendDiesMidGetChunked(t *testing.T) {
	r := rigFor(t, hijackAfter(70000, false), time.Second)
	c := rawDial(t, r.front)
	fmt.Fprintf(c, "GET /b/k HTTP/1.1\r\nHost: h\r\n\r\n")
	raw, _ := io.ReadAll(c) // until the proxy closes the connection
	if !bytes.Contains(raw, []byte("Transfer-Encoding: chunked")) {
		t.Fatalf("expected a chunked response, got:\n%s", raw[:min(400, len(raw))])
	}
	if bytes.HasSuffix(raw, []byte("0\r\n\r\n")) {
		t.Fatal("silent short read: chunked body was terminated cleanly")
	}
}

// D-006: a trickle response reaches the client as it arrives. The backend sends headers, then
// one byte per second; the client sees the headers and the first byte within 1.5 s.
func TestTrickleResponseReachesClient(t *testing.T) {
	r := rigFor(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(200)
		http.NewResponseController(w).Flush() //nolint:errcheck // test
		for i := 0; i < 3; i++ {
			select {
			case <-req.Context().Done():
				return
			case <-time.After(time.Second):
			}
			_, _ = w.Write([]byte(" "))
			http.NewResponseController(w).Flush() //nolint:errcheck // test
		}
	}), 10*time.Second)
	start := time.Now()
	resp, err := fresh().Get(r.front.URL + "/b/k?uploadId=1") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	var one [1]byte
	n, err := resp.Body.Read(one[:])
	if err != nil || n != 1 {
		t.Fatalf("first byte: n=%d err=%v", n, err)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("headers and first byte took %v; the response was buffered", d)
	}
}

func TestIdleTimeoutStalledBackend(t *testing.T) {
	r := rigFor(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(200)
		_, _ = w.Write(bytes.Repeat([]byte("p"), 70000))
		http.NewResponseController(w).Flush() //nolint:errcheck // test
		<-req.Context().Done()                // stall until the proxy gives up
	}), 300*time.Millisecond)
	start := time.Now()
	resp, err := fresh().Get(r.front.URL + "/b/k") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	if _, err = io.ReadAll(resp.Body); err == nil {
		t.Fatal("expected an error from the stalled backend")
	}
	if d := time.Since(start); d < 250*time.Millisecond || d > 2*time.Second {
		t.Fatalf("idle timeout fired after %v", d)
	}
}

func TestIdleTimeoutNotFiredWhileProgressing(t *testing.T) {
	r := rigFor(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "6")
		w.WriteHeader(200)
		for i := 0; i < 6; i++ { // 6 bytes over ~1.2 s with a 300 ms idle limit
			_, _ = w.Write([]byte("x"))
			http.NewResponseController(w).Flush() //nolint:errcheck // test
			time.Sleep(200 * time.Millisecond)
		}
	}), 300*time.Millisecond)
	resp, err := fresh().Get(r.front.URL + "/b/k") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	got, err := io.ReadAll(resp.Body)
	if err != nil || string(got) != "xxxxxx" {
		t.Fatalf("slow-but-progressing transfer failed: %q %v", got, err)
	}
}

// The idle watchdog is not armed while waiting for response headers: a backend that takes
// longer than idle_body_timeout to answer a small request still gets through.
func TestSlowHeadersAreNotIdle(t *testing.T) {
	r := rigFor(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(700 * time.Millisecond)
		w.WriteHeader(200)
	}), 300*time.Millisecond)
	req, _ := http.NewRequest(http.MethodPost, r.front.URL+"/b/k?uploadId=1", strings.NewReader("<CompleteMultipartUpload/>"))
	resp, err := fresh().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() //nolint:errcheck // test
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestAllBackendsDown(t *testing.T) {
	r := newRig(t, 2, time.Second)
	for _, be := range r.backends {
		be.Close()
	}
	resp, err := fresh().Get(r.front.URL + "/b/k") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	body, _ := io.ReadAll(resp.Body)
	var e ErrorResponse
	if resp.StatusCode != http.StatusServiceUnavailable || xml.Unmarshal(body, &e) != nil || e.Code != CodeServiceUnavailable || e.RequestID == "" {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if resp.Header.Get("x-amz-request-id") != e.RequestID {
		t.Errorf("request id header missing: %v", resp.Header)
	}
	if r.pool.Healthy() {
		t.Error("pool still healthy with every endpoint refusing")
	}
	r.waitLog(t, `"result":"error"`)
}

// One backend down: the dial error moves the request to the other endpoint before any byte was
// sent, the request succeeds, and the dead endpoint is out of rotation afterwards.
func TestOneBackendDownRetriesBeforeBytes(t *testing.T) {
	r := newRig(t, 2, time.Second)
	r.backends[1].Close()
	// Force the rotation to start at the dead endpoint.
	for r.pool.Next() != r.pool.Endpoints[0] {
	}
	for i := 0; i < 4; i++ {
		resp, err := fresh().Post(r.front.URL+"/b/k", "application/octet-stream", strings.NewReader("payload")) //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close() //nolint:errcheck // test
		if resp.StatusCode != 200 {
			t.Fatalf("attempt %d: status %d", i, resp.StatusCode)
		}
	}
	if r.pool.Endpoints[1].Up() {
		t.Error("dead endpoint still in rotation")
	}
	if n := r.null.Served(); n != 4 {
		t.Errorf("live backend served %d, want 4", n)
	}
}

// A panic inside the handler fails open: 503 when nothing was written, an abort otherwise,
// never a hang and never a truncated body delivered as complete.
func TestPanicFailsOpen(t *testing.T) {
	r := newRig(t, 1, time.Second)
	r.pool.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { panic("boom") })
	resp, err := fresh().Get(r.front.URL + "/b/k") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	if resp.StatusCode != http.StatusServiceUnavailable || r.h.Errors.Load() != 1 {
		t.Fatalf("status %d errors %d", resp.StatusCode, r.h.Errors.Load())
	}
	r.waitLog(t, `"error":"panic: boom"`)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMetricsObserved(t *testing.T) {
	r := newRig(t, 1, time.Second)
	resp, _ := fresh().Get(r.front.URL + "/b/k") //nolint:noctx // test
	io.Copy(io.Discard, resp.Body)               //nolint:errcheck // test
	resp.Body.Close()                            //nolint:errcheck // test
	r.waitLog(t, `"operation":"GetObject"`)
	fams, _ := r.h.Metrics.Registry.Gather()
	got := map[string]float64{}
	for _, f := range fams {
		for _, m := range f.GetMetric() {
			labels := ""
			for _, lp := range m.GetLabel() {
				labels += lp.GetName() + "=" + lp.GetValue() + ","
			}
			key := f.GetName() + "{" + labels + "}"
			switch {
			case m.GetCounter() != nil:
				got[key] = m.GetCounter().GetValue()
			case m.GetHistogram() != nil:
				got[key] = float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	for key, want := range map[string]float64{
		"tollbooth_requests_total{op_class=READ,operation=GetObject,result=forwarded,status_class=2xx,}": 1, // Gather sorts labels by name
		"tollbooth_request_duration_seconds{op_class=READ,}":                                             1,
		"tollbooth_proxy_overhead_seconds{op_class=READ,}":                                               1,
		"tollbooth_backend_ttfb_seconds{op_class=READ,}":                                                 1,
		"tollbooth_bytes_total{direction=out,}":                                                          4096,
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
}

// nopWriter is a ResponseWriter that discards everything and allocates nothing after
// construction, so the allocation test counts only the handler's own work.
type nopWriter struct {
	h http.Header
}

func (n *nopWriter) Header() http.Header              { return n.h }
func (n *nopWriter) Write(p []byte) (int, error)      { return len(p), nil }
func (n *nopWriter) WriteHeader(int)                  {}
func (n *nopWriter) Flush()                           {}
func (n *nopWriter) SetReadDeadline(time.Time) error  { return nil } // net/http's writer supports these; without them
func (n *nopWriter) SetWriteDeadline(time.Time) error { return nil } // ResponseController allocates an ErrNotSupported

// cannedBody is a reusable response body: Reset per request, no allocation.
type cannedBody struct{ bytes.Reader }

func (c *cannedBody) Close() error { return nil }

// fakeTransport returns one pre-built response per call. Reusing the response struct is safe
// because the handler reads it and closes the body on the same goroutine before the next call.
type fakeTransport struct {
	resp *http.Response
	body *cannedBody
	data []byte
}

func (f *fakeTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.body.Reset(f.data)
	f.resp.Body = f.body
	return f.resp, nil
}

// TestHandlerAllocations enforces the budget of docs/DESIGN.md §9 exactly: at most seven
// allocations of the handler's own per forwarded 4 KiB GetObject, counted against a no-op
// ResponseWriter and a fake RoundTripper so net/http's server and transport contribute nothing.
func TestHandlerAllocations(t *testing.T) {
	c := testConfig(t, []string{"127.0.0.1:1"}, time.Second)
	cfg := &atomic.Pointer[config.Config]{}
	cfg.Store(c)
	m := admin.NewMetrics()
	pool, err := NewPool(&c.Backend, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("x"), 4096)
	ft := &fakeTransport{data: data, body: &cannedBody{}}
	ft.resp = &http.Response{StatusCode: 200, ContentLength: int64(len(data)), Header: http.Header{"Etag": {`"x"`}, "Content-Type": {"application/octet-stream"}, "Content-Length": {"4096"}}}
	pool.Transport = ft
	h := New(cfg, pool, m, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/b/k", nil)
	req.RequestURI = "/b/k"
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAX/20261109/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=x")
	w := &nopWriter{h: http.Header{}}
	h.ServeHTTP(w, req) // warm the pools
	allocs := testing.AllocsPerRun(2000, func() {
		clear(w.h)
		h.ServeHTTP(w, req)
	})
	t.Logf("handler allocations per forwarded GetObject: %.2f", allocs)
	if allocs > 7 {
		t.Fatalf("handler allocates %.2f per request; the budget is 7 (docs/DESIGN.md §9)", allocs)
	}
}

func TestAccessLoggerNilIsOff(t *testing.T) {
	if NewAccessLogger(nil) != nil {
		t.Fatal("a nil writer must disable the logger")
	}
}

func TestWriteS3Error(t *testing.T) {
	rec := httptest.NewRecorder()
	rid := writeS3Error(rec, CodeTooManyRequests, `/b/k<&>"'`, 3)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "3" || rec.Header().Get("x-amz-request-id") != rid || len(rid) != 32 {
		t.Fatalf("status %d headers %v", rec.Code, rec.Header())
	}
	var e ErrorResponse
	if err := xml.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("%v:\n%s", err, rec.Body.String())
	}
	if e.Code != CodeTooManyRequests || e.Resource != `/b/k<&>"'` || e.RequestID != rid || e.Message != "Please reduce your request rate." {
		t.Fatalf("%+v", e)
	}
	if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprint(rec.Body.Len()) {
		t.Fatalf("content-length %s vs body %d", cl, rec.Body.Len())
	}
	rec = httptest.NewRecorder()
	writeS3Error(rec, CodeServiceUnavailable, "/", 0)
	if rec.Header().Get("Retry-After") != "" || rec.Code != 503 {
		t.Fatalf("no Retry-After expected: %v", rec.Header())
	}
}

func BenchmarkSmallGET(b *testing.B) {
	c, _ := config.Parse([]byte("node_id: b\nbackend: {scheme: http, endpoints: [127.0.0.1:1]}\nshowback: {dir: /tmp/x}\n"))
	cfg := &atomic.Pointer[config.Config]{}
	cfg.Store(c)
	m := admin.NewMetrics()
	pool, _ := NewPool(&c.Backend, m, nil)
	data := make([]byte, 4096)
	ft := &fakeTransport{data: data, body: &cannedBody{}}
	ft.resp = &http.Response{StatusCode: 200, ContentLength: 4096, Header: http.Header{"Content-Length": {"4096"}}}
	pool.Transport = ft
	h := New(cfg, pool, m, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/b/k", nil)
	req.RequestURI = "/b/k"
	w := &nopWriter{h: http.Header{}}
	b.ReportAllocs()
	for b.Loop() {
		clear(w.h)
		h.ServeHTTP(w, req)
	}
}

func BenchmarkSmallGETEndToEnd(b *testing.B) {
	nb := nullbackend.New(4096)
	be := httptest.NewServer(nb)
	defer be.Close()
	c, _ := config.Parse([]byte("node_id: b\nbackend: {scheme: http, endpoints: [" + strings.TrimPrefix(be.URL, "http://") + "]}\nshowback: {dir: /tmp/x}\n"))
	cfg := &atomic.Pointer[config.Config]{}
	cfg.Store(c)
	m := admin.NewMetrics()
	pool, _ := NewPool(&c.Backend, m, nil)
	h := New(cfg, pool, m, nil, nil)
	front := httptest.NewServer(h)
	defer front.Close()
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 16}}
	b.ReportAllocs()
	for b.Loop() {
		resp, err := client.Get(front.URL + "/b/k") //nolint:noctx // bench
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // bench
		resp.Body.Close()              //nolint:errcheck // bench
	}
}

var _ = context.Background

// D-034: a backend that answers and closes before reading the request body. Go's transport
// races the write error against the response; the proxy relays the response when it wins and
// answers 503 when it does not. Either way the client gets a complete answer, promptly, and the
// backend saw no body it did not ask for. With Expect: 100-continue the answer is always relayed.
func TestBackendAnswersBeforeReadingBody(t *testing.T) {
	r := rigFor(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code></Error>`))
	}), time.Second)
	body := bytes.Repeat([]byte("p"), 8<<20)
	outcomes := map[string]int{}
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest(http.MethodPut, r.front.URL+"/b/k", bytes.NewReader(body))
		start := time.Now()
		resp, err := fresh().Do(req)
		switch {
		case err != nil:
			// The same race one hop earlier: the proxy answered and closed while the client was
			// still writing, and the client's transport reported the write error instead of the
			// response, exactly as it would against a backend that closes early.
			outcomes["transport error"]++
		case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusServiceUnavailable:
			outcomes[resp.Status]++
			io.Copy(io.Discard, resp.Body) //nolint:errcheck // test
			resp.Body.Close()              //nolint:errcheck // test
		default:
			t.Fatalf("attempt %d: status %d", i, resp.StatusCode)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("attempt %d took %v", i, d)
		}
	}
	t.Logf("without Expect: %v", outcomes)
	// With Expect: 100-continue the 403 arrives before any body byte and is relayed every time.
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest(http.MethodPut, r.front.URL+"/b/k", bytes.NewReader(body))
		req.Header.Set("Expect", "100-continue")
		resp, err := (&http.Client{Transport: &http.Transport{ExpectContinueTimeout: time.Second, DisableKeepAlives: true}}).Do(req)
		if err != nil {
			t.Fatalf("expect attempt %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // test
		resp.Body.Close()              //nolint:errcheck // test
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expect attempt %d: status %d, want 403 relayed", i, resp.StatusCode)
		}
	}
}
