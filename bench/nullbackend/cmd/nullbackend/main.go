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

// Command nullbackend serves the in-repo null backend as a process, for bench/run.sh: every S3
// operation gets a canned answer, GetObject bodies are -size bytes, -latency is added to every
// response. HTTP/1.1 only, like the proxy's listener.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/blakegolliher/tollbooth/bench/nullbackend"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:19000", "address to serve on")
	size := flag.Int("size", 4096, "GetObject body size in bytes")
	latency := flag.Duration("latency", 0, "added to every response")
	flag.Parse()
	nb := nullbackend.New(*size)
	nb.Latency = *latency
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Handler: nb, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	_, _ = fmt.Fprintf(os.Stdout, "nullbackend listening on %s (body %d bytes, latency %s)\n", ln.Addr(), *size, *latency)
	log.Fatal(srv.Serve(ln))
}
