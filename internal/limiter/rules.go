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
	"fmt"
	"math"
	"net/netip"
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/s3id"
)

// Mode is the response a denying rule asks for (docs/SPEC.md, Rate limiting, Enforcement modes).
type Mode uint8

// Modes.
const (
	ModeSlowDown        Mode = iota // HTTP 503, S3 code SlowDown
	ModeTooManyRequests             // HTTP 429, S3 code TooManyRequests
)

func (m Mode) String() string {
	if m == ModeTooManyRequests {
		return config.ModeTooManyRequests
	}
	return config.ModeSlowDown
}

// group_by dimensions as bits.
const (
	dimAccessKey uint8 = 1 << iota
	dimBucket
	dimPrefix
	dimSourceIP
)

// The ops bitset holds one bit per operation; the table has 113 and the set has room for 128.
var _ [128 - s3id.NumOps]struct{}

// Rule is one compiled rate-limit rule (docs/DESIGN.md §5.1). Matching reads only the maps,
// slices, and bitset built here; nothing in a match allocates.
type Rule struct {
	// ID is the configured identifier. It is part of every key and never shown to clients.
	ID  string
	idx int

	accessKeys map[string]struct{} // nil = any; "" is a legal member (anonymous)
	buckets    map[string]struct{} // nil = any; "" = service level
	prefixes   []string            // nil = any; sorted longest first; first HasPrefix wins
	cidrs      []netip.Prefix      // nil = any; linear Contains
	ops        [2]uint64           // bitset over Op; every bit set = any
	groupBy    uint8               // dim* bits

	// GCRA parameters after cluster.node_count: emission interval and tolerance in nanoseconds.
	T, tau   int64
	blockAll bool // rate == 0: always deny, Retry-After 1, no state
	mode     Mode

	// As configured, for the admin view.
	src           config.Rule
	effRate       float64
	effBurst      int
	groupByNames  []string
	matchOpsNames []string

	// Per-rule counters carried across reloads by id, and the pre-resolved
	// tollbooth_limiter_denied_total{rule} child (nil without metrics).
	stats  *ruleStats
	denied prometheus.Counter
}

// RuleSet is one compiled configuration, published whole by Apply and read whole by every
// decision. A request in flight finishes under the set it loaded at entry.
type RuleSet struct {
	rules   []*Rule
	byID    map[string]*Rule
	byClass [s3id.NumClasses][]*Rule // candidates per class, in config order
}

// compile builds a RuleSet from a validated config. Validation (internal/config) has already
// rejected everything that can fail here; the errors remain so a reload can never publish a
// half-built set.
func compile(cfg *config.Config) (*RuleSet, error) {
	n := cfg.Cluster.NodeCount
	if n < 1 {
		n = 1
	}
	defMode, err := parseMode(cfg.RateLimits.DefaultOnLimit)
	if err != nil {
		return nil, fmt.Errorf("rate_limits.default_on_limit: %w", err)
	}
	rs := &RuleSet{byID: make(map[string]*Rule, len(cfg.RateLimits.Rules))}
	for i := range cfg.RateLimits.Rules {
		src := &cfg.RateLimits.Rules[i]
		r, err := compileRule(src, i, n, defMode)
		if err != nil {
			return nil, fmt.Errorf("rate_limits.rules[%d] (%s): %w", i, src.ID, err)
		}
		if _, dup := rs.byID[r.ID]; dup {
			return nil, fmt.Errorf("rate_limits.rules[%d]: duplicate id %q", i, r.ID)
		}
		rs.rules = append(rs.rules, r)
		rs.byID[r.ID] = r
		for c := 0; c < s3id.NumClasses; c++ {
			if r.coversClass(s3id.Class(c)) {
				rs.byClass[c] = append(rs.byClass[c], r)
			}
		}
	}
	return rs, nil
}

