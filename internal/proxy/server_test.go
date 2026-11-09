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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/blakegolliher/tollbooth/bench/nullbackend"
)

// Chaos rows of the failure table that the server settings answer (test plan category 9):
// slowloris is cut off by ReadHeaderTimeout, and a header block over MaxHeaderBytes is refused
// before the handler runs. Neither reaches the backend.

func TestSlowlorisIsCutOffByReadHeaderTimeout(t *testing.T) {
	// ReadHeaderTimeout is a server setting fixed at start, so the rig is built with it.
	nb := nullbackend.New(16)
	nb.Record = true
	be := httptest.NewServer(nb)
	t.Cleanup(be.Close)
	c := testConfig(t, []string{strings.TrimPrefix(be.URL, "http://")}, time.Second)
	c.Listen.ReadHeaderTimeout = 300 * time.Millisecond
	r := (&rig{log: &syncBuf{}, null: nb}).start(t, c)
	conn := rawDial(t, r.front)
	start := time.Now()
	fmt.Fprintf(conn, "GET /b/k HTTP/1.1\r\nHost: h\r\nX-Slow: ") // never finishes the header block
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := io.ReadAll(conn)
	if err != nil && !strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "reset") {
		// A clean close from the server is a nil error with zero bytes, or EOF/reset.
		t.Logf("read ended with: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("slowloris connection stayed open %v", d)
	}
	if n := r.null.Served(); n != 0 {
		t.Fatalf("backend served %d requests from a request that never completed its headers", n)
	}
}

func TestHugeHeadersAreRefused(t *testing.T) {
	r := newRig(t, 1, time.Second)
	// The rig's server carries listen.max_header_bytes (1 MiB default). Send 1.5 MiB of headers.
	conn := rawDial(t, r.front)
	fmt.Fprintf(conn, "GET /b/k HTTP/1.1\r\nHost: h\r\n")
	chunk := strings.Repeat("x", 8000)
	for i := 0; i < 200; i++ { // 200 × ~8 KiB > 1 MiB
		if _, err := fmt.Fprintf(conn, "X-Pad-%d: %s\r\n", i, chunk); err != nil {
			break // the server may close early; that is the point
		}
	}
	fmt.Fprintf(conn, "\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err == nil {
		resp.Body.Close() //nolint:errcheck // test
		if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
			t.Fatalf("status %d for a 1.5 MiB header block", resp.StatusCode)
		}
	}
	if n := r.null.Served(); n != 0 {
		t.Fatalf("backend served %d requests with oversized headers", n)
	}
	// A header block just under the cap goes through.
	conn2 := rawDial(t, r.front)
	fmt.Fprintf(conn2, "GET /b/k HTTP/1.1\r\nHost: h\r\nX-Pad: %s\r\nConnection: close\r\n\r\n", strings.Repeat("y", 900<<10))
	resp2, err := http.ReadResponse(bufio.NewReader(conn2), nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body) //nolint:errcheck // test
	resp2.Body.Close()              //nolint:errcheck // test
	if resp2.StatusCode != 200 {
		t.Fatalf("900 KiB header block: status %d", resp2.StatusCode)
	}
}

// The in-flight cap counts correctly under concurrency: with cap 4 and 8 stalled requests, four
// are refused with 503 SlowDown before touching the backend and the gauge never exceeds the cap.
func TestInflightCapUnderConcurrency(t *testing.T) {
	r := newRig(t, 1, 5*time.Second)
	c := *r.cfg.Load()
	c.Listen.MaxInflight = 4
	r.cfg.Store(&c)
	r.null.Stall.Store(true)
	results := make(chan int, 8)
	// The four requests that get a slot stall on the backend; a client timeout ends them, which
	// cancels the proxy's backend requests, so the test does not wait for response_header_timeout.
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	for i := 0; i < 8; i++ {
		go func() {
			resp, err := client.Get(r.front.URL + "/b/k") //nolint:noctx // test
			if err != nil {
				results <- -1
				return
			}
			resp.Body.Close() //nolint:errcheck // test
			results <- resp.StatusCode
		}()
	}
	refused, timedOut := 0, 0
	deadline := time.After(10 * time.Second)
	for refused+timedOut < 8 {
		select {
		case code := <-results:
			switch code {
			case http.StatusServiceUnavailable:
				refused++
				if refused == 4 {
					if n := r.h.inflight.Load(); n != 4 {
						t.Fatalf("in-flight gauge %d with four stalled requests, want 4", n)
					}
				}
			case -1:
				timedOut++
			default:
				t.Fatalf("unexpected status %d while the backend stalls", code)
			}
		case <-deadline:
			t.Fatalf("refused=%d timedOut=%d after 10 s", refused, timedOut)
		}
	}
	if refused != 4 || timedOut != 4 {
		t.Fatalf("refused=%d timedOut=%d, want 4 and 4", refused, timedOut)
	}
	r.null.Stall.Store(false)
	deadline = time.After(5 * time.Second)
	for r.h.inflight.Load() != 0 {
		select {
		case <-deadline:
			t.Fatalf("in-flight gauge stuck at %d after the clients left", r.h.inflight.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
