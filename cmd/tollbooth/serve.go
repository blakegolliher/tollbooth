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
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/blakegolliher/tollbooth/internal/admin"
	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/limiter"
	"github.com/blakegolliher/tollbooth/internal/proxy"
)

// runServe runs the proxy until SIGTERM or SIGINT: load the config, open the listener, build the
// backend pool and the handler, start the admin server, and reload on SIGHUP. Exit 0 after a
// clean drain, 1 on a startup failure.
func runServe(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "tollbooth.yaml", "path to the YAML config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*path)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "tollbooth serve: %v\n", err)
		return 1
	}
	log := newLogger(stdout, cfg.Logging.Level)
	for _, w := range cfg.Warnings(goMemLimit()) {
		log.Warn("config warning", "msg", w)
	}
	if err := serve(context.Background(), *path, cfg, log, stdout); err != nil {
		log.Error("serve failed", "err", err.Error())
		return 1
	}
	return 0
}

func newLogger(w *os.File, level string) *slog.Logger {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(level)) // validated by config; info on any surprise
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl}))
}

// serve is the process body, separated from flag parsing so a test can drive it with a context.
func serve(ctx context.Context, path string, cfg *config.Config, log *slog.Logger, stdout *os.File) error {
	published := &atomic.Pointer[config.Config]{}
	published.Store(cfg)
	metrics := admin.NewMetrics()

	ln, certs, err := proxy.Listen(&cfg.Listen)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
	pool, err := proxy.NewPool(&cfg.Backend, metrics, log)
	if err != nil {
		return err
	}
	pool.Start()
	defer pool.Stop()

	lim := limiter.New(&metrics.Limiter)
	if err = lim.Apply(cfg); err != nil {
		return fmt.Errorf("rules: %w", err)
	}
	sweepCtx, stopSweep := context.WithCancel(ctx)
	defer stopSweep()
	go lim.Run(sweepCtx)

	var access *proxy.AccessLogger
	if cfg.Logging.Access {
		access = proxy.NewAccessLogger(stdout)
	}
	h := proxy.New(published, pool, metrics, lim, access, log)
	data := proxy.NewServer(h, &cfg.Listen, metrics, log)
	data.BaseContext = func(net.Listener) context.Context { return ctx }

	rl := &reloader{path: path, published: published, certs: certs, lim: lim, metrics: metrics, log: log, generation: 1, loadedAt: time.Now()}
	adm := &admin.Server{Address: cfg.Admin.Address, Metrics: metrics, Ready: pool.Healthy, Token: cfg.AdminToken(), Pprof: cfg.Admin.Pprof,
		Limiter: lim, Reload: rl.reload, Config: rl.view}
	admSrv := adm.HTTPServer()
	admLn, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Admin.Address)
	if err != nil {
		return fmt.Errorf("admin listener: %w", err)
	}

	scheme := "http"
	if certs != nil {
		scheme = "https"
	}
	log.Info("tollbooth starting", "version", version, "commit", commit, "config", path, "node_id", cfg.NodeID,
		"listen", ln.Addr().String(), "scheme", scheme, "proxy_protocol", cfg.Listen.ProxyProtocol,
		"admin", admLn.Addr().String(), "backend_scheme", cfg.Backend.Scheme, "backends", cfg.Backend.Endpoints,
		"rules", len(cfg.RateLimits.Rules), "base_domains", cfg.BaseDomains)

	errc := make(chan error, 2)
	go func() { errc <- data.Serve(ln) }()
	go func() { errc <- admSrv.Serve(admLn) }()

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)

	for {
		select {
		case <-ctx.Done():
			return drain(data, admSrv, published.Load().Proxy.DrainTimeout, log)
		case err := <-errc:
			if errors.Is(err, http.ErrServerClosed) {
				continue
			}
			return err
		case s := <-sigs:
			switch s {
			case syscall.SIGHUP:
				rl.reload()
			default:
				log.Info("signal received, draining", "signal", s.String())
				return drain(data, admSrv, published.Load().Proxy.DrainTimeout, log)
			}
		}
	}
}

// reloader is the one reload path, shared by SIGHUP and POST /admin/reload and serialized by its
// mutex. It keeps the state GET /admin/config shows.
type reloader struct {
	path      string
	published *atomic.Pointer[config.Config]
	certs     *proxy.CertStore
	lim       *limiter.Limiter
	metrics   *admin.Metrics
	log       *slog.Logger

	mu         sync.Mutex
	generation int
	loadedAt   time.Time
	last       *admin.ReloadResult
}

// reload loads the file again, refuses a change to a restart-only key, re-reads the TLS pair,
// compiles and publishes the rule set, then swaps the config pointer. An invalid file keeps the
// running config and is logged and counted (docs/CONFIG.md). The limiter's store is untouched.
func (r *reloader) reload() admin.ReloadResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	start := time.Now()
	next, err := config.Load(r.path)
	if err == nil {
		err = next.ReloadCompatible(r.published.Load())
	}
	if err == nil && r.certs != nil {
		err = r.certs.Load(next.Listen.TLS.Cert, next.Listen.TLS.Key)
	}
	if err == nil {
		err = r.lim.Apply(next) // D-036: the rule set first, so a compile failure publishes nothing
	}
	res := admin.ReloadResult{At: start, Generation: r.generation}
	if err != nil {
		res.Error = err.Error()
		res.DurationMS = float64(time.Since(start)) / 1e6
		r.last = &res
		r.metrics.ConfigReload.WithLabelValues("error").Inc()
		r.log.Error("config reload failed; running config kept", "config", r.path, "generation", r.generation, "err", err.Error())
		return res
	}
	r.published.Store(next)
	r.generation++
	r.loadedAt = start
	res.OK, res.Generation, res.Rules = true, r.generation, len(next.RateLimits.Rules)
	res.DurationMS = float64(time.Since(start)) / 1e6
	r.last = &res
	r.metrics.ConfigReload.WithLabelValues("ok").Inc()
	r.log.Info("config reloaded", "config", r.path, "generation", r.generation, "rules", res.Rules, "duration_ms", res.DurationMS)
	return res
}

// view is GET /admin/config: the running config, redacted, with the reload state.
func (r *reloader) view() admin.ConfigView {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := admin.ConfigView{Config: r.published.Load().Redacted(), Generation: r.generation, LoadedAt: r.loadedAt}
	if r.last != nil {
		last := *r.last
		v.LastReload = &last
	}
	return v
}

// drain stops accepting, waits for in-flight requests up to the drain timeout, then stops the
// admin server. Phase 3 adds the showback flush here.
func drain(data, adm *http.Server, timeout time.Duration, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := data.Shutdown(ctx)
	if err != nil {
		log.Warn("drain timed out; closing remaining connections", "err", err.Error())
		_ = data.Close()
	}
	_ = adm.Shutdown(ctx)
	log.Info("tollbooth stopped")
	return nil
}