func compileRule(src *config.Rule, idx, nodeCount int, defMode Mode) (*Rule, error) {
	r := &Rule{ID: strings.Clone(src.ID), idx: idx, src: *src}
	if len(src.Match.AccessKeys) > 0 {
		r.accessKeys = make(map[string]struct{}, len(src.Match.AccessKeys))
		for _, k := range src.Match.AccessKeys {
			r.accessKeys[strings.Clone(k)] = struct{}{}
		}
	}
	if len(src.Match.Buckets) > 0 {
		r.buckets = make(map[string]struct{}, len(src.Match.Buckets))
		for _, b := range src.Match.Buckets {
			r.buckets[strings.Clone(b)] = struct{}{}
		}
	}
	if len(src.Match.Prefixes) > 0 {
		r.prefixes = make([]string, 0, len(src.Match.Prefixes))
		for _, p := range src.Match.Prefixes {
			if p == "" {
				return nil, fmt.Errorf("match.prefixes: empty prefix")
			}
			r.prefixes = append(r.prefixes, strings.Clone(p))
		}
		// Longest first, so the first HasPrefix is the longest match; ties cannot happen for
		// distinct strings of equal length that both prefix the same key.
		sort.SliceStable(r.prefixes, func(i, j int) bool { return len(r.prefixes[i]) > len(r.prefixes[j]) })
	}
	if len(src.Match.SourceCIDRs) > 0 {
		r.cidrs = make([]netip.Prefix, 0, len(src.Match.SourceCIDRs))
		for _, c := range src.Match.SourceCIDRs {
			p, err := parseCIDR(c)
			if err != nil {
				return nil, fmt.Errorf("match.source_cidrs: %w", err)
			}
			r.cidrs = append(r.cidrs, p)
		}
	}
	if len(src.Match.Ops) == 0 {
		r.ops = [2]uint64{math.MaxUint64, math.MaxUint64}
	} else {
		for _, name := range src.Match.Ops {
			if c, ok := s3id.ParseClass(name); ok {
				for _, op := range s3id.Ops() {
					if op.Class() == c {
						r.setOp(op)
					}
				}
				continue
			}
			op, ok := s3id.ParseOp(name)
			if !ok {
				return nil, fmt.Errorf("match.ops: unknown operation or class %q", name)
			}
			r.setOp(op)
		}
	}
	r.matchOpsNames = append([]string(nil), src.Match.Ops...)
	for _, g := range src.GroupBy {
		switch g {
		case config.DimAccessKey:
			r.groupBy |= dimAccessKey
		case config.DimBucket:
			r.groupBy |= dimBucket
		case config.DimPrefix:
			if r.prefixes == nil {
				return nil, fmt.Errorf("group_by: prefix needs match.prefixes")
			}
			r.groupBy |= dimPrefix
		case config.DimSourceIP:
			r.groupBy |= dimSourceIP
		default:
			return nil, fmt.Errorf("group_by: unknown dimension %q", g)
		}
	}
	r.groupByNames = append([]string(nil), src.GroupBy...)
	mode := defMode
	if src.OnLimit != "" {
		var err error
		if mode, err = parseMode(src.OnLimit); err != nil {
			return nil, fmt.Errorf("on_limit: %w", err)
		}
	}
	r.mode = mode

	switch {
	case src.Limit.Rate < 0 || math.IsNaN(src.Limit.Rate) || math.IsInf(src.Limit.Rate, 0):
		return nil, fmt.Errorf("limit.rate: must be >= 0, got %v", src.Limit.Rate)
	case src.Limit.Rate == 0:
		r.blockAll = true
	default:
		// rate_eff = rate / node_count; T = 1e9 / rate_eff; burst_eff = max(1, ceil(burst / n));
		// tau = (burst_eff - 1) × T, so exactly burst_eff requests pass from a full bucket.
		r.effRate = src.Limit.Rate / float64(nodeCount)
		t := math.Round(1e9 / r.effRate)
		if t < 1 {
			t = 1
		}
		if t > float64(maxEmission) {
			return nil, fmt.Errorf("limit.rate %v over %d nodes: emission interval %.0f ns exceeds %d", src.Limit.Rate, nodeCount, t, maxEmission)
		}
		r.T = int64(t)
		burst := src.Limit.Burst
		if burst < 1 {
			burst = 1
		}
		r.effBurst = int(math.Ceil(float64(burst) / float64(nodeCount)))
		if r.effBurst < 1 {
			r.effBurst = 1
		}
		tau := float64(r.effBurst-1) * t
		if tau > float64(maxTolerance) {
			return nil, fmt.Errorf("limit.burst %d at rate %v: the bucket takes %.0f s to refill, more than the %d s bound", burst, src.Limit.Rate, tau/1e9, maxTolerance/1e9)
		}
		r.tau = int64(tau)
	}
	return r, nil
}

