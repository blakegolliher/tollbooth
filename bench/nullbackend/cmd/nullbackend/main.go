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

// Command nullbackend serves the in-repo null backend as a process, for bench/run.sh and
// bench/soak.sh: every S3 operation gets a canned answer, GetObject bodies are -size bytes, -latency
// is added to every response. HTTP/1.1 only, like the proxy's listener. A second listener,
// -control, takes fault injections for the soak: GET /chaos?fail=503, ?stall=1 (until the client
// gives up), ?stall_for=20s (bounded), ?reset=1, ?reset_after=N, ?clear=1 (any combination; clear
// resets everything), and GET /served.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/blakegolliher/tollbooth/bench/nullbackend"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:19000", "address to serve on")
	control := flag.String("control", "", "address for the fault-injection control listener (empty: none)")
	size := flag.Int("size", 4096, "GetObject body size in bytes")
	latency := flag.Duration("latency", 0, "added to every response")
	flag.Parse()
	nb := nullbackend.New(*size)
	nb.Latency = *latency
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	if *control != "" {
		cln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", *control)
		if err != nil {
			log.Fatal(err)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /chaos", func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("clear") != "" {
				nb.Fail.Store(0)
				nb.Stall.Store(false)
				nb.StallFor.Store(0)
				nb.Reset.Store(false)
				nb.ResetAfter.Store(0)
			}
			if v := q.Get("stall_for"); v != "" {
				d, err := time.ParseDuration(v)
				if err != nil || d < 0 {
					http.Error(w, "stall_for must be a duration", http.StatusBadRequest)
					return
				}
				nb.StallFor.Store(int64(d))
			}
			if v := q.Get("fail"); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 0 || n > 599 {
					http.Error(w, "fail must be an HTTP status", http.StatusBadRequest)
					return
				}
				nb.Fail.Store(int32(n)) //nolint:gosec // bounded above
			}
			if v := q.Get("stall"); v != "" {
				nb.Stall.Store(v == "1" || v == "true")
			}
			if v := q.Get("reset"); v != "" {
				nb.Reset.Store(v == "1" || v == "true")
			}
			if v := q.Get("reset_after"); v != "" {
				n, err := strconv.ParseInt(v, 10, 64)
				if err != nil || n < 0 {
					http.Error(w, "reset_after must be a byte count", http.StatusBadRequest)
					return
				}
				nb.ResetAfter.Store(n)
			}
			_, _ = fmt.Fprintf(w, "fail=%d stall=%v stall_for=%s reset=%v reset_after=%d served=%d\n", nb.Fail.Load(), nb.Stall.Load(), time.Duration(nb.StallFor.Load()), nb.Reset.Load(), nb.ResetAfter.Load(), nb.Served())
		})
		mux.HandleFunc("GET /served", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, "%d\n", nb.Served())
		})
		csrv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { log.Fatal(csrv.Serve(cln)) }()
		_, _ = fmt.Fprintf(os.Stdout, "nullbackend control on %s\n", cln.Addr())
	}
	srv := &http.Server{Handler: nb, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	_, _ = fmt.Fprintf(os.Stdout, "nullbackend listening on %s (body %d bytes, latency %s)\n", ln.Addr(), *size, *latency)
	log.Fatal(srv.Serve(ln))
}
