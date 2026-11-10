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
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/blakegolliher/tollbooth/internal/config"
)

// check is one doctor finding: ok, warn, or fail.
type check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail"`
}

// runDoctor is `tollbooth doctor`: environment checks against a config file (docs/SPEC.md,
// Operator surface): ulimits, GOMAXPROCS and GOMEMLIMIT, TLS material, backend TCP and HTTP
// reachability, clock sync, disk headroom and filesystem of the showback directory. It does not
// guess whether a backend accepts the base domains; that is a configuration fact. Exit 1 on any
// fail, 0 otherwise (warnings included).
func runDoctor(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "tollbooth.yaml", "config file to check the environment against")
	format := fs.String("format", "table", "output format: table, json, or csv")
	timeout := fs.Duration("timeout", 3*time.Second, "per-backend connect and HEAD timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *format != "table" && *format != "json" && *format != "csv" {
		_, _ = fmt.Fprintf(stderr, "tollbooth doctor: --format must be table, json, or csv, got %q\n", *format)
		return 1
	}
	checks := doctor(*cfgPath, *timeout)
	fails := 0
	g := &grid{header: []string{"CHECK", "STATUS", "DETAIL"}}
	for _, c := range checks {
		g.add(c.Name, c.Status, c.Detail)
		if c.Status == "fail" {
			fails++
		}
	}
	raw, _ := json.Marshal(map[string]any{"config": *cfgPath, "checks": checks, "fails": fails})
	if err := render(stdout, *format, g, raw); err != nil {
		_, _ = fmt.Fprintf(stderr, "tollbooth doctor: %v\n", err)
		return 1
	}
	if *format == "table" {
		_, _ = fmt.Fprintf(stdout, "\n%d checks, %d fail\n", len(checks), fails)
	}
	if fails > 0 {
		return 1
	}
	return 0
}

