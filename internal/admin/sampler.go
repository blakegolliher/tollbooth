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

package admin

import (
	"context"
	"sync"
	"time"
)

// sampler takes the two plain request counters once a second into a 60-slot ring so
// GET /admin/status can report req/s and throttled/s over 1, 10, and 60 seconds without the
// data path doing any of the arithmetic.
type sampler struct {
	mu      sync.Mutex
	ring    [61]sample // 61 samples give 60 one-second deltas
	n       int        // samples taken
	started bool
}

type sample struct {
	at                  time.Time
	requests, throttled int64
}

// Run samples until ctx ends. Call once; the first sample is taken immediately.
func (s *Server) Run(ctx context.Context) {
	s.sampler.mu.Lock()
	if s.sampler.started || s.Metrics == nil {
		s.sampler.mu.Unlock()
		return
	}
	s.sampler.started = true
	s.sampler.mu.Unlock()
	s.sampler.take(s.Metrics)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sampler.take(s.Metrics)
		}
	}
}

func (sp *sampler) take(m *Metrics) {
	sp.mu.Lock()
	sp.ring[sp.n%len(sp.ring)] = sample{at: time.Now(), requests: m.TotalRequests.Load(), throttled: m.TotalThrottled.Load()}
	sp.n++
	sp.mu.Unlock()
}

// rates returns the per-second rates over the last 1, 10, and 60 samples that exist.
func (sp *sampler) rates() Rates {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	var r Rates
	if sp.n < 2 {
		return r
	}
	latest := sp.ring[(sp.n-1)%len(sp.ring)]
	over := func(back int) (reqs, thr float64) {
		if back > sp.n-1 {
			back = sp.n - 1
		}
		if back > len(sp.ring)-1 {
			back = len(sp.ring) - 1
		}
		old := sp.ring[(sp.n-1-back)%len(sp.ring)]
		secs := latest.at.Sub(old.at).Seconds()
		if secs <= 0 {
			return 0, 0
		}
		return float64(latest.requests-old.requests) / secs, float64(latest.throttled-old.throttled) / secs
	}
	r.Requests1s, r.Throttled1s = over(1)
	r.Requests10s, r.Throttled10s = over(10)
	r.Requests60s, r.Throttled60s = over(60)
	return r
}
