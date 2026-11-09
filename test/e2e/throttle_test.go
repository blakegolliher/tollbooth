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

// Package e2e holds the end-to-end scenarios that run the real proxy in-process against the null
// backend with real SDK clients (test plan categories 7 and 9). Nothing here touches the network
// beyond loopback.
package e2e

import (
	"bytes"
	"context"
	"errors"
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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/blakegolliher/tollbooth/bench/nullbackend"
	"github.com/blakegolliher/tollbooth/internal/admin"
	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/limiter"
	"github.com/blakegolliher/tollbooth/internal/proxy"
)

// rig is the proxy in-process in front of one null backend, with the rules of a YAML tail.
type rig struct {
	nb      *nullbackend.Server
	front   *httptest.Server
	lim     *limiter.Limiter
	metrics *admin.Metrics
	// accepted counts TCP connections the proxy accepted, for the reconnect-cost measurement.
	accepted atomic.Int64
}

func start(t *testing.T, tail string) *rig {
	t.Helper()
	r := &rig{nb: nullbackend.New(4096)}
	be := httptest.NewServer(r.nb)
	t.Cleanup(be.Close)
	y := "node_id: e2e\nbackend: {scheme: http, endpoints: [" + strings.TrimPrefix(be.URL, "http://") + "]}\nshowback: {dir: /tmp/tb-e2e}\n" + tail
	c, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	published := &atomic.Pointer[config.Config]{}
	published.Store(c)
	r.metrics = admin.NewMetrics()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := proxy.NewPool(&c.Backend, r.metrics, log)
	if err != nil {
		t.Fatal(err)
	}
	r.lim = limiter.New(&r.metrics.Limiter)
	if err := r.lim.Apply(c); err != nil {
		t.Fatal(err)
	}
	h := proxy.New(published, pool, r.metrics, r.lim, nil, log)
	r.front = httptest.NewUnstartedServer(h)
	r.front.Config = proxy.NewServer(h, &c.Listen, r.metrics, log)
	r.front.Listener = &countingListener{Listener: r.front.Listener, n: &r.accepted}
	r.front.Start()
	t.Cleanup(r.front.Close)
	return r
}

type countingListener struct {
	net.Listener
	n *atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.n.Add(1)
	}
	return c, err
}

// client is an aws-sdk-go-v2 S3 client at the proxy. maxAttempts 0 keeps the SDK's default retry
// settings (standard mode, 3 attempts, exponential backoff with jitter, x-amz-retry-after
// honored), which is what category 7 tests; adaptive selects the SDK's own throttling-aware mode
// (client-side rate limiting that adapts to SlowDown), as AWS_RETRY_MODE=adaptive would.
func (r *rig) client(maxAttempts int, adaptive bool) *s3.Client {
	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIAE2E", "e2e-secret", ""),
	}
	if maxAttempts > 0 {
		cfg.RetryMaxAttempts = maxAttempts
	}
	if adaptive {
		cfg.Retryer = func() aws.Retryer { return retry.NewAdaptiveMode() }
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(r.front.URL)
		o.UsePathStyle = true
	})
}

func (r *rig) denied(rule string) float64 {
	fams, _ := r.metrics.Registry.Gather()
	for _, f := range fams {
		if f.GetName() != "tollbooth_limiter_denied_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "rule" && lp.GetValue() == rule {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// The 503 body is an S3 error the SDK parses: code, message, request id, and the SDK's own
// classification of SlowDown as a throttling error. Same for 429 TooManyRequests.
func TestSDKParsesThrottleResponse(t *testing.T) {
	r := start(t, `
rate_limits:
  rules:
    - id: frozen-503
      match: {buckets: [frozen]}
      limit: {rate: 0}
    - id: frozen-429
      match: {buckets: [frozen429]}
      limit: {rate: 0}
      on_limit: too_many_requests
`)
	c := r.client(1, false)
	for bucket, code := range map[string]string{"frozen": "SlowDown", "frozen429": "TooManyRequests"} {
		_, err := c.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("k")})
		if err == nil {
			t.Fatalf("%s: expected an error", bucket)
		}
		var api smithy.APIError
		if !errors.As(err, &api) || api.ErrorCode() != code || api.ErrorMessage() != "Please reduce your request rate." {
			t.Fatalf("%s: %v", bucket, err)
		}
		var re *smithyhttp.ResponseError
		if !errors.As(err, &re) || len(re.Response.Header.Get("x-amz-request-id")) != 32 || re.Response.Header.Get("Retry-After") != "1" || re.Response.Header.Get("x-amz-retry-after") != "1000" {
			t.Fatalf("%s: response error %v", bucket, err)
		}
		if bucket == "frozen" && re.Response.StatusCode != 503 || bucket == "frozen429" && re.Response.StatusCode != 429 {
			t.Fatalf("%s: status %d", bucket, re.Response.StatusCode)
		}
		var rid interface{ ServiceRequestID() string }
		if !errors.As(err, &rid) || rid.ServiceRequestID() == "" {
			t.Fatalf("%s: the SDK did not extract the request id: %v", bucket, err)
		}
	}
	if r.nb.Served() != 0 {
		t.Fatalf("backend served %d", r.nb.Served())
	}
}