func doctor(cfgPath string, timeout time.Duration) []check {
	var out []check
	add := func(name, status, format string, args ...any) {
		out = append(out, check{Name: name, Status: status, Detail: fmt.Sprintf(format, args...)})
	}

	// Config first: every other check reads it.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		add("config", "fail", "%s: %v", cfgPath, firstLine(err.Error()))
		return out
	}
	warnings := cfg.Warnings(goMemLimit())
	add("config", "ok", "%s loads: %d rules, %d backend endpoints, %d warnings", cfgPath, len(cfg.Limits), len(cfg.Backend.Endpoints), len(warnings))
	for _, w := range warnings {
		add("config warning", "warn", "%s", w)
	}

	// File descriptors: every client and backend connection is one; the in-flight cap bounds the
	// need at roughly 2 × max_inflight plus idle backend connections.
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err == nil {
		need := 2*cfg.Listen.MaxInflight + cfg.Backend.MaxIdleConnsPerHost*len(cfg.Backend.Endpoints) + 64
		status := "ok"
		if rl.Cur < uint64(need) { //nolint:gosec // need is a small positive int from validated config
			status = "warn"
		}
		add("ulimit nofile", status, "soft %d, hard %d; about %d needed at max_inflight %d", rl.Cur, rl.Max, need, cfg.Listen.MaxInflight)
	}

	// Scheduler and memory limits.
	add("GOMAXPROCS", "ok", "%d (NumCPU %d)", runtime.GOMAXPROCS(0), runtime.NumCPU())
	if lim := debug.SetMemoryLimit(-1); lim == 1<<63-1 {
		add("GOMEMLIMIT", "warn", "unset: the specification's memory target assumes GOMEMLIMIT=1GiB; without it the heap grows to GOGC's discretion")
	} else {
		add("GOMEMLIMIT", "ok", "%d MiB", lim>>20)
	}

	// TLS material.
	if cfg.Listen.TLS.Cert != "" {
		if crt, err := tls.LoadX509KeyPair(cfg.Listen.TLS.Cert, cfg.Listen.TLS.Key); err != nil {
			add("tls", "fail", "%v", err)
		} else if len(crt.Certificate) > 0 {
			if leaf, err := x509.ParseCertificate(crt.Certificate[0]); err == nil {
				left := time.Until(leaf.NotAfter)
				status := "ok"
				if left < 14*24*time.Hour {
					status = "warn"
				}
				add("tls", status, "pair loads; expires %s (%s left)", leaf.NotAfter.UTC().Format(time.RFC3339), left.Truncate(time.Hour))
			} else {
				add("tls", "ok", "pair loads")
			}
		}
	} else {
		add("tls", "ok", "plaintext listener (no listen.tls)")
	}

	// Backends: TCP then HEAD /, any HTTP status means reachable (403 included).
	for _, ep := range cfg.Backend.Endpoints {
		d := net.Dialer{Timeout: timeout}
		t0 := time.Now()
		conn, err := d.DialContext(context.Background(), "tcp", ep)
		if err != nil {
			add("backend "+ep, "fail", "tcp: %v", err)
			continue
		}
		_ = conn.Close()
		dial := time.Since(t0)
		client := &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.Backend.TLS.InsecureSkipVerify}}} //nolint:gosec // the operator's own setting
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodHead, cfg.Backend.Scheme+"://"+ep+"/", http.NoBody)
		t1 := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			add("backend "+ep, "fail", "tcp ok in %s; HEAD /: %v", dial.Round(time.Millisecond), err)
			continue
		}
		_ = resp.Body.Close()
		add("backend "+ep, "ok", "tcp %s, HEAD / %d in %s", dial.Round(time.Millisecond), resp.StatusCode, time.Since(t1).Round(time.Millisecond))
	}

	// Clock: the kernel's own view of synchronization (adjtimex STA_UNSYNC); showback hours and
	// SigV4 signatures both depend on it.
	var tx syscall.Timex
	if state, err := syscall.Adjtimex(&tx); err == nil {
		const staUnsync = 0x0040
		if tx.Status&staUnsync != 0 || state == 5 /* TIME_ERROR */ {
			add("clock", "warn", "kernel reports the clock unsynchronized (adjtimex status 0x%x); showback hours and request signatures depend on it", tx.Status)
		} else {
			add("clock", "ok", "kernel reports the clock synchronized (max error %d µs)", tx.Maxerror)
		}
	}

	// Showback directory: exists or can be created, writable, headroom, not tmpfs.
	dir := cfg.Showback.Dir
	if err := os.MkdirAll(dir, 0o750); err != nil {
		add("showback.dir", "fail", "%s: %v", dir, err)
	} else {
		probe := dir + "/.doctor-probe"
		if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
			add("showback.dir", "fail", "%s not writable: %v", dir, err)
		} else {
			_ = os.Remove(probe)
			var st syscall.Statfs_t
			if err := syscall.Statfs(dir, &st); err == nil {
				free := uint64(st.Bavail) * uint64(st.Bsize) //nolint:gosec,unconvert // kernel values, non-negative; the field types differ across architectures
				// Sizing: a full hour at the tuple cap is about 50 MiB per node; 400 days of them is the default retention.
				status := "ok"
				if free < 2<<30 {
					status = "warn"
				}
				detail := fmt.Sprintf("%s writable; %.1f GiB free", dir, float64(free)/(1<<30))
				const tmpfsMagic, ramfsMagic = 0x01021994, 0x858458f6
				if uint64(st.Type) == tmpfsMagic || uint64(st.Type) == ramfsMagic { //nolint:gosec,unconvert // kernel value
					status = "warn"
					detail += "; on tmpfs: showback files do not survive a reboot, use a persistent path"
				}
				add("showback.dir", status, "%s", detail)
			} else {
				add("showback.dir", "ok", "%s writable", dir)
			}
		}
	}

	// Admin listener reachable (the proxy may or may not be running; a connect failure is a note).
	if conn, err := (&net.Dialer{Timeout: timeout}).DialContext(context.Background(), "tcp", cfg.Admin.Address); err == nil {
		_ = conn.Close()
		add("admin", "ok", "%s accepts connections (tollbooth is running)", cfg.Admin.Address)
	} else {
		add("admin", "warn", "%s: %v (tollbooth not running, or bound elsewhere)", cfg.Admin.Address, errString(err))
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func errString(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		return s[i+2:]
	}
	return s
}
