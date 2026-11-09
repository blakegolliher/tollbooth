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

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/blakegolliher/tollbooth/internal/admin"
)

// runStatus is `tollbooth status`: the one-screen summary from GET /admin/status.
func runStatus(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	af := &adminFlags{}
	af.bind(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := statusCmd(af, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "tollbooth status: %v\n", err)
		return 1
	}
	return 0
}

func statusCmd(af *adminFlags, stdout *os.File) error {
	c, err := af.client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	var v admin.StatusView
	raw, err := c.Do(cx, http.MethodGet, "/admin/status", nil, &v)
	if err != nil {
		return err
	}
	g := &grid{header: []string{"FIELD", "VALUE"}}
	ready := "not ready"
	if v.Ready {
		ready = "ready"
	}
	last := "none"
	if v.LastReload != nil {
		if v.LastReload.OK {
			last = fmt.Sprintf("ok at %s (%d rules, %.1f ms)", v.LastReload.At.UTC().Format(time.RFC3339), v.LastReload.Rules, v.LastReload.DurationMS)
		} else {
			last = fmt.Sprintf("FAILED at %s: %s", v.LastReload.At.UTC().Format(time.RFC3339), v.LastReload.Error)
		}
	}
	g.add("node_id", v.NodeID)
	g.add("version", v.Version)
	g.add("uptime", (time.Duration(v.UptimeSeconds * float64(time.Second))).Truncate(time.Second).String())
	g.add("state", ready)
	g.add("in_flight", strconv.FormatInt(v.Inflight, 10))
	g.add("requests", fmt.Sprintf("%d total; %.1f/s (1s) %.1f/s (10s) %.1f/s (60s)", v.Requests, v.Rates.Requests1s, v.Rates.Requests10s, v.Rates.Requests60s))
	g.add("throttled", fmt.Sprintf("%d total; %.1f/s (1s) %.1f/s (10s) %.1f/s (60s)", v.Throttled, v.Rates.Throttled1s, v.Rates.Throttled10s, v.Rates.Throttled60s))
	g.add("rules", fmt.Sprintf("%d in force; %d live keys; %d denied since start", v.Rules, v.LiveKeys, v.Denied))
	g.add("showback", fmt.Sprintf("hour %s; %d tuples", v.ShowbackHour, v.Tuples))
	g.add("config", fmt.Sprintf("generation %d, loaded %s", v.Generation, v.LoadedAt.UTC().Format(time.RFC3339)))
	g.add("last_reload", last)
	g.add("backends", fmt.Sprintf("%d of %d up", v.BackendsUp, len(v.Backends)))
	for i := range v.Backends {
		b := &v.Backends[i]
		state := "down"
		if b.Up {
			state = "up"
		}
		g.add("  "+b.Endpoint, fmt.Sprintf("%s; %d in flight; %d requests; %d errors; ttfb mean %.1f ms", state, b.Inflight, b.Requests, b.Errors, b.TTFBMeanMS))
	}
	return render(stdout, af.format, g, raw)
}

// runBackends is `tollbooth backends list`: GET /admin/backends.
func runBackends(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, _ = fmt.Fprint(stderr, "usage: tollbooth backends list [--admin --token --config --format]\n")
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	if args[0] != "list" {
		_, _ = fmt.Fprintf(stderr, "tollbooth backends: unknown verb %q (only list)\n", args[0])
		return 2
	}
	fs := flag.NewFlagSet("backends list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	af := &adminFlags{}
	af.bind(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if err := backendsCmd(af, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "tollbooth backends list: %v\n", err)
		return 1
	}
	return 0
}

func backendsCmd(af *adminFlags, stdout *os.File) error {
	c, err := af.client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	var out struct {
		Backends []admin.BackendStatus `json:"backends"`
	}
	raw, err := c.Do(cx, http.MethodGet, "/admin/backends", nil, &out)
	if err != nil {
		return err
	}
	g := &grid{header: []string{"ENDPOINT", "STATE", "IN_FLIGHT", "REQUESTS", "ERRORS", "DIAL", "TLS", "TIMEOUT", "RESET", "OTHER", "TTFB_MEAN_MS", "TTFB_LAST_MS", "LAST_ERROR"}}
	for i := range out.Backends {
		b := &out.Backends[i]
		state := "down"
		if b.Up {
			state = "up"
		}
		last := "-"
		if b.LastError != "" {
			last = b.LastErrAt + " " + b.LastError
		}
		g.add(b.Endpoint, state, strconv.FormatInt(b.Inflight, 10), strconv.FormatInt(b.Requests, 10), strconv.FormatInt(b.Errors, 10),
			strconv.FormatInt(b.ErrorsDial, 10), strconv.FormatInt(b.ErrorsTLS, 10), strconv.FormatInt(b.ErrorsTime, 10), strconv.FormatInt(b.ErrorsRst, 10), strconv.FormatInt(b.ErrorsOth, 10),
			strconv.FormatFloat(b.TTFBMeanMS, 'f', 1, 64), strconv.FormatFloat(b.TTFBLastMS, 'f', 1, 64), strings.TrimSpace(last))
	}
	return render(stdout, af.format, g, raw)
}