// Gate 2 criterion: SDK clients complete work under a 1 req/s rule via their own retries. The Go
// SDK's default retryer (standard mode, 3 attempts, exponential backoff with jitter) is used as is;
// it waits at least the proxy's x-amz-retry-after (D-043), so a sequential client never burns its
// three attempts inside one emission interval. The test reports attempts, failures, and time.
func TestGoSDKCompletesUnderOneRequestPerSecond(t *testing.T) {
	r := start(t, `
rate_limits:
  rules:
    - id: one-per-second
      match: {buckets: [slow]}
      limit: {rate: 1, burst: 1}
`)
	c := r.client(0, false)
	const objects = 5
	start := time.Now()
	failed := 0
	for i := 0; i < objects; i++ {
		_, err := c.PutObject(context.Background(), &s3.PutObjectInput{Bucket: aws.String("slow"), Key: aws.String(fmt.Sprintf("o%d", i)), Body: bytes.NewReader(bytes.Repeat([]byte("x"), 4096))})
		if err != nil {
			failed++
			t.Logf("object %d failed after the SDK's retries: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	denied := r.denied("one-per-second")
	t.Logf("%d sequential PutObjects under a 1 req/s rule with the Go SDK's default retries: %d failed, %.0f throttled attempts, %v total (floor %v)", objects, failed, denied, elapsed.Round(time.Millisecond), (objects-1)*time.Second)
	if served := r.nb.Served(); int(served) != objects-failed {
		t.Fatalf("backend served %d, want %d", served, objects-failed)
	}
	if failed != 0 {
		t.Fatalf("%d of %d objects failed under a 1 req/s rule with default retries", failed, objects)
	}
	if elapsed < (objects-1)*time.Second {
		t.Fatalf("%d objects at 1 req/s finished in %v, faster than the rule allows", objects, elapsed)
	}
}

// Category 7: a 200-object sync at concurrency 40 against a rule whose rate is half the
// concurrency and whose burst equals it, in both of the Go SDK's retry modes. Invariants for both:
// the backend served exactly the objects the client counts as done, every failure the client saw
// is the proxy's throttle error, and the rule denied. Adaptive mode, the SDK's own answer to
// SlowDown, must complete every object; standard mode's result is recorded (its three attempts
// can run out when half of all requests are denied and forty retries land together).
func TestGoSDKSyncAgainstHalfConcurrency(t *testing.T) {
	const concurrency, objects = 40, 200
	for _, mode := range []struct {
		name     string
		adaptive bool
	}{{"standard-default", false}, {"adaptive", true}} {
		t.Run(mode.name, func(t *testing.T) {
			r := start(t, fmt.Sprintf(`
rate_limits:
  rules:
    - id: sync
      match: {buckets: [sync]}
      group_by: [access_key]
      limit: {rate: %d, burst: %d}
`, concurrency/2, concurrency))
			c := r.client(0, mode.adaptive)
			var failed, wrongErr atomic.Int64
			var wg sync.WaitGroup
			sem := make(chan struct{}, concurrency)
			start := time.Now()
			for i := 0; i < objects; i++ {
				wg.Add(1)
				sem <- struct{}{}
				go func(i int) {
					defer wg.Done()
					defer func() { <-sem }()
					_, err := c.PutObject(context.Background(), &s3.PutObjectInput{Bucket: aws.String("sync"), Key: aws.String(fmt.Sprintf("o%d", i)), Body: bytes.NewReader(bytes.Repeat([]byte("x"), 1024))})
					if err != nil {
						failed.Add(1)
						var api smithy.APIError
						slowDown := errors.As(err, &api) && api.ErrorCode() == "SlowDown"
						if !slowDown && !strings.Contains(err.Error(), "retry quota exceeded") {
							wrongErr.Add(1)
							t.Logf("unexpected error: %v", err)
						}
					}
				}(i)
			}
			wg.Wait()
			elapsed := time.Since(start)
			denied := r.denied("sync")
			floor := time.Duration(float64(objects-concurrency) / float64(concurrency/2) * float64(time.Second))
			t.Logf("%s: %d objects, concurrency %d, rule rate %d burst %d: %d failed, %.0f throttled attempts, %v (floor %v)",
				mode.name, objects, concurrency, concurrency/2, concurrency, failed.Load(), denied, elapsed.Round(time.Millisecond), floor)
			if served := r.nb.Served(); served != objects-failed.Load() {
				t.Fatalf("backend served %d, want %d (client-side failures must not have reached the backend)", served, objects-failed.Load())
			}
			if wrongErr.Load() != 0 {
				t.Fatalf("%d failures were not the proxy's throttle error", wrongErr.Load())
			}
			if denied == 0 {
				t.Fatal("the rule never denied; the test did not exercise throttling")
			}
			if mode.adaptive && failed.Load() != 0 {
				t.Fatalf("adaptive mode: %d of %d objects failed", failed.Load(), objects)
			}
		})
	}
}

// A throttled Expect: 100-continue PUT sends zero body bytes, the 503 lands inside the client's
// expect timeout, and the connection is closed, so every denied upload costs a reconnect. Measured
// here with Go's transport (ExpectContinueTimeout 1 s, the SDKs' value) and recorded for the gate
// report; the per-tool numbers (aws cli, boto3, s5cmd) come from the integration stage.
func TestThrottledExpectContinueReconnectCost(t *testing.T) {
	r := start(t, `
rate_limits:
  rules:
    - id: frozen
      match: {buckets: [frozen]}
      limit: {rate: 0}
`)
	body := bytes.Repeat([]byte("x"), 4<<20)
	tr := &http.Transport{ExpectContinueTimeout: time.Second}
	hc := &http.Client{Transport: tr}
	const n = 20
	r.accepted.Store(0)
	start := time.Now()
	for i := 0; i < n; i++ {
		req, _ := http.NewRequest(http.MethodPut, r.front.URL+"/frozen/big", bytes.NewReader(body)) //nolint:noctx // test
		req.Header.Set("Expect", "100-continue")
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // test
		resp.Body.Close()              //nolint:errcheck // test
		if resp.StatusCode != 503 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	expectElapsed := time.Since(start)
	expectConns := r.accepted.Load()

	// The same denials as GETs reuse one connection.
	r.accepted.Store(0)
	start = time.Now()
	for i := 0; i < n; i++ {
		resp, err := hc.Get(r.front.URL + "/frozen/k") //nolint:noctx // test
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // test
		resp.Body.Close()              //nolint:errcheck // test
	}
	getElapsed := time.Since(start)
	getConns := r.accepted.Load()
	t.Logf("%d throttled Expect PUTs (4 MiB declared): %d connections, %v per denial; %d throttled GETs: %d connections, %v per denial; reconnect cost %v per denied upload",
		n, expectConns, (expectElapsed / n).Round(time.Microsecond), n, getConns, (getElapsed / n).Round(time.Microsecond), ((expectElapsed - getElapsed) / n).Round(time.Microsecond))
	if expectConns != n {
		t.Fatalf("expected one connection per throttled Expect PUT, got %d for %d", expectConns, n)
	}
	if getConns != 1 {
		t.Fatalf("throttled GETs should share one keep-alive connection, got %d", getConns)
	}
	if r.nb.Served() != 0 {
		t.Fatalf("backend served %d requests", r.nb.Served())
	}
	if v := r.metrics.Registry; v == nil {
		t.Fatal("metrics")
	}
}
