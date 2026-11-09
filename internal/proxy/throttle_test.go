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
	"bufio"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blakegolliher/tollbooth/bench/nullbackend"
	"github.com/blakegolliher/tollbooth/internal/admin"
	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/limiter"
)

// rigWithRules starts one null backend behind a proxy whose config carries the given YAML tail
// (rate_limits, kill_switches, logging).
func rigWithRules(t *testing.T, tail string) *rig {
	t.Helper()
	nb := nullbackend.New(4096)
	nb.Record = true
	be := httptest.NewServer(nb)
	t.Cleanup(be.Close)
	y := "node_id: test\nbase_domains: [tb.example.com]\nbackend: {scheme: http, endpoints: [" + strings.TrimPrefix(be.URL, "http://") + "]}\nshowback: {dir: /tmp/tb-test}\n" + tail
	c, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{log: &syncBuf{}, null: nb, backends: []*httptest.Server{be}}
	return r.start(t, c)
}

const frozenRule = `
rate_limits:
  rules:
    - id: rule-secret-name
      match: {buckets: [frozen]}
      limit: {rate: 0}
`

// metricValue reads one gathered series by family and label string (Gather sorts labels by name).
func metricValue(t *testing.T, m *admin.Metrics, family, labels string) float64 {
	t.Helper()
	fams, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != family {
			continue
		}
		for _, mm := range f.GetMetric() {
			l := ""
			for _, lp := range mm.GetLabel() {
				l += lp.GetName() + "=" + lp.GetValue() + ","
			}
			if l != labels {
				continue
			}
			switch {
			case mm.GetCounter() != nil:
				return mm.GetCounter().GetValue()
			case mm.GetGauge() != nil:
				return mm.GetGauge().GetValue()
			case mm.GetHistogram() != nil:
				return float64(mm.GetHistogram().GetSampleCount())
			}
		}
	}
	return -1
}

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// A denied request is answered 503 SlowDown with the S3 body, Retry-After, and a request id; the
// backend never sees it; the rule id appears in the operator's log and metrics only, never in the
// response (CLAUDE.md).
func TestThrottleSlowDown(t *testing.T) {
	r := rigWithRules(t, frozenRule)
	resp, err := fresh().Get(r.front.URL + "/frozen/k?versionId=3") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close() //nolint:errcheck // test
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Content-Type") != "application/xml" || resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("status %d headers %v", resp.StatusCode, resp.Header)
	}
	var e ErrorResponse
	if err := xml.Unmarshal(body, &e); err != nil || e.Code != CodeSlowDown || e.Resource != "/frozen/k" || e.Message != "Please reduce your request rate." {
		t.Fatalf("body %s (%v)", body, err)
	}
	if rid := resp.Header.Get("x-amz-request-id"); !hex32.MatchString(rid) || rid != e.RequestID {
		t.Fatalf("request id header %q body %q", rid, e.RequestID)
	}
	if strings.Contains(string(body), "rule-secret") {
		t.Fatalf("rule id leaked to the client: %s", body)
	}
	for k := range resp.Header {
		if strings.Contains(strings.ToLower(k), "rule") {
			t.Fatalf("rule id leaked in a header: %v", resp.Header)
		}
	}
	if n := r.null.Served(); n != 0 {
		t.Fatalf("backend served %d requests for a throttled client", n)
	}
	logLine := r.waitLog(t, `"result":"throttled"`)
	if !strings.Contains(logLine, `"status_class":"throttled"`) || !strings.Contains(logLine, `"rule":"rule-secret-name"`) || !strings.Contains(logLine, `"status":503`) {
		t.Fatalf("access log: %s", logLine)
	}
	m := r.h.Metrics
	if v := metricValue(t, m, "tollbooth_requests_total", "op_class=READ,operation=GetObject,result=throttled,status_class=throttled,"); v != 1 {
		t.Fatalf("requests_total throttled = %v", v)
	}
	if v := metricValue(t, m, "tollbooth_limiter_denied_total", "rule=rule-secret-name,"); v != 1 {
		t.Fatalf("limiter_denied_total = %v", v)
	}
	if v := metricValue(t, m, "tollbooth_limiter_decision_seconds", ""); v != 1 {
		t.Fatalf("limiter_decision_seconds samples = %v", v)
	}
	// Another bucket is unlimited and forwarded.
	resp, err = fresh().Get(r.front.URL + "/open/k") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck // test
	resp.Body.Close()              //nolint:errcheck // test
	if resp.StatusCode != 200 || r.null.Served() != 1 {
		t.Fatalf("open bucket: %d, backend served %d", resp.StatusCode, r.null.Served())
	}
	if v := metricValue(t, m, "tollbooth_limiter_decision_seconds", ""); v != 2 {
		t.Fatalf("every request through the limiter is timed: %v", v)
	}
}

