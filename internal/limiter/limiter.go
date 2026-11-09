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

package limiter

import (
	"context"
	"hash/maphash"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/s3id"
)

// numShards is the key store's shard count (docs/DESIGN.md §5.2).
const numShards = 256

// Metrics are the catalog metrics the limiter owns (docs/telemetry-catalog.md, Phase 2 rows).
// internal/admin registers them; the limiter only observes. Any field may be nil in tests.
type Metrics struct {
	Decision   prometheus.Observer    // tollbooth_limiter_decision_seconds
	Keys       prometheus.Gauge       // tollbooth_limiter_keys
	Overflow   prometheus.Counter     // tollbooth_limiter_keys_overflow_total
	Denied     *prometheus.CounterVec // tollbooth_limiter_denied_total{rule}
	Errors     prometheus.Counter     // tollbooth_limiter_errors_total
	ShapedWait *prometheus.HistogramVec
}

// Limiter is the rule set in force and the GCRA key store (docs/DESIGN.md §5). One per process.
// Apply publishes a new rule set with one pointer store; the store is never reset.
type Limiter struct {
	rules         atomic.Pointer[RuleSet]
	maxKeys       atomic.Int64
	sweepInterval atomic.Int64 // nanoseconds
	live          atomic.Int64
	seed          maphash.Seed
	epoch         time.Time
	m             *Metrics

	statsMu sync.Mutex
	stats   map[string]*ruleStats // by rule id; survives reloads that keep the id

	shards [numShards]shard
}

// shard is one lock and one map; padded to a cache line so neighbors do not false-share.
type shard struct {
	mu sync.Mutex
	m  map[key]int64 // theoretical arrival time, monotonic nanoseconds since epoch (D-007)
	_  [40]byte
}

// ruleStats are the per-rule counters the admin API shows and the deny ring behind
// /admin/limits/top. They belong to the rule id, not to one compiled RuleSet.
type ruleStats struct {
	live   atomic.Int64 // keys of this rule in the store
	denied atomic.Int64 // denials since the id first appeared
	ring   ring
}

// New returns an empty limiter: no rules, every request allowed. m may be nil.
func New(m *Metrics) *Limiter {
	l := &Limiter{seed: maphash.MakeSeed(), epoch: time.Now(), m: m, stats: map[string]*ruleStats{}}
	if m == nil {
		l.m = &Metrics{}
	}
	for i := range l.shards {
		l.shards[i].m = make(map[key]int64)
	}
	l.maxKeys.Store(config.DefaultMaxKeys)
	l.sweepInterval.Store(int64(config.DefaultSweepInterval))
	return l
}

// Now converts a wall-clock reading taken by the caller into the limiter's monotonic
// nanoseconds, so the handler's one time.Now() serves the limiter too. Tests pass numbers.
func (l *Limiter) Now(t time.Time) int64 { return int64(t.Sub(l.epoch)) }

// Apply compiles cfg and publishes it. State is keyed by rule id and never reset: a changed rate
// or burst applies from the next decision, and the sweeper drops the keys of removed rules. Per-rule
// counters follow the id. Callers serialize reloads.
func (l *Limiter) Apply(cfg *config.Config) error {
	rs, err := compile(cfg)
	if err != nil {
		return err
	}
	l.statsMu.Lock()
	for _, r := range rs.rules {
		st := l.stats[r.ID]
		if st == nil {
			st = &ruleStats{}
			l.stats[r.ID] = st
		}
		r.stats = st
		if l.m.Denied != nil {
			r.denied = l.m.Denied.WithLabelValues(r.ID)
		}
	}
	for id := range l.stats {
		if _, keep := rs.byID[id]; !keep {
			delete(l.stats, id)
			if l.m.Denied != nil {
				l.m.Denied.DeleteLabelValues(id)
			}
		}
	}
	l.statsMu.Unlock()
	l.maxKeys.Store(int64(cfg.Limiter.MaxKeys))
	l.sweepInterval.Store(int64(cfg.Limiter.SweepInterval))
	l.rules.Store(rs)
	return nil
}

