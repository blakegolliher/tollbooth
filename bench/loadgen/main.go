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

// Command loadgen is the in-repo load generator (ADR-0002). Two modes:
//
//	-mode raw   presigned GET URLs fired over net/http, for the added-latency rows: no signing
//	            or SDK middleware on the hot path, so proxied minus direct is the proxy's cost
//	-mode sdk   an aws-sdk-go-v2 operation mix with the SDK's own retries, for realistic traffic
//	            and the throttle path
//
// -direct and -proxied name the two endpoints; runs alternate between them (-runs each, each
// preceded by -warmup) so both see the same machine state, and the median run per target is
// reported with the added latency. Key distributions: -keys access keys, -buckets, -prefixes,
// -sources bind addresses. Latencies go into HDR histograms, one per worker, merged at the end;
// the report is printed and written as JSON under -out.
//
// math/rand/v2 shapes the workload (which URL, which operation); it is not security material, so
// the crypto-randomness rule is suppressed on its import.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2" // nosemgrep
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
)

type options struct {
	mode, direct, proxied, label, out, ops, region, bind, bucketBase string
	concurrency, keys, buckets, prefixes, objects, size, runs        int
	duration, warmup                                                 time.Duration
	adaptive                                                         bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	var o options
	fs := flag.NewFlagSet("loadgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.mode, "mode", "raw", "raw (presigned GETs over net/http) or sdk (aws-sdk-go-v2 operation mix)")
	fs.StringVar(&o.direct, "direct", "", "backend endpoint, http://host:port; empty skips the direct runs")
	fs.StringVar(&o.proxied, "proxied", "", "proxy endpoint, http://host:port; empty skips the proxied runs")
	fs.StringVar(&o.label, "label", "", "name for the result file")
	fs.StringVar(&o.out, "out", "", "directory for the JSON result; empty writes none")
	fs.StringVar(&o.ops, "ops", "GET=100", "sdk mode operation mix, e.g. GET=70,PUT=20,HEAD=10 (GET, PUT, HEAD, LIST, DELETE)")
	fs.StringVar(&o.region, "region", "us-east-1", "signing region")
	fs.StringVar(&o.bind, "bind", "", "comma-separated local addresses to bind clients to (one transport per address, -sources is their count)")
	fs.IntVar(&o.concurrency, "concurrency", 32, "concurrent workers")
	fs.IntVar(&o.keys, "keys", 1, "distinct access keys (AKIALOAD000, …)")
	fs.StringVar(&o.bucketBase, "bucket-base", "load-", "bucket names are this followed by 0, 1, …")
	fs.IntVar(&o.buckets, "buckets", 1, "distinct buckets (<bucket-base>0, …)")
	fs.IntVar(&o.prefixes, "prefixes", 1, "distinct prefixes under each bucket (p0/, …)")
	fs.IntVar(&o.objects, "objects", 1000, "distinct object names per prefix")
	fs.IntVar(&o.size, "size", 4096, "PUT body size in sdk mode")
	fs.IntVar(&o.runs, "runs", 3, "measured runs per target, interleaved")
	fs.DurationVar(&o.duration, "duration", 30*time.Second, "length of one measured run")
	fs.DurationVar(&o.warmup, "warmup", 30*time.Second, "unmeasured warm-up before each run")
	fs.BoolVar(&o.adaptive, "adaptive", false, "sdk mode: use the SDK's adaptive retry mode instead of standard")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if o.direct == "" && o.proxied == "" {
		_, _ = fmt.Fprintln(stderr, "loadgen: at least one of -direct or -proxied is required")
		return 2
	}
	if o.mode != "raw" && o.mode != "sdk" {
		_, _ = fmt.Fprintln(stderr, "loadgen: -mode must be raw or sdk")
		return 2
	}
	rep, err := execute(&o, stdout)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "loadgen:", err)
		return 1
	}
	if o.out != "" {
		if err := os.MkdirAll(o.out, 0o750); err != nil {
			_, _ = fmt.Fprintln(stderr, "loadgen:", err)
			return 1
		}
		name := time.Now().UTC().Format("20060102T150405Z") + "-" + o.mode
		if o.label != "" {
			name += "-" + o.label
		}
		b, _ := json.MarshalIndent(rep, "", "  ")
		path := filepath.Join(o.out, name+".json")
		if err := os.WriteFile(path, b, 0o644); err != nil { //nolint:gosec // a result file
			_, _ = fmt.Fprintln(stderr, "loadgen:", err)
			return 1
		}
		_, _ = fmt.Fprintln(stdout, "wrote", path)
	}
	return 0
}