func TestThrottleTooManyRequests(t *testing.T) {
	r := rigWithRules(t, strings.Replace(frozenRule, "limit: {rate: 0}", "limit: {rate: 0}\n      on_limit: too_many_requests", 1))
	resp, err := fresh().Get(r.front.URL + "/frozen/k") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close() //nolint:errcheck // test
	var e ErrorResponse
	if resp.StatusCode != http.StatusTooManyRequests || xml.Unmarshal(body, &e) != nil || e.Code != CodeTooManyRequests || resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("status %d body %s headers %v", resp.StatusCode, body, resp.Header)
	}
	r.waitLog(t, `"status":429`)
}

// Retry-After is the GCRA wait rounded up, so a client that honors it is admitted.
func TestThrottleRetryAfterIsHonest(t *testing.T) {
	r := rigWithRules(t, "rate_limits:\n  rules:\n    - id: slow\n      match: {buckets: [slow]}\n      limit: {rate: 0.5, burst: 1}\n")
	c := fresh()
	get := func() *http.Response {
		resp, err := c.Get(r.front.URL + "/slow/k") //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // test
		resp.Body.Close()              //nolint:errcheck // test
		return resp
	}
	if resp := get(); resp.StatusCode != 200 {
		t.Fatalf("first: %d", resp.StatusCode)
	}
	resp := get()
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "2" {
		t.Fatalf("second: %d Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

// The decision is made before any body byte is read: a denied PUT's body is never touched and the
// backend never sees it. A small unread body is discarded by Go's server and the connection stays
// open; a body past 256 KiB or an Expect: 100-continue request closes it (D-005).
func TestThrottleReadsNoBody(t *testing.T) {
	r := rigWithRules(t, frozenRule)
	var bodyReads atomic.Int64
	inner := r.h
	r.front.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.Body = countingBody{req.Body, &bodyReads}
		inner.ServeHTTP(w, req)
	})
	keepAlive := &http.Client{Transport: &http.Transport{}}
	put := func(size int) *http.Response {
		req, _ := http.NewRequest(http.MethodPut, r.front.URL+"/frozen/obj", bytes.NewReader(bytes.Repeat([]byte("x"), size))) //nolint:noctx // test
		resp, err := keepAlive.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // test
		resp.Body.Close()              //nolint:errcheck // test
		return resp
	}
	small := put(64 << 10)
	if small.StatusCode != 503 || small.Close {
		t.Fatalf("64 KiB denied PUT: status %d, Connection close %v (Go discards a small unread body and keeps the connection)", small.StatusCode, small.Close)
	}
	big := put(1 << 20)
	if big.StatusCode != 503 || !big.Close {
		t.Fatalf("1 MiB denied PUT: status %d, Connection close %v (an unread body past 256 KiB closes the connection)", big.StatusCode, big.Close)
	}
	if bodyReads.Load() != 0 {
		t.Fatalf("the handler read the body %d times", bodyReads.Load())
	}
	if r.null.Served() != 0 {
		t.Fatalf("backend served %d", r.null.Served())
	}
	r.waitLog(t, `"bytes_in":0`)
}

// A throttled Expect: 100-continue PUT: the 503 lands before Go's server would send 100 Continue,
// so the client sends zero body bytes; the connection is then closed because its body was never
// read (the reconnect cost the specification asks Phase 2 to measure; D-005).
func TestThrottledExpectContinueSendsNoBody(t *testing.T) {
	r := rigWithRules(t, frozenRule)
	var bodyReads atomic.Int64
	inner := r.h
	r.front.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.Body = countingBody{req.Body, &bodyReads}
		inner.ServeHTTP(w, req)
	})
	conn := rawDial(t, r.front)
	start := time.Now()
	fmt.Fprintf(conn, "PUT /frozen/huge HTTP/1.1\r\nHost: tb.example.com\r\nContent-Length: 5368709120\r\nExpect: 100-continue\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	body, _ := io.ReadAll(resp.Body)
	var e ErrorResponse
	if resp.StatusCode != 503 || xml.Unmarshal(body, &e) != nil || e.Code != CodeSlowDown || e.Resource != "/frozen/huge" {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if !resp.Close {
		t.Fatalf("a throttled Expect request must close the connection (its body was never read): %v", resp.Header)
	}
	if bodyReads.Load() != 0 || r.null.Served() != 0 {
		t.Fatalf("body reads %d, backend served %d", bodyReads.Load(), r.null.Served())
	}
	// Inside every client's expect-continue timeout (1 s in Go's transport and the AWS SDKs).
	if elapsed > 500*time.Millisecond {
		t.Fatalf("503 took %v; must land inside the client's 1 s expect timeout", elapsed)
	}
	if _, err := br.ReadByte(); err != io.EOF {
		t.Fatalf("expected the server to close after the 503, read err %v", err)
	}
	t.Logf("throttled Expect: 100-continue PUT answered in %v, zero body bytes, connection closed", elapsed)
}

// kill_switches.limiter_disable forwards everything.
func TestThrottleKillSwitch(t *testing.T) {
	r := rigWithRules(t, frozenRule+"kill_switches: {limiter_disable: true}\n")
	resp, err := fresh().Get(r.front.URL + "/frozen/k") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck // test
	resp.Body.Close()              //nolint:errcheck // test
	if resp.StatusCode != 200 || r.null.Served() != 1 {
		t.Fatalf("kill switch: %d, served %d", resp.StatusCode, r.null.Served())
	}
	if v := metricValue(t, r.h.Metrics, "tollbooth_limiter_decision_seconds", ""); v != 0 {
		t.Fatalf("the limiter ran with the kill switch on: %v samples", v)
	}
}

// Throttle decisions are logged at debug, one in logging.throttle_sample, with the rule id; the
// identity fields appear there because debug is the one level allowed to carry them.
func TestThrottleLogSampled(t *testing.T) {
	r := rigWithRules(t, frozenRule+"logging: {throttle_sample: 3}\n")
	buf := &syncBuf{}
	r.h.Log = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := fresh()
	for i := 0; i < 9; i++ {
		resp, err := c.Get(r.front.URL + "/frozen/k") //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close() //nolint:errcheck // test
	}
	r.waitLog(t, `"status":503`)
	lines := strings.Count(buf.String(), `"msg":"throttled"`)
	if lines != 3 {
		t.Fatalf("9 denials with throttle_sample 3 logged %d lines:\n%s", lines, buf.String())
	}
	if s := buf.String(); !strings.Contains(s, `"rule":"rule-secret-name"`) || !strings.Contains(s, `"bucket":"frozen"`) || !strings.Contains(s, `"retry_after":1`) || strings.Contains(s, "access_key") {
		t.Fatalf("log fields (no access key at any level, D-042):\n%s", s)
	}
	// At info nothing is logged for a denial.
	info := &syncBuf{}
	r.h.Log = slog.New(slog.NewJSONHandler(info, nil))
	resp, _ := c.Get(r.front.URL + "/frozen/k") //nolint:noctx // test
	resp.Body.Close()                           //nolint:errcheck // test
	if strings.Contains(info.String(), "throttled") {
		t.Fatalf("denial logged above debug: %s", info.String())
	}
}

// A limiter whose rule set makes every request pass still costs no allocation on the forwarded
// path, with 100 matching rules (docs/DESIGN.md §9: seven in total, zero in the limiter).
func TestHandlerAllocationsWithRules(t *testing.T) {
	var rules strings.Builder
	rules.WriteString("rate_limits:\n  rules:\n")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&rules, "    - id: r%d\n      match: {buckets: [b], ops: [READ], access_keys: [AKIAX]}\n      group_by: [access_key, bucket]\n      limit: {rate: 1000000000, burst: 1000000000}\n", i)
	}
	h, w, req := allocRig(t, rules.String())
	h.ServeHTTP(w, req)
	allocs := testing.AllocsPerRun(2000, func() {
		clear(w.h)
		h.ServeHTTP(w, req)
	})
	t.Logf("handler allocations per forwarded GetObject with 100 matching rules: %.2f", allocs)
	if allocs > 7 {
		t.Fatalf("handler allocates %.2f per request with rules; the budget is 7", allocs)
	}
}