// Bounds that keep every GCRA sum inside int64 nanoseconds: an emission interval of at most
// ~11.6 days and a tolerance of at most ~31.7 years. Validation (internal/config) enforces the
// same bounds in configuration terms.
const (
	maxEmission  = int64(1e15)
	maxTolerance = int64(1e18)
)

func parseMode(s string) (Mode, error) {
	switch s {
	case config.ModeSlowDown:
		return ModeSlowDown, nil
	case config.ModeTooManyRequests:
		return ModeTooManyRequests, nil
	}
	return 0, fmt.Errorf("must be %s or %s, got %q", config.ModeSlowDown, config.ModeTooManyRequests, s)
}

func parseCIDR(s string) (netip.Prefix, error) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

func (r *Rule) setOp(op s3id.Op) { r.ops[op>>6] |= 1 << (op & 63) }

func (r *Rule) hasOp(op s3id.Op) bool {
	if int(op) >= s3id.NumOps {
		op = s3id.OpUnknown
	}
	return r.ops[op>>6]&(1<<(op&63)) != 0
}

// coversClass reports whether any operation of the class is in the rule's ops set, which puts
// the rule on that class's candidate list.
func (r *Rule) coversClass(c s3id.Class) bool {
	for _, op := range s3id.Ops() {
		if op.Class() == c && r.hasOp(op) {
			return true
		}
	}
	return false
}

// matches tests the rule against an identity in the cheapest order: ops bit, bucket set,
// access-key set, CIDR list, prefix list. It returns the matched configured prefix ("" when the
// rule has none). Nothing here allocates.
func (r *Rule) matches(id *s3id.RequestIdentity) (prefix string, ok bool) {
	if !r.hasOp(id.Op) {
		return "", false
	}
	if r.buckets != nil {
		if _, ok := r.buckets[id.Bucket]; !ok {
			return "", false
		}
	}
	if r.accessKeys != nil {
		if _, ok := r.accessKeys[id.AccessKey]; !ok {
			return "", false
		}
	}
	if r.cidrs != nil {
		found := false
		for i := range r.cidrs {
			if r.cidrs[i].Contains(id.SourceIP) {
				found = true
				break
			}
		}
		if !found {
			return "", false
		}
	}
	if r.prefixes != nil {
		in := prefixInput(id)
		for _, p := range r.prefixes {
			if strings.HasPrefix(in, p) {
				return p, true
			}
		}
		return "", false
	}
	return "", true
}

// prefixInput is the string a prefix rule matches: the object key for object-level operations,
// the `prefix` query parameter for the four bucket listings (ADR-0001), and nothing otherwise, so
// a prefix rule never matches a bucket- or service-level request (ADR-0001, Gate 0 clarification).
func prefixInput(id *s3id.RequestIdentity) string {
	switch {
	case id.Level == s3id.LevelObject:
		return id.Key
	case id.Op.IsBucketList():
		return id.ListPrefix
	}
	return ""
}

// key is one GCRA bucket's identity (docs/DESIGN.md §5.2): struct values of the identity's
// existing substrings, so a lookup allocates nothing and two tenants can never collide. Strings
// are cloned on first insert only.
type key struct {
	rule   string // Rule.ID; a reload that keeps the id keeps the state
	ak     string // "" unless access_key in group_by
	bucket string // "" unless bucket in group_by
	prefix string // the matched configured prefix, "" unless prefix in group_by
	ip     netip.Addr
}

func (r *Rule) key(id *s3id.RequestIdentity, prefix string) key {
	k := key{rule: r.ID}
	if r.groupBy&dimAccessKey != 0 {
		k.ak = id.AccessKey
	}
	if r.groupBy&dimBucket != 0 {
		k.bucket = id.Bucket
	}
	if r.groupBy&dimPrefix != 0 {
		k.prefix = prefix // already the rule's own string
	}
	if r.groupBy&dimSourceIP != 0 {
		k.ip = id.SourceIP
	}
	return k
}

// clone detaches the request-owned strings so a stored key does not pin a header buffer. rule
// and prefix are the rule's own strings already.
func (k key) clone() key {
	k.ak = strings.Clone(k.ak)
	k.bucket = strings.Clone(k.bucket)
	return k
}