// Report is the JSON result: every run in order plus the median per target and the deltas.
type Report struct {
	Mode        string            `json:"mode"`
	Options     options           `json:"options"`
	Environment map[string]string `json:"environment"`
	Runs        []Run             `json:"runs"`
	Median      map[string]Run    `json:"median"` // by target
	// Added is proxied minus direct for the median runs, in microseconds, when both ran.
	Added map[string]float64 `json:"added_latency_us,omitempty"`
}

// Run is one measured interval against one target.
type Run struct {
	Target      string             `json:"target"`
	Index       int                `json:"index"`
	Seconds     float64            `json:"seconds"`
	Requests    int64              `json:"requests"`
	RPS         float64            `json:"rps"`
	Bytes       int64              `json:"bytes"`
	Status      map[string]int     `json:"status"`
	Errors      int64              `json:"errors"`
	Attempts    int64              `json:"attempts,omitempty"`  // sdk mode: HTTP attempts including retries
	Throttled   int64              `json:"throttled,omitempty"` // sdk mode: attempts answered SlowDown or TooManyRequests
	Percentiles map[string]float64 `json:"latency_us"`          // p50, p90, p99, p99.9, max
}

func environment() map[string]string {
	env := map[string]string{
		"go": runtime.Version(), "gomaxprocs": strconv.Itoa(runtime.GOMAXPROCS(0)), "num_cpu": strconv.Itoa(runtime.NumCPU()),
		"gogc": os.Getenv("GOGC"), "gomemlimit": os.Getenv("GOMEMLIMIT"),
	}
	if h, err := os.Hostname(); err == nil {
		env["host"] = h
	}
	if b, err := os.ReadFile("/proc/version"); err == nil {
		env["kernel"] = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "model name") {
				env["cpu"] = strings.TrimSpace(strings.SplitN(l, ":", 2)[1])
				break
			}
		}
	}
	return env
}

func execute(o *options, stdout io.Writer) (*Report, error) {
	rep := &Report{Mode: o.mode, Options: *o, Environment: environment(), Median: map[string]Run{}}
	targets := []struct{ name, url string }{}
	if o.direct != "" {
		targets = append(targets, struct{ name, url string }{"direct", o.direct})
	}
	if o.proxied != "" {
		targets = append(targets, struct{ name, url string }{"proxied", o.proxied})
	}
	byTarget := map[string][]Run{}
	for i := 1; i <= o.runs; i++ {
		for _, t := range targets {
			w, err := newWorkload(o, t.url)
			if err != nil {
				return nil, err
			}
			if o.warmup > 0 {
				_, _ = fmt.Fprintf(stdout, "run %d/%d %-7s warm-up %s\n", i, o.runs, t.name, o.warmup)
				w.measure(o.warmup, o.concurrency)
			}
			_, _ = fmt.Fprintf(stdout, "run %d/%d %-7s measuring %s … ", i, o.runs, t.name, o.duration)
			r := w.measure(o.duration, o.concurrency)
			r.Target, r.Index = t.name, i
			_, _ = fmt.Fprintf(stdout, "%d requests, %.0f req/s, p50 %.0f µs, p99 %.0f µs, p99.9 %.0f µs, errors %d",
				r.Requests, r.RPS, r.Percentiles["p50"], r.Percentiles["p99"], r.Percentiles["p99.9"], r.Errors)
			if r.Attempts > 0 {
				_, _ = fmt.Fprintf(stdout, ", attempts %d, throttled %d", r.Attempts, r.Throttled)
			}
			_, _ = fmt.Fprintln(stdout)
			rep.Runs = append(rep.Runs, r)
			byTarget[t.name] = append(byTarget[t.name], r)
			w.close()
		}
	}
	for name, runs := range byTarget {
		sort.Slice(runs, func(a, b int) bool { return runs[a].Percentiles["p50"] < runs[b].Percentiles["p50"] })
		rep.Median[name] = runs[len(runs)/2]
	}
	_, _ = fmt.Fprintf(stdout, "\n%-8s %10s %10s %10s %10s %10s %10s %8s %10s %10s\n", "median", "req/s", "p50 µs", "p90 µs", "p99 µs", "p99.9 µs", "max µs", "errors", "attempts", "throttled")
	for _, name := range []string{"direct", "proxied"} {
		r, ok := rep.Median[name]
		if !ok {
			continue
		}
		_, _ = fmt.Fprintf(stdout, "%-8s %10.0f %10.0f %10.0f %10.0f %10.0f %10.0f %8d %10d %10d\n", name, r.RPS, r.Percentiles["p50"], r.Percentiles["p90"], r.Percentiles["p99"], r.Percentiles["p99.9"], r.Percentiles["max"], r.Errors, r.Attempts, r.Throttled)
	}
	if d, ok := rep.Median["direct"]; ok {
		if p, ok := rep.Median["proxied"]; ok {
			rep.Added = map[string]float64{}
			for _, q := range []string{"p50", "p90", "p99", "p99.9"} {
				rep.Added[q] = p.Percentiles[q] - d.Percentiles[q]
			}
			_, _ = fmt.Fprintf(stdout, "%-8s %10s %10.0f %10.0f %10.0f %10.0f\n", "added", "", rep.Added["p50"], rep.Added["p90"], rep.Added["p99"], rep.Added["p99.9"])
		}
	}
	return rep, nil
}