// Decision is the outcome of one pass over the matching rules.
type Decision struct {
	Allowed    bool
	Mode       Mode  // the denying rule's response mode
	RetryAfter int   // seconds, ≥ 1 when denied: ceil(Wait), for Retry-After
	Wait       int64 // nanoseconds until the denying rule would admit one request, for x-amz-retry-after
	Rule       *Rule // the denying rule; nil when allowed
	Matched    int   // rules that matched the request
	Overflow   bool  // a matching rule had no state because limiter.max_keys was reached
}

// Decide is the hot-path entry: one pass over the rules on the identity's class list in config
// order, consuming a token from each matching rule; the first denial ends the pass and decides
// the mode, and the tokens already taken stay taken (docs/SPEC.md, Rate limiting, Evaluation).
// With consume false it computes the same decision without storing (`rules test`, D-018). A
// panic inside fails open and counts in limiter_errors_total. Zero allocations at steady state.
func (l *Limiter) Decide(id *s3id.RequestIdentity, now int64, consume bool) (d Decision) {
	defer func() {
		if p := recover(); p != nil {
			d = Decision{Allowed: true}
			if l.m.Errors != nil {
				l.m.Errors.Inc()
			}
		}
	}()
	rs := l.rules.Load()
	if rs == nil || len(rs.rules) == 0 {
		return Decision{Allowed: true}
	}
	var t0 time.Time
	if l.m.Decision != nil {
		t0 = time.Now()
	}
	d = l.evaluate(rs, id, now, consume, nil)
	if l.m.Decision != nil {
		l.m.Decision.Observe(time.Since(t0).Seconds())
	}
	return d
}

