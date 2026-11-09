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

package main

// Record mode (-record): the recording backend and a tollbooth handler run in-process, the whole
// matrix runs against both, and for every case the requests the backend received direct and via
// the proxy are diffed as well as the responses. This is the request-side half of test plan
// category 5, and it needs no external backend, so it runs on every PR.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/blakegolliher/tollbooth/bench/nullbackend"
	"github.com/blakegolliher/tollbooth/internal/admin"
	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/proxy"
)

// requestHeaderAllow lists request headers that legitimately differ between what the backend saw
// direct and via the proxy: per-attempt SDK ids, per-request signatures and their inputs (the
// signing time differs between the two sends), and the hop-by-hop headers the proxy strips.
var requestHeaderAllow = map[string]bool{
	"Amz-Sdk-Invocation-Id": true, "Amz-Sdk-Request": true,
	"Authorization": true, "X-Amz-Date": true, "Date": true, "X-Amz-Security-Token": true,
	"Connection": true, "Keep-Alive": true,
}

// requestHeaderPresence lists request headers whose values differ per send but whose presence
// must match: a signature on one side and none on the other would be a transparency bug.
var requestHeaderPresence = []string{"Authorization", "X-Amz-Date", "Date"}

func runRecord(sizes []int, styles []string, sigModes bool) int {
	ctx := context.Background()
	const domain = "tb.example.com"
	ak, sk := "AKIARECORD", "record-secret"

	// One recording backend serves both sides; per case, its request log is split by the order
	// the matrix runs (direct first, then via).
	nb := nullbackend.New(4096)
	nb.Record = true
	be := httptest.NewServer(nb)
	defer be.Close()

	cfgText := "node_id: s3diff\nbase_domains: [" + domain + "]\nbackend: {scheme: http, endpoints: [" + strings.TrimPrefix(be.URL, "http://") + "]}\nshowback: {dir: /tmp/s3diff-record}\n"
	cfg, err := config.Parse([]byte(cfgText))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	published := &atomic.Pointer[config.Config]{}
	published.Store(cfg)
	metrics := admin.NewMetrics()
	pool, err := proxy.NewPool(&cfg.Backend, metrics, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	h := proxy.New(published, pool, metrics, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	front := httptest.NewUnstartedServer(h)
	front.Config = proxy.NewServer(h, &cfg.Listen, metrics, nil)
	front.Start()
	defer front.Close()

	var all []result
	failed, gaps := 0, 0
	for _, style := range styles {
		pathStyle := style == "path"
		direct, err := newTarget("direct", "http://"+domain, strings.TrimPrefix(be.URL, "http://"), domain, "us-east-1", ak, sk, "", pathStyle)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		via, err := newTarget("via", "http://"+domain, strings.TrimPrefix(front.URL, "http://"), domain, "us-east-1", ak, sk, "", pathStyle)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		// The matrix runs f(direct) then f(via): the backend's log for a case is the direct half
		// followed by the via half, equal in length when both sides sent the same steps.
		// The matrix runs f(direct) then f(via). Setup traffic a case sends direct (a PUT before a
		// DELETE) is not compared, so the via half is the last len(r.via) requests and the direct
		// half the same number just before it.
		afterCase := func(r *result) {
			reqs := nb.Requests()
			nb.Clear()
			n := len(r.via)
			if len(reqs) < 2*n {
				r.diffs = append(r.diffs, fmt.Sprintf("request count: backend saw %d requests, expected at least %d", len(reqs), 2*n))
				return
			}
			directHalf, viaHalf := reqs[len(reqs)-2*n:len(reqs)-n], reqs[len(reqs)-n:]
			for i := 0; i < n; i++ {
				d, v := directHalf[i], viaHalf[i]
				half := n
				step := ""
				if half > 1 {
					step = fmt.Sprintf("request step %d: ", i+1)
				}
				for _, diff := range diffRequests(d, v) {
					r.diffs = append(r.diffs, step+diff)
				}
			}
		}
		setup := func(f func() error) error {
			before := nb.Recorded()
			err := f()
			nb.Drop(nb.Recorded() - before)
			return err
		}
		rs, f, g := runMatrix(direct, via, matrixOptions{ctx: ctx, style: style, bucket: "s3diff-" + style, sizes: sizes, sigModes: sigModes, afterCase: afterCase, setup: setup})
		all = append(all, rs...)
		failed += f
		gaps += g
	}
	return report(all, failed, gaps, "mode=record (in-process recording backend and tollbooth; requests and responses compared)")
}

// diffRequests compares what the backend received for one request on each side.
func diffRequests(d, v nullbackend.Request) []string {
	var out []string
	if d.Method != v.Method {
		out = append(out, fmt.Sprintf("request method: direct=%s via=%s", d.Method, v.Method))
	}
	if d.RequestURI != v.RequestURI {
		out = append(out, fmt.Sprintf("request-target: direct=%q via=%q", d.RequestURI, v.RequestURI))
	}
	if d.Host != v.Host {
		out = append(out, fmt.Sprintf("request Host: direct=%q via=%q", d.Host, v.Host))
	}
	if !bytes.Equal(d.Body, v.Body) {
		out = append(out, fmt.Sprintf("request body: direct=%d bytes via=%d bytes", len(d.Body), len(v.Body)))
	}
	keys := map[string]bool{}
	for k := range d.Header {
		keys[k] = true
	}
	for k := range v.Header {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		if requestHeaderAllow[k] {
			continue
		}
		if a, b := strings.Join(d.Header[k], ","), strings.Join(v.Header[k], ","); a != b {
			out = append(out, fmt.Sprintf("request header %s: direct=%q via=%q", k, a, b))
		}
	}
	for _, k := range requestHeaderPresence {
		_, dp := d.Header[k]
		_, vp := v.Header[k]
		if dp != vp {
			out = append(out, fmt.Sprintf("request header %s present: direct=%v via=%v", k, dp, vp))
		}
	}
	if len(d.Trailer) != len(v.Trailer) {
		out = append(out, fmt.Sprintf("request trailers: direct=%v via=%v", d.Trailer, v.Trailer))
	}
	return out
}

var _ = http.MethodGet