// workload is one target's request source: raw URLs with net/http clients, or SDK clients.
type workload struct {
	o          *options
	transports []*http.Transport
	urls       []string
	clients    []*s3.Client
	ops        []opKind
	attempts   atomic.Int64
	throttled  atomic.Int64
}

type opKind uint8

const (
	opGet opKind = iota
	opPut
	opHead
	opList
	opDelete
)

func newWorkload(o *options, endpoint string) (*workload, error) {
	w := &workload{o: o}
	binds := []string{""}
	if o.bind != "" {
		binds = strings.Split(o.bind, ",")
	}
	for _, b := range binds {
		d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
		if b != "" {
			ip := net.ParseIP(strings.TrimSpace(b))
			if ip == nil {
				return nil, fmt.Errorf("bad bind address %q", b)
			}
			d.LocalAddr = &net.TCPAddr{IP: ip}
		}
		w.transports = append(w.transports, &http.Transport{
			DialContext: d.DialContext, MaxIdleConns: o.concurrency * 2, MaxIdleConnsPerHost: o.concurrency * 2,
			IdleConnTimeout: 90 * time.Second, DisableCompression: true, ForceAttemptHTTP2: false,
		})
	}
	ctx := context.Background()
	for k := 0; k < max(1, o.keys); k++ {
		ak := fmt.Sprintf("AKIALOAD%03d", k)
		cfg := aws.Config{Region: o.region, Credentials: credentials.NewStaticCredentialsProvider(ak, "load-secret", "")}
		if o.adaptive {
			cfg.Retryer = func() aws.Retryer { return retry.NewAdaptiveMode() }
		}
		cfg.APIOptions = append(cfg.APIOptions, func(stack *middleware.Stack) error {
			return stack.Deserialize.Add(middleware.DeserializeMiddlewareFunc("loadgen-attempts", func(ctx context.Context, in middleware.DeserializeInput, next middleware.DeserializeHandler) (middleware.DeserializeOutput, middleware.Metadata, error) {
				w.attempts.Add(1)
				out, md, err := next.HandleDeserialize(ctx, in)
				var api smithy.APIError
				if err != nil && errors.As(err, &api) && (api.ErrorCode() == "SlowDown" || api.ErrorCode() == "TooManyRequests") {
					w.throttled.Add(1)
				}
				return out, md, err
			}), middleware.Before)
		})
		tr := w.transports[k%len(w.transports)]
		c := s3.NewFromConfig(cfg, func(so *s3.Options) {
			so.BaseEndpoint = aws.String(endpoint)
			so.UsePathStyle = true
			so.HTTPClient = &http.Client{Transport: tr}
		})
		w.clients = append(w.clients, c)
		if o.mode == "raw" {
			ps := s3.NewPresignClient(c, func(po *s3.PresignOptions) { po.Expires = 24 * time.Hour })
			for b := 0; b < max(1, o.buckets); b++ {
				for p := 0; p < max(1, o.prefixes); p++ {
					for n := 0; n < min(max(1, o.objects), 64); n++ { // a bounded URL set per (key, bucket, prefix)
						req, err := ps.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(fmt.Sprintf("%s%d", o.bucketBase, b)), Key: aws.String(fmt.Sprintf("p%d/obj-%d", p, n))})
						if err != nil {
							return nil, err
						}
						w.urls = append(w.urls, req.URL)
					}
				}
			}
		}
	}
	if o.mode == "sdk" {
		for _, part := range strings.Split(o.ops, ",") {
			name, weight, _ := strings.Cut(strings.TrimSpace(part), "=")
			n, err := strconv.Atoi(weight)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("bad -ops entry %q", part)
			}
			var kind opKind
			switch strings.ToUpper(name) {
			case "GET":
				kind = opGet
			case "PUT":
				kind = opPut
			case "HEAD":
				kind = opHead
			case "LIST":
				kind = opList
			case "DELETE":
				kind = opDelete
			default:
				return nil, fmt.Errorf("unknown op %q in -ops", name)
			}
			for i := 0; i < n; i++ {
				w.ops = append(w.ops, kind)
			}
		}
		if len(w.ops) == 0 {
			return nil, errors.New("-ops has no weight")
		}
	}
	return w, nil
}

