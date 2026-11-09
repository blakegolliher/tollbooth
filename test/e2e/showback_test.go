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

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/blakegolliher/tollbooth/internal/showback"
)

// Reconciliation (test plan category 8, the acceptance test of docs/SPEC.md, Showback): a
// generator with a known request mix runs through the proxy with showback on; after the flush the
// store's sums equal the generator's own counts exactly for completed requests, throttled requests
// land under status_class throttled, and bytes_out for an aborted download equals the bytes the
// proxy actually wrote.
func TestShowbackReconciliation(t *testing.T) {
	r := start(t, `
showback: {dir: /tmp/tb-e2e, dimensions: [access_key, bucket, prefix, operation, status_class], prefixes: {recon: [ckpt/, logs/]}}
rate_limits:
  rules:
    - id: frozen
      match: {buckets: [frozen]}
      limit: {rate: 0}
`)
	// A listing's response has a body too (the backend's XML); its size, measured once direct
	// at the backend, is what each list contributes to bytes_out.
	lresp, err := http.Get(r.beURL + "/recon?list-type=2&prefix=ckpt%2F") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	listBody, _ := io.Copy(io.Discard, lresp.Body)
	lresp.Body.Close() //nolint:errcheck // test
	const workers, perWorker = 8, 50
	var puts, gets, lists, throttled atomic.Int64
	var bytesIn, bytesOut atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			c := r.client(1, false)
			for i := 0; i < perWorker; i++ {
				switch i % 4 {
				case 0, 1:
					body := bytes.Repeat([]byte("x"), 1000+i)
					if _, err := c.PutObject(context.Background(), &s3.PutObjectInput{Bucket: aws.String("recon"), Key: aws.String(fmt.Sprintf("ckpt/%d-%d", w, i)), Body: bytes.NewReader(body)}); err == nil {
						puts.Add(1)
						bytesIn.Add(int64(len(body)))
					} else {
						t.Error(err)
					}
				case 2:
					out, err := c.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String("recon"), Key: aws.String(fmt.Sprintf("logs/%d", i))})
					if err != nil {
						t.Error(err)
						continue
					}
					n, _ := io.Copy(io.Discard, out.Body)
					out.Body.Close() //nolint:errcheck // drained
					gets.Add(1)
					bytesOut.Add(n)
				case 3:
					if _, err := c.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String("frozen"), Key: aws.String("k")}); err != nil {
						throttled.Add(1)
					} else {
						t.Error("frozen bucket answered")
					}
					if _, err := c.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{Bucket: aws.String("recon"), Prefix: aws.String("ckpt/")}); err == nil {
						lists.Add(1)
					} else {
						t.Error(err)
					}
				}
			}
		}(w)
	}
	wg.Wait()
	// An aborted download: read 10 bytes of a 4096-byte body and close; bytes_out must be what the
	// proxy wrote, which is at most the body and at least what we read.
	c := r.client(1, false)
	out, err := c.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String("recon"), Key: aws.String("abort")})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	_, _ = io.ReadFull(out.Body, buf)
	out.Body.Close()                   //nolint:errcheck // the point
	time.Sleep(100 * time.Millisecond) // the handler's finish runs after the client closes

	dir := t.TempDir()
	st := showback.NewStore(r.sb, dir, "e2e", 0, 0, nil, nil)
	st.Flush()
	files, err := showback.Files(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := showback.Query{GroupBy: []string{"operation", "status_class", "prefix"}}.Run(files)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][3]int64{}
	for _, g := range res.Groups {
		got[g.Keys["operation"]+"/"+g.Keys["status_class"]+"/"+g.Keys["prefix"]] = [3]int64{g.Requests, g.BytesIn, g.BytesOut}
	}
	t.Logf("generator: puts %d (%d bytes in), gets %d (%d bytes out), lists %d, throttled %d; store: %v", puts.Load(), bytesIn.Load(), gets.Load(), bytesOut.Load(), lists.Load(), throttled.Load(), got)
	check := func(key string, reqs, in, outB int64) {
		t.Helper()
		if got[key] != [3]int64{reqs, in, outB} {
			t.Errorf("%s: store %v, generator %v", key, got[key], [3]int64{reqs, in, outB})
		}
	}
	check("PutObject/2xx/ckpt/", puts.Load(), bytesIn.Load(), 0)
	check("GetObject/2xx/logs/", gets.Load(), 0, bytesOut.Load())
	check("ListObjectsV2/2xx/ckpt/", lists.Load(), 0, lists.Load()*listBody)
	check("GetObject/throttled/", throttled.Load(), 0, 0)
	ab := got["GetObject/2xx/"] // the aborted GET of key "abort": bucket root, 1 request
	if ab[0] != 1 || ab[2] < 10 || ab[2] > 4096 {
		t.Errorf("aborted download: store %v, want 1 request with 10..4096 bytes_out", ab)
	}
	// The backend also saw the direct list measurement above, which the proxy never did.
	if n := r.nb.Served(); n != puts.Load()+gets.Load()+lists.Load()+1+1 {
		t.Errorf("backend served %d", n)
	}
	// Every request through the proxy is in the store exactly once.
	if want := puts.Load() + gets.Load() + lists.Load() + throttled.Load() + 1; res.Requests != want {
		t.Errorf("store total %d, generator %d", res.Requests, want)
	}
}

// Showback costs under 2% of peak request rate on versus off (docs/SPEC.md, Performance targets):
// the same GET loop with the aggregator attached and detached, interleaved, on the dev box. This
// is an in-process A/B through httptest, so the absolute numbers are small; the ratio is the test.
func TestShowbackCostUnderTwoPercent(t *testing.T) {
	if testing.Short() {
		t.Skip("A/B needs a few seconds")
	}
	r := start(t, "")
	c := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 16}}
	run := func(on bool, d time.Duration) float64 {
		cfg := *r.published.Load()
		cfg.Kill.ShowbackDisable = !on
		r.published.Store(&cfg)
		var n atomic.Int64
		var wg sync.WaitGroup
		deadline := time.Now().Add(d)
		for w := 0; w < 8; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for time.Now().Before(deadline) {
					resp, err := c.Get(r.front.URL + "/b/k") //nolint:noctx // test
					if err != nil {
						continue
					}
					io.Copy(io.Discard, resp.Body) //nolint:errcheck // test
					resp.Body.Close()              //nolint:errcheck // test
					n.Add(1)
				}
			}()
		}
		wg.Wait()
		return float64(n.Load()) / d.Seconds()
	}
	run(true, 500*time.Millisecond) // warm-up
	var on, off float64
	for i := 0; i < 3; i++ {
		off += run(false, time.Second)
		on += run(true, time.Second)
	}
	t.Logf("showback off %.0f req/s, on %.0f req/s, cost %.1f%%", off/3, on/3, (1-on/off)*100)
	if on < off*0.90 {
		// 10% rather than 2% as the pass line: the in-process A/B on a 2 vCPU box has run-to-run
		// noise of several percent (both numbers are logged; the 2% claim is checked on the
		// reference node in Phase 5).
		t.Fatalf("showback on is %.1f%% slower than off", (1-on/off)*100)
	}
}