// RuleMatch is one matching rule's view of a request, for `rules test`.
type RuleMatch struct {
	Rule      string `json:"rule"`
	AccessKey string `json:"access_key,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	SourceIP  string `json:"source_ip,omitempty"`
	LiveKeys  int64  `json:"live_keys"`
	Allowed   bool   `json:"allowed"`
	// RetryAfter is the seconds the rule would ask the client to wait, 0 when it allows.
	RetryAfter int    `json:"retry_after,omitempty"`
	Mode       string `json:"mode"`
}

// Explanation is the dry run `rules test` and POST /admin/limits/test return: every matching rule
// with the key it resolves to and its own verdict, and the decision the pass would reach now. No
// token is consumed.
type Explanation struct {
	Allowed    bool        `json:"allowed"`
	Mode       string      `json:"mode,omitempty"`
	RetryAfter int         `json:"retry_after,omitempty"`
	DeniedBy   string      `json:"denied_by,omitempty"`
	Matches    []RuleMatch `json:"matches"`
}

// Explain runs the pass with consume false and reports every matching rule, continuing past the
// first denial so the operator sees all of them (the real pass stops there).
func (l *Limiter) Explain(id *s3id.RequestIdentity, now int64) Explanation {
	rs := l.rules.Load()
	ex := Explanation{Allowed: true, Matches: []RuleMatch{}}
	if rs == nil {
		return ex
	}
	d := l.evaluate(rs, id, now, false, &ex.Matches)
	ex.Allowed = d.Allowed
	if !d.Allowed {
		ex.Mode = d.Mode.String()
		ex.RetryAfter = d.RetryAfter
		ex.DeniedBy = d.Rule.ID
	}
	return ex
}

// evaluate is the one pass. With explain non-nil it never consumes, records every match, and does
// not stop at the first denial.
func (l *Limiter) evaluate(rs *RuleSet, id *s3id.RequestIdentity, now int64, consume bool, explain *[]RuleMatch) Decision {
	d := Decision{Allowed: true}
	class := id.Class
	if int(class) >= s3id.NumClasses {
		class = s3id.ClassUnknown
	}
	if explain != nil {
		consume = false
	}
	for _, r := range rs.byClass[class] {
		prefix, ok := r.matches(id)
		if !ok {
			continue
		}
		d.Matched++
		k := r.key(id, prefix)
		var allow, overflow bool
		var wait int64
		if r.blockAll {
			allow, wait = false, 1e9
		} else {
			allow, wait, overflow = l.gcra(r, k, now, consume)
		}
		if overflow {
			d.Overflow = true
		}
		if explain != nil {
			m := RuleMatch{Rule: r.ID, AccessKey: k.ak, Bucket: k.bucket, Prefix: k.prefix, LiveKeys: r.stats.live.Load(), Allowed: allow, Mode: r.mode.String()}
			if k.ip.IsValid() {
				m.SourceIP = k.ip.String()
			}
			if !allow {
				m.RetryAfter = retryAfter(wait)
			}
			*explain = append(*explain, m)
		}
		if allow || !d.Allowed {
			continue
		}
		d.Allowed = false
		d.Rule = r
		d.Mode = r.mode
		d.Wait = wait
		d.RetryAfter = retryAfter(wait)
		if consume {
			r.stats.denied.Add(1)
			if r.denied != nil {
				r.denied.Inc()
			}
			r.stats.ring.add(k, now)
		}
		if explain == nil {
			break
		}
	}
	return d
}

// gcra is one rule's decision for one key (docs/DESIGN.md §5.3): lock the shard, look the key up
// (a miss is a full bucket), compare, store, unlock. A miss past limiter.max_keys is allowed
// without state and counted (fail open).
func (l *Limiter) gcra(r *Rule, k key, now int64, consume bool) (allow bool, wait int64, overflow bool) {
	s := &l.shards[maphash.Comparable(l.seed, k)&(numShards-1)]
	s.mu.Lock()
	tat, ok := s.m[k]
	if !ok {
		tat = now
	}
	if now >= tat-r.tau {
		if consume {
			next := max(tat, now) + r.T
			switch {
			case ok:
				s.m[k] = next
			case l.live.Load() < l.maxKeys.Load():
				s.m[k.clone()] = next
				n := l.live.Add(1)
				r.stats.live.Add(1)
				if l.m.Keys != nil {
					l.m.Keys.Set(float64(n))
				}
			default:
				overflow = true
			}
		}
		s.mu.Unlock()
		if overflow && l.m.Overflow != nil {
			l.m.Overflow.Inc()
		}
		return true, 0, overflow
	}
	wait = tat - r.tau - now
	s.mu.Unlock()
	return false, wait, false
}

// retryAfter is ceil(wait) in seconds, at least 1.
func retryAfter(wait int64) int {
	if wait <= 0 {
		return 1
	}
	s := (wait + 999_999_999) / 1_000_000_000
	if s < 1 {
		return 1
	}
	return int(s)
}

// Run sweeps the store every limiter.sweep_interval until ctx ends.
func (l *Limiter) Run(ctx context.Context) {
	for {
		t := time.NewTimer(time.Duration(l.sweepInterval.Load()))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			l.sweep(l.Now(time.Now()))
		}
	}
}

// sweep deletes every key whose TAT ≤ now (that bucket is full, so re-creating it on the next
// request is indistinguishable from having kept it: eviction is exact) and every key of a rule no
// longer configured. One shard lock at a time.
func (l *Limiter) sweep(now int64) (removed int) {
	rs := l.rules.Load()
	for i := range l.shards {
		s := &l.shards[i]
		s.mu.Lock()
		for k, tat := range s.m {
			var r *Rule
			if rs != nil {
				r = rs.byID[k.rule]
			}
			if tat <= now || r == nil {
				delete(s.m, k)
				removed++
				l.live.Add(-1)
				if r != nil {
					r.stats.live.Add(-1)
				}
			}
		}
		s.mu.Unlock()
	}
	if l.m.Keys != nil {
		l.m.Keys.Set(float64(l.live.Load()))
	}
	return removed
}

// Keys is the live key count across all rules.
func (l *Limiter) Keys() int64 { return l.live.Load() }

// Rules returns the rule set in force, nil before the first Apply.
func (l *Limiter) Rules() *RuleSet { return l.rules.Load() }

// Len is the number of rules in the set.
func (rs *RuleSet) Len() int {
	if rs == nil {
		return 0
	}
	return len(rs.rules)
}