func (w *workload) close() {
	for _, t := range w.transports {
		t.CloseIdleConnections()
	}
}

// measure runs concurrency workers for d and returns the merged result. Each worker owns a
// histogram and a status map; nothing is shared until the merge.
func (w *workload) measure(d time.Duration, concurrency int) Run {
	type local struct {
		h      *hdrhistogram.Histogram
		status map[int]int
		bytes  int64
		errs   int64
	}
	locals := make([]*local, concurrency)
	w.attempts.Store(0)
	w.throttled.Store(0)
	var wg sync.WaitGroup
	start := time.Now()
	deadline := start.Add(d)
	for i := 0; i < concurrency; i++ {
		l := &local{h: hdrhistogram.New(1, int64(60*time.Second), 3), status: map[int]int{}}
		locals[i] = l
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(i), uint64(start.UnixNano()))) //nolint:gosec // workload shaping, not security
			client := &http.Client{Transport: w.transports[i%len(w.transports)]}
			body := bytes.Repeat([]byte("x"), w.o.size)
			for time.Now().Before(deadline) {
				t0 := time.Now()
				var status int
				var n int64
				var err error
				if w.o.mode == "raw" {
					status, n, err = w.rawGet(client, w.urls[rng.IntN(len(w.urls))])
				} else {
					status, n, err = w.sdkOp(rng, body)
				}
				lat := time.Since(t0)
				_ = l.h.RecordValue(int64(lat))
				l.bytes += n
				if err != nil {
					l.errs++
					l.status[0]++
				} else {
					l.status[status]++
				}
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)
	merged := hdrhistogram.New(1, int64(60*time.Second), 3)
	r := Run{Seconds: elapsed.Seconds(), Status: map[string]int{}, Percentiles: map[string]float64{}}
	for _, l := range locals {
		merged.Merge(l.h)
		r.Bytes += l.bytes
		r.Errors += l.errs
		for s, c := range l.status {
			k := strconv.Itoa(s)
			if s == 0 {
				k = "error"
			}
			r.Status[k] += c
		}
	}
	r.Requests = merged.TotalCount()
	r.RPS = float64(r.Requests) / elapsed.Seconds()
	for name, q := range map[string]float64{"p50": 50, "p90": 90, "p99": 99, "p99.9": 99.9} {
		r.Percentiles[name] = float64(merged.ValueAtQuantile(q)) / 1e3
	}
	r.Percentiles["max"] = float64(merged.Max()) / 1e3
	if w.o.mode == "sdk" {
		r.Attempts, r.Throttled = w.attempts.Load(), w.throttled.Load()
	}
	return r
}

func (w *workload) rawGet(c *http.Client, url string) (status int, n int64, err error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		return 0, 0, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, 0, err
	}
	n, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, n, nil
}

func (w *workload) sdkOp(rng *rand.Rand, body []byte) (status int, n int64, err error) {
	ctx := context.Background()
	c := w.clients[rng.IntN(len(w.clients))]
	bucket := aws.String(fmt.Sprintf("%s%d", w.o.bucketBase, rng.IntN(max(1, w.o.buckets))))
	key := aws.String(fmt.Sprintf("p%d/obj-%d", rng.IntN(max(1, w.o.prefixes)), rng.IntN(max(1, w.o.objects))))
	switch w.ops[rng.IntN(len(w.ops))] {
	case opGet:
		var out *s3.GetObjectOutput
		out, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: key})
		if err != nil {
			return 0, 0, err
		}
		n, _ = io.Copy(io.Discard, out.Body)
		_ = out.Body.Close()
		return 200, n, nil
	case opPut:
		_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: key, Body: bytes.NewReader(body)})
		return 200, int64(len(body)), err
	case opHead:
		_, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: key})
		return 200, 0, err
	case opList:
		_, err = c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket, Prefix: aws.String(fmt.Sprintf("p%d/", rng.IntN(max(1, w.o.prefixes))))})
		return 200, 0, err
	default:
		_, err = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: key})
		return 204, 0, err
	}
}
