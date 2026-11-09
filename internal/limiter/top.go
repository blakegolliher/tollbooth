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
	"sort"
	"sync"
)

// ringSize is the per-rule deny ring (D-019): 1,024 entries, written only on denial, read only
// by the operator.
const ringSize = 1024

type denyEntry struct {
	k  key
	at int64
}

// ring is the fixed buffer of a rule's most recent denials (docs/DESIGN.md §5.5). The backing
// array is allocated on the first denial, so rules that never deny cost nothing.
type ring struct {
	mu sync.Mutex
	n  int
	e  *[ringSize]denyEntry
}

// add records a denial. The slot's strings are reused when the same key lands there again, so a
// storm on few keys allocates nothing after the first lap.
func (r *ring) add(k key, now int64) {
	r.mu.Lock()
	if r.e == nil {
		r.e = new([ringSize]denyEntry)
	}
	e := &r.e[r.n%ringSize]
	if e.k != k {
		e.k = k.clone()
	}
	e.at = now
	r.n++
	r.mu.Unlock()
}

// TopEntry is one row of GET /admin/limits/top: a key and how often its rule denied it inside
// the window.
type TopEntry struct {
	Rule      string `json:"rule"`
	AccessKey string `json:"access_key,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	SourceIP  string `json:"source_ip,omitempty"`
	Denied    int    `json:"denied"`
	// LastSeconds is how long ago, in seconds, the most recent denial of this key happened.
	LastSeconds float64 `json:"last_denied_seconds_ago"`
}

// Top returns the n most-denied keys across all rules within the last window nanoseconds, most
// denied first. Allowed requests never touched the rings, so this is a read of denials only.
func (l *Limiter) Top(n int, window, now int64) []TopEntry {
	rs := l.rules.Load()
	if rs == nil || n <= 0 {
		return []TopEntry{}
	}
	since := now - window
	type agg struct {
		count int
		last  int64
	}
	counts := map[key]*agg{}
	for _, r := range rs.rules {
		rg := &r.stats.ring
		rg.mu.Lock()
		if rg.e != nil {
			filled := min(rg.n, ringSize)
			for i := 0; i < filled; i++ {
				e := &rg.e[i]
				if e.at < since {
					continue
				}
				a := counts[e.k]
				if a == nil {
					a = &agg{}
					counts[e.k] = a
				}
				a.count++
				a.last = max(a.last, e.at)
			}
		}
		rg.mu.Unlock()
	}
	out := make([]TopEntry, 0, len(counts))
	for k, a := range counts {
		t := TopEntry{Rule: k.rule, AccessKey: k.ak, Bucket: k.bucket, Prefix: k.prefix, Denied: a.count, LastSeconds: float64(now-a.last) / 1e9}
		if k.ip.IsValid() {
			t.SourceIP = k.ip.String()
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Denied != out[j].Denied {
			return out[i].Denied > out[j].Denied
		}
		if out[i].Rule != out[j].Rule {
			return out[i].Rule < out[j].Rule
		}
		return out[i].AccessKey+out[i].Bucket+out[i].Prefix+out[i].SourceIP < out[j].AccessKey+out[j].Bucket+out[j].Prefix+out[j].SourceIP
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// RuleStatus is one row of GET /admin/limits: the rule as configured, its effective per-node
// limit, and its counters.
type RuleStatus struct {
	ID          string   `json:"id"`
	AccessKeys  []string `json:"access_keys,omitempty"`
	Buckets     []string `json:"buckets,omitempty"`
	Prefixes    []string `json:"prefixes,omitempty"`
	SourceCIDRs []string `json:"source_cidrs,omitempty"`
	Ops         []string `json:"ops,omitempty"`
	GroupBy     []string `json:"group_by"`
	Rate        float64  `json:"rate"`
	Burst       int      `json:"burst"`
	// EffectiveRate and EffectiveBurst are what this node enforces after cluster.node_count.
	EffectiveRate  float64 `json:"effective_rate"`
	EffectiveBurst int     `json:"effective_burst"`
	OnLimit        string  `json:"on_limit"`
	LiveKeys       int64   `json:"live_keys"`
	Denied         int64   `json:"denied"`
}

// Status lists the rules in force, in config order, with live key counts and denials since the
// rule id first appeared.
func (l *Limiter) Status() []RuleStatus {
	rs := l.rules.Load()
	if rs == nil {
		return []RuleStatus{}
	}
	out := make([]RuleStatus, 0, len(rs.rules))
	for _, r := range rs.rules {
		st := RuleStatus{
			ID: r.ID, AccessKeys: r.src.Match.AccessKeys, Buckets: r.src.Match.Buckets, Prefixes: r.src.Match.Prefixes,
			SourceCIDRs: r.src.Match.SourceCIDRs, Ops: r.matchOpsNames, GroupBy: r.groupByNames,
			Rate: r.src.Limit.Rate, Burst: r.src.Limit.Burst, EffectiveRate: r.effRate, EffectiveBurst: r.effBurst,
			OnLimit: r.mode.String(), LiveKeys: r.stats.live.Load(), Denied: r.stats.denied.Load(),
		}
		if st.GroupBy == nil {
			st.GroupBy = []string{}
		}
		out = append(out, st)
	}
	return out
}