// The denied path: identity, decision, and the pooled error body. Measured and pinned so the
// throttle storm cost cannot grow unnoticed (docs/DESIGN.md §9, denied path).
func TestThrottledAllocations(t *testing.T) {
	h, w, req := allocRig(t, "rate_limits:\n  rules:\n    - id: r\n      match: {buckets: [b]}\n      limit: {rate: 0}\n")
	h.ServeHTTP(w, req)
	allocs := testing.AllocsPerRun(2000, func() {
		clear(w.h)
		h.ServeHTTP(w, req)
	})
	t.Logf("handler allocations per throttled GetObject: %.2f", allocs)
	if allocs > throttledAllocBudget {
		t.Fatalf("throttled request allocates %.2f; the budget is %d", allocs, throttledAllocBudget)
	}
}

// throttledAllocBudget is the measured denied-path count (docs/DESIGN.md §9, D-041): the request
// id string, the Content-Length string, and the two single-value header slices that vary
// (Content-Length and X-Amz-Request-Id).
const throttledAllocBudget = 4

func allocRig(t *testing.T, tail string) (*Handler, *nopWriter, *http.Request) {
	t.Helper()
	c, err := config.Parse([]byte("node_id: test\nbackend: {scheme: http, endpoints: [127.0.0.1:1]}\nshowback: {dir: /tmp/tb-test}\n" + tail))
	if err != nil {
		t.Fatal(err)
	}
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
	lim := limiter.New(&m.Limiter)
	if err := lim.Apply(c); err != nil {
		t.Fatal(err)
	}
	h := New(cfg, pool, m, lim, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/b/k", nil)
	req.RequestURI = "/b/k"
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAX/20261109/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=x")
	return h, &nopWriter{h: http.Header{}}, req
}
