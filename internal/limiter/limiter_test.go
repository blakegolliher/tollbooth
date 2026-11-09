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
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/s3id"
)

const (
	second = int64(1e9)
	ms     = int64(1e6)
)

const base = "node_id: t\nbackend: {scheme: http, endpoints: [127.0.0.1:1]}\nshowback: {dir: /tmp/x}\n"

// cfg parses base plus the given YAML (rate_limits, limiter, cluster) through the real loader,
// so every test runs against validated, defaulted config.
func cfg(t testing.TB, yaml string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(base + yaml))
	if err != nil {
		t.Fatalf("config: %v\n%s", err, yaml)
	}
	return c
}

// lim builds a limiter with the rules applied and test metrics attached.
func lim(t testing.TB, yaml string) (*Limiter, *Metrics) {
	t.Helper()
	m := testMetrics()
	l := New(m)
	if err := l.Apply(cfg(t, yaml)); err != nil {
		t.Fatal(err)
	}
	return l, m
}

// value reads a counter or gauge without the testutil package (and its extra module).
func value(m prometheus.Metric) float64 {
	var out dto.Metric
	if err := m.Write(&out); err != nil {
		panic(err)
	}
	if out.Counter != nil {
		return out.Counter.GetValue()
	}
	return out.Gauge.GetValue()
}

func testMetrics() *Metrics {
	return &Metrics{
		Decision: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "d"}),
		Keys:     prometheus.NewGauge(prometheus.GaugeOpts{Name: "k"}),
		Overflow: prometheus.NewCounter(prometheus.CounterOpts{Name: "o"}),
		Denied:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "den"}, []string{"rule"}),
		Errors:   prometheus.NewCounter(prometheus.CounterOpts{Name: "e"}),
	}
}

// ident builds the identity the limiter sees after s3id.Parse and classify.
func ident(ak, bucket, key string, op s3id.Op, ip string) *s3id.RequestIdentity {
	id := &s3id.RequestIdentity{AccessKey: ak, Bucket: bucket, Key: key, Op: op, Class: op.Class()}
	switch {
	case key != "":
		id.Level = s3id.LevelObject
	case bucket != "":
		id.Level = s3id.LevelBucket
	}
	if ip != "" {
		id.SourceIP = netip.MustParseAddr(ip)
	}
	return id
}

func getObject(ak, bucket, key string) *s3id.RequestIdentity {
	return ident(ak, bucket, key, s3id.OpGetObject, "10.1.2.3")
}

// admitted counts allowed decisions for id at a fixed instant.
func admitted(l *Limiter, id *s3id.RequestIdentity, now int64, attempts int) int {
	n := 0
	for i := 0; i < attempts; i++ {
		if l.Decide(id, now, true).Allowed {
			n++
		}
	}
	return n
}

const oneRule = `
rate_limits:
  rules:
    - id: r1
      match: {ops: [READ]}
      group_by: [access_key]
      limit: {rate: 100, burst: 50}
`

// Exactly burst requests pass from a full bucket and the next is denied; with tau = burst × T it
// would be burst + 1 (docs/SPEC.md, Rate limiting, GCRA).
func TestExactlyBurst(t *testing.T) {
	l, _ := lim(t, oneRule)
	id := getObject("AKIA1", "b", "k")
	if got := admitted(l, id, 0, 1000); got != 50 {
		t.Fatalf("admitted %d from a full bucket, want exactly burst 50", got)
	}
	d := l.Decide(id, 0, true)
	if d.Allowed || d.Rule == nil || d.Rule.ID != "r1" || d.Mode != ModeSlowDown {
		t.Fatalf("51st request: %+v", d)
	}
	// One emission interval later exactly one more passes.
	if got := admitted(l, id, 10*ms, 10); got != 1 {
		t.Fatalf("after one T: admitted %d, want 1", got)
	}
	if got := admitted(l, id, 10*ms+9*ms, 10); got != 0 {
		t.Fatalf("inside the same T: admitted %d, want 0", got)
	}
}

// Over a closed 10 s window with an attempt every millisecond the admitted count is exactly
// burst + rate × 10: 50 at t = 0 and one per emission interval at t = 10 ms, 20 ms, …, 10,000 ms.
func TestExactTokenCountOverTenSeconds(t *testing.T) {
	l, _ := lim(t, oneRule)
	id := getObject("AKIA1", "b", "k")
	got := 0
	for now := int64(0); now <= 10*second; now += ms {
		if l.Decide(id, now, true).Allowed {
			got++
		}
	}
	if want := 50 + 100*10; got != want {
		t.Fatalf("admitted %d over 10 s, want burst + rate × 10 = %d", got, want)
	}
}

func TestRateZeroAlwaysDenies(t *testing.T) {
	l, m := lim(t, `
rate_limits:
  rules:
    - id: frozen
      match: {access_keys: [AKIAREVOKED]}
      limit: {rate: 0}
`)
	id := getObject("AKIAREVOKED", "b", "k")
	for i := 0; i < 100; i++ {
		d := l.Decide(id, int64(i)*second, true)
		if d.Allowed || d.RetryAfter != 1 || d.Wait != second || d.Rule.ID != "frozen" {
			t.Fatalf("attempt %d: %+v", i, d)
		}
	}
	if l.Keys() != 0 {
		t.Fatalf("rate 0 keeps no state; live keys %d", l.Keys())
	}
	if n := value(m.Denied.WithLabelValues("frozen")); n != 100 {
		t.Fatalf("denied counter %v, want 100", n)
	}
	if d := l.Decide(getObject("AKIAOTHER", "b", "k"), 0, true); !d.Allowed || d.Matched != 0 {
		t.Fatalf("other key must not match: %+v", d)
	}
}

// Two rules in config order: a denial by the second still charges the first (the S3 behavior; no
// check-then-consume, no rollback), and the test asserts the charge.
func TestDenialChargesEarlierRule(t *testing.T) {
	l, _ := lim(t, `
rate_limits:
  rules:
    - id: wide
      match: {ops: [READ]}
      limit: {rate: 1, burst: 10}
    - id: narrow
      match: {ops: [READ], buckets: [hot]}
      limit: {rate: 1, burst: 1}
`)
	hot := getObject("AK", "hot", "k")
	cold := getObject("AK", "cold", "k")
	if d := l.Decide(hot, 0, true); !d.Allowed || d.Matched != 2 {
		t.Fatalf("first hot request: %+v", d)
	}
	d := l.Decide(hot, 0, true)
	if d.Allowed || d.Rule.ID != "narrow" {
		t.Fatalf("second hot request must be denied by narrow: %+v", d)
	}
	// wide has been charged twice (10 − 2 = 8 left), even though the second request was denied.
	if got := admitted(l, cold, 0, 100); got != 8 {
		t.Fatalf("wide admitted %d more, want 8 (two tokens charged, one by a denied request)", got)
	}
}

// Evicting a full bucket is lossless: a swept limiter and an unswept one make identical decisions
// on the same sequence.
func TestEvictionLossless(t *testing.T) {
	a, _ := lim(t, oneRule)
	b, _ := lim(t, oneRule)
	id := getObject("AKIA1", "b", "k")
	admitted(a, id, 0, 50)
	admitted(b, id, 0, 50)
	// At t = 500 ms the TAT (0 + 50 × 10 ms) equals now: the bucket is full again.
	now := 500 * ms
	if n := a.sweep(now); n != 1 || a.Keys() != 0 {
		t.Fatalf("sweep removed %d keys (live %d), want the one full bucket", n, a.Keys())
	}
	if n := b.sweep(now - 1); n != 0 {
		t.Fatalf("a bucket not yet full was swept: %d", n)
	}
	for i := 0; i < 200; i++ {
		t0 := now + int64(i)*3*ms
		da, db := a.Decide(id, t0, true), b.Decide(id, t0, true)
		if da.Allowed != db.Allowed || da.RetryAfter != db.RetryAfter {
			t.Fatalf("decision %d diverged after eviction: swept %+v, kept %+v", i, da, db)
		}
	}
}

// A reload never resets state: a consumed bucket stays consumed under the new set; a changed rate
// applies from the next decision; the keys of a removed rule are swept.
func TestReloadKeepsState(t *testing.T) {
	l, _ := lim(t, oneRule)
	id := getObject("AKIA1", "b", "k")
	admitted(l, id, 0, 50)
	if err := l.Apply(cfg(t, strings.Replace(oneRule, "rate: 100, burst: 50", "rate: 1000, burst: 50", 1))); err != nil {
		t.Fatal(err)
	}
	if d := l.Decide(id, 0, true); d.Allowed {
		t.Fatal("reload reset the bucket")
	}
	// New T = 1 ms: the next token arrives at TAT − tau = 500 ms − 49 ms = 451 ms.
	if d := l.Decide(id, 450*ms, true); d.Allowed {
		t.Fatal("old rate still applied after reload")
	}
	if d := l.Decide(id, 451*ms, true); !d.Allowed {
		t.Fatalf("new rate not applied: %+v", d)
	}
	if st := l.Status(); len(st) != 1 || st[0].LiveKeys != 1 || st[0].Denied != 2 {
		t.Fatalf("counters did not survive the reload: %+v", st)
	}
	if err := l.Apply(cfg(t, "rate_limits:\n  rules: []\n")); err != nil {
		t.Fatal(err)
	}
	if n := l.sweep(0); n != 1 || l.Keys() != 0 {
		t.Fatalf("removed rule's key not swept: removed %d, live %d", n, l.Keys())
	}
	if d := l.Decide(id, 0, true); !d.Allowed || d.Matched != 0 {
		t.Fatalf("no rules: %+v", d)
	}
}

// 64 goroutines on one key at one instant never exceed burst.
func TestConcurrentOneKeyNeverExceedsBound(t *testing.T) {
	l, _ := lim(t, oneRule)
	id := getObject("AKIA1", "b", "k")
	var wg sync.WaitGroup
	var allowed [64]int
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if l.Decide(id, 0, true).Allowed {
					allowed[g]++
				}
			}
		}(g)
	}
	wg.Wait()
	total := 0
	for _, n := range allowed {
		total += n
	}
	if total != 50 {
		t.Fatalf("64 goroutines admitted %d, want exactly burst 50", total)
	}
}

// The decision allocates nothing at steady state, with 100 rules loaded and every one of them
// matching (docs/DESIGN.md §9 stage 5).
func TestDecideAllocatesZero(t *testing.T) {
	l, _ := lim(t, hundredRules(true))
	id := getObject("AKIA1", "b7", "ckpt/7/x")
	l.Decide(id, 0, true) // first sight inserts the keys
	allocs := testing.AllocsPerRun(1000, func() { l.Decide(id, 0, true) })
	if allocs != 0 {
		t.Fatalf("Decide allocates %.2f per call; the budget is 0", allocs)
	}
	allocs = testing.AllocsPerRun(1000, func() { l.Decide(id, 0, false) })
	if allocs != 0 {
		t.Fatalf("Decide(consume=false) allocates %.2f per call; the budget is 0", allocs)
	}
	// A denial clones the key into the deny ring's slot (2 strings) until the ring has lapped once
	// with the same key; a storm on one key then allocates nothing.
	l2, _ := lim(t, oneRule)
	denied := getObject("AKIA1", "b", "k")
	for i := 0; i < 50+ringSize+10; i++ {
		l2.Decide(denied, 0, true)
	}
	allocs = testing.AllocsPerRun(1000, func() { l2.Decide(denied, 0, true) })
	if allocs != 0 {
		t.Fatalf("a repeated denial allocates %.2f per call after the ring lapped; the slot must be reused", allocs)
	}
}

// hundredRules writes 100 rules; with matchAll every one matches a READ on bucket b7 under ckpt/
// from AKIA1, otherwise each pins a different bucket so one request matches one rule.
func hundredRules(matchAll bool) string {
	var b strings.Builder
	b.WriteString("rate_limits:\n  rules:\n")
	for i := 0; i < 100; i++ {
		bucket := fmt.Sprintf("b%d", i)
		if matchAll {
			bucket = "b7"
		}
		fmt.Fprintf(&b, "    - id: rule-%03d\n      match: {ops: [READ, WRITE], buckets: [%s], prefixes: [ckpt/, logs/], access_keys: [AKIA1, AKIA2], source_cidrs: [10.0.0.0/8]}\n      group_by: [access_key, bucket, prefix, source_ip]\n      limit: {rate: 100000, burst: 100000}\n", i, bucket)
	}
	return b.String()
}

func TestKeyCapFailsOpen(t *testing.T) {
	l, m := lim(t, oneRule+"limiter: {max_keys: 10}\n")
	for i := 0; i < 20; i++ {
		d := l.Decide(getObject(fmt.Sprintf("AKIA%02d", i), "b", "k"), 0, true)
		if !d.Allowed {
			t.Fatalf("key %d denied: %+v", i, d)
		}
		if want := i >= 10; d.Overflow != want {
			t.Fatalf("key %d overflow %v, want %v", i, d.Overflow, want)
		}
	}
	if l.Keys() != 10 {
		t.Fatalf("live keys %d, want the cap 10", l.Keys())
	}
	if n := value(m.Overflow); n != 10 {
		t.Fatalf("overflow counter %v, want 10", n)
	}
	if n := value(m.Keys); n != 10 {
		t.Fatalf("keys gauge %v, want 10", n)
	}
	// An overflowed key is unlimited, which is the fail-open contract.
	if got := admitted(l, getObject("AKIA19", "b", "k"), 0, 500); got != 500 {
		t.Fatalf("overflowed key admitted %d of 500", got)
	}
}

// A panic inside the limiter fails open and counts; a rule whose stats pointer is missing stands
// in for an internal fault.
func TestLimiterPanicFailsOpen(t *testing.T) {
	l, m := lim(t, "rate_limits:\n  rules:\n    - id: r\n      limit: {rate: 0}\n")
	l.rules.Load().rules[0].stats = nil
	d := l.Decide(getObject("AK", "b", "k"), 0, true)
	if !d.Allowed {
		t.Fatalf("panic must fail open: %+v", d)
	}
	if n := value(m.Errors); n != 1 {
		t.Fatalf("errors counter %v, want 1", n)
	}
}

func TestRetryAfter(t *testing.T) {
	for wait, want := range map[int64]int{0: 1, 1: 1, second: 1, second + 1: 2, 3*second - 1: 3, 10 * second: 10, -5: 1} {
		if got := retryAfter(wait); got != want {
			t.Errorf("retryAfter(%d) = %d, want %d", wait, got, want)
		}
	}
}

func TestNodeCountDividesRateAndBurst(t *testing.T) {
	l, _ := lim(t, oneRule+"cluster: {node_count: 4}\n")
	id := getObject("AKIA1", "b", "k")
	// burst 50 / 4 → 13 (rounded up); rate 100 / 4 → 25 req/s, T = 40 ms.
	if got := admitted(l, id, 0, 100); got != 13 {
		t.Fatalf("admitted %d, want ceil(50/4) = 13", got)
	}
	if got := admitted(l, id, 40*ms, 10); got != 1 {
		t.Fatalf("after 40 ms admitted %d, want 1", got)
	}
	st := l.Status()
	if st[0].EffectiveRate != 25 || st[0].EffectiveBurst != 13 || st[0].Rate != 100 || st[0].Burst != 50 {
		t.Fatalf("%+v", st[0])
	}
	l2, _ := lim(t, "rate_limits:\n  rules:\n    - id: r\n      limit: {rate: 1, burst: 1}\ncluster: {node_count: 8}\n")
	if got := admitted(l2, id, 0, 5); got != 1 {
		t.Fatalf("burst never rounds below 1: admitted %d", got)
	}
}

func TestFractionalRate(t *testing.T) {
	l, _ := lim(t, "rate_limits:\n  rules:\n    - id: slow\n      limit: {rate: 0.5}\n")
	id := getObject("AK", "b", "k")
	if got := admitted(l, id, 0, 5); got != 1 {
		t.Fatalf("burst defaults to max(1, ceil(0.5)) = 1; admitted %d", got)
	}
	d := l.Decide(id, 0, true)
	if d.Allowed || d.RetryAfter != 2 || d.Wait != 2*second {
		t.Fatalf("T = 2 s so Retry-After is 2 and the wait is 2 s: %+v", d)
	}
	if d := l.Decide(id, 500*ms, true); d.Allowed || d.RetryAfter != 2 || d.Wait != 1500*ms {
		t.Fatalf("half a second later the wait is 1.5 s, Retry-After still rounds up to 2: %+v", d)
	}
	if d := l.Decide(id, 2*second, true); !d.Allowed {
		t.Fatalf("one request every two seconds: %+v", d)
	}
}

func TestModes(t *testing.T) {
	l, _ := lim(t, `
rate_limits:
  default_on_limit: too_many_requests
  rules:
    - id: a
      match: {buckets: [a]}
      limit: {rate: 0}
    - id: b
      match: {buckets: [b]}
      limit: {rate: 0}
      on_limit: slowdown
`)
	if d := l.Decide(getObject("AK", "a", "k"), 0, true); d.Mode != ModeTooManyRequests || d.Mode.String() != "too_many_requests" {
		t.Fatalf("default mode: %+v", d)
	}
	if d := l.Decide(getObject("AK", "b", "k"), 0, true); d.Mode != ModeSlowDown || d.Mode.String() != "slowdown" {
		t.Fatalf("rule mode: %+v", d)
	}
}

// Matching: every condition is AND, omitted means any, and each dimension is exact except the
// longest-match prefix and the CIDR table.
func TestMatchers(t *testing.T) {
	l, _ := lim(t, `
rate_limits:
  rules:
    - id: keys
      match: {access_keys: [AKIA1, ""]}
      limit: {rate: 0}
    - id: buckets
      match: {buckets: [data, ""]}
      limit: {rate: 0}
    - id: prefixes
      match: {buckets: [p], prefixes: [ckpt/, ckpt/deep/, logs/]}
      group_by: [prefix]
      limit: {rate: 0}
    - id: cidrs
      match: {buckets: [c], source_cidrs: [10.20.0.0/16, 192.0.2.7]}
      limit: {rate: 0}
    - id: ops
      match: {buckets: [o], ops: [LIST, DeleteObject]}
      limit: {rate: 0}
`)
	cases := []struct {
		name string
		id   *s3id.RequestIdentity
		want string // denying rule id, "" when allowed
	}{
		{"key exact", getObject("AKIA1", "x", "k"), "keys"},
		{"key other", getObject("AKIA2", "x", "k"), ""},
		{"anonymous matches the empty member", getObject("", "x", "k"), "keys"},
		{"bucket exact", getObject("AKIA9", "data", "k"), "buckets"},
		{"bucket suffix is not a match", getObject("AKIA9", "data2", "k"), ""},
		{"service level matches the empty member", ident("AKIA9", "", "", s3id.OpListBuckets, ""), "buckets"},
		{"prefix match", getObject("AKIA9", "p", "ckpt/1"), "prefixes"},
		{"prefix no match", getObject("AKIA9", "p", "model/1"), ""},
		{"prefix on a bucket-level op never matches", ident("AKIA9", "p", "", s3id.OpHeadBucket, ""), ""},
		{"list with prefix matches (ADR-0001)", withListPrefix(ident("AKIA9", "p", "", s3id.OpListObjectsV2, ""), "ckpt/a"), "prefixes"},
		{"list without prefix matches bucket-level rules only", ident("AKIA9", "p", "", s3id.OpListObjectsV2, ""), ""},
		{"list with a shorter prefix than every configured one", withListPrefix(ident("AKIA9", "p", "", s3id.OpListObjectsV2, ""), "ck"), ""},
		{"cidr inside", ident("AKIA9", "c", "k", s3id.OpGetObject, "10.20.3.4"), "cidrs"},
		{"cidr single host", ident("AKIA9", "c", "k", s3id.OpGetObject, "192.0.2.7"), "cidrs"},
		{"cidr outside", ident("AKIA9", "c", "k", s3id.OpGetObject, "10.21.0.1"), ""},
		{"cidr no address", ident("AKIA9", "c", "k", s3id.OpGetObject, ""), ""},
		{"ops class", ident("AKIA9", "o", "", s3id.OpListObjectsV2, ""), "ops"},
		{"ops name", ident("AKIA9", "o", "k", s3id.OpDeleteObject, ""), "ops"},
		{"ops other", ident("AKIA9", "o", "k", s3id.OpGetObject, ""), ""},
	}
	for _, c := range cases {
		d := l.Decide(c.id, 0, true)
		got := ""
		if !d.Allowed {
			got = d.Rule.ID
		}
		if got != c.want {
			t.Errorf("%s: denied by %q, want %q (%+v)", c.name, got, c.want, d)
		}
	}
}

func withListPrefix(id *s3id.RequestIdentity, p string) *s3id.RequestIdentity {
	id.ListPrefix = p
	return id
}

// The longest configured prefix wins and becomes the key's prefix dimension.
func TestLongestPrefixWinsAndKeysTheBucket(t *testing.T) {
	l, _ := lim(t, `
rate_limits:
  rules:
    - id: p
      match: {prefixes: [ckpt/, ckpt/deep/]}
      group_by: [prefix]
      limit: {rate: 1, burst: 1}
`)
	if d := l.Decide(getObject("AK", "b", "ckpt/deep/x"), 0, true); !d.Allowed {
		t.Fatalf("%+v", d)
	}
	// ckpt/ is a different bucket from ckpt/deep/: still a token there.
	if d := l.Decide(getObject("AK", "b", "ckpt/x"), 0, true); !d.Allowed {
		t.Fatalf("shallow prefix shares the deep prefix's bucket: %+v", d)
	}
	if d := l.Decide(getObject("AK", "b", "ckpt/deep/y"), 0, true); d.Allowed {
		t.Fatal("deep prefix bucket should be empty")
	}
	ex := l.Explain(getObject("AK", "b", "ckpt/deep/y"), 0)
	if len(ex.Matches) != 1 || ex.Matches[0].Prefix != "ckpt/deep/" || ex.Allowed || ex.DeniedBy != "p" || ex.Matches[0].LiveKeys != 2 {
		t.Fatalf("%+v", ex)
	}
}

// group_by dimensions each create independent buckets; the empty group_by is one shared bucket.
func TestGroupBy(t *testing.T) {
	l, _ := lim(t, `
rate_limits:
  rules:
    - id: shared
      match: {buckets: [s]}
      limit: {rate: 1, burst: 2}
    - id: per-ip
      match: {buckets: [i]}
      group_by: [source_ip]
      limit: {rate: 1, burst: 1}
    - id: per-key-bucket
      match: {buckets: [kb1, kb2]}
      group_by: [access_key, bucket]
      limit: {rate: 1, burst: 1}
`)
	if admitted(l, getObject("A", "s", "k"), 0, 1)+admitted(l, getObject("B", "s", "k"), 0, 1)+admitted(l, getObject("C", "s", "k"), 0, 1) != 2 {
		t.Fatal("shared bucket must admit 2 across three keys")
	}
	for _, ip := range []string{"10.0.0.1", "10.0.0.2", "fd00::1"} {
		if got := admitted(l, ident("A", "i", "k", s3id.OpGetObject, ip), 0, 3); got != 1 {
			t.Fatalf("ip %s admitted %d, want 1", ip, got)
		}
	}
	for _, c := range []struct{ ak, b string }{{"A", "kb1"}, {"A", "kb2"}, {"B", "kb1"}} {
		if got := admitted(l, getObject(c.ak, c.b, "k"), 0, 3); got != 1 {
			t.Fatalf("%s/%s admitted %d, want 1", c.ak, c.b, got)
		}
	}
	if l.Keys() != 1+3+3 {
		t.Fatalf("live keys %d, want 7", l.Keys())
	}
	top := l.Top(100, 60*second, 0)
	if len(top) != 7 {
		t.Fatalf("top has %d keys, want 7: %+v", len(top), top)
	}
}

// Keys are struct values of the identity's strings: a stored key must not alias the request's
// buffer, and two tenants whose concatenation would collide stay distinct.
func TestKeysAreClonedAndNeverCollide(t *testing.T) {
	l, _ := lim(t, `
rate_limits:
  rules:
    - id: r
      group_by: [access_key, bucket]
      limit: {rate: 1, burst: 1}
`)
	buf := []byte("AKIAXbucket")
	ak := string(buf[:5])
	b := string(buf[5:])
	if !l.Decide(getObject(ak, b, "k"), 0, true).Allowed {
		t.Fatal("first")
	}
	// "AKIAXb" + "ucket" concatenates to the same bytes but is a different tenant.
	if !l.Decide(getObject("AKIAXb", "ucket", "k"), 0, true).Allowed {
		t.Fatal("distinct tuple collided with the first key")
	}
	if l.Keys() != 2 {
		t.Fatalf("live %d, want 2", l.Keys())
	}
}

func TestTopWindowAndOrder(t *testing.T) {
	l, _ := lim(t, `
rate_limits:
  rules:
    - id: r
      group_by: [access_key]
      limit: {rate: 0}
`)
	for i := 0; i < 5; i++ {
		l.Decide(getObject("A", "b", "k"), 0, true)
	}
	for i := 0; i < 3; i++ {
		l.Decide(getObject("B", "b", "k"), 30*second, true)
	}
	l.Decide(getObject("C", "b", "k"), 59*second, true)
	top := l.Top(2, 60*second, 60*second)
	if len(top) != 2 || top[0].AccessKey != "A" || top[0].Denied != 5 || top[1].AccessKey != "B" || top[1].Denied != 3 || top[1].LastSeconds != 30 {
		t.Fatalf("%+v", top)
	}
	top = l.Top(10, 10*second, 60*second)
	if len(top) != 1 || top[0].AccessKey != "C" || top[0].LastSeconds != 1 {
		t.Fatalf("window 10 s: %+v", top)
	}
	if len(l.Top(0, 60*second, 0)) != 0 || len(New(nil).Top(5, 1, 0)) != 0 {
		t.Fatal("n ≤ 0 or no rules is empty")
	}
	// The ring wraps: 1,024 entries hold the last 1,024 denials only.
	for i := 0; i < 3000; i++ {
		l.Decide(getObject(fmt.Sprintf("K%d", i), "b", "k"), 100*second, true)
	}
	top = l.Top(5000, 60*second, 100*second)
	if len(top) != ringSize {
		t.Fatalf("ring holds %d keys, want %d", len(top), ringSize)
	}
}

func TestStatusAndExplain(t *testing.T) {
	l, _ := lim(t, `
rate_limits:
  rules:
    - id: tenant-a-writes
      match: {access_keys: [AKIAEXAMPLE1], buckets: [training-data], prefixes: [ckpt/], source_cidrs: [10.20.0.0/16], ops: [WRITE]}
      group_by: [access_key, bucket]
      limit: {rate: 2000, burst: 4000}
    - id: lists
      match: {ops: [LIST]}
      group_by: [source_ip]
      limit: {rate: 50, burst: 200}
      on_limit: too_many_requests
`)
	st := l.Status()
	if len(st) != 2 || st[0].ID != "tenant-a-writes" || st[0].Rate != 2000 || st[0].EffectiveBurst != 4000 || st[0].OnLimit != "slowdown" ||
		len(st[0].Prefixes) != 1 || st[1].OnLimit != "too_many_requests" || len(st[1].GroupBy) != 1 || st[1].Ops[0] != "LIST" {
		t.Fatalf("%+v", st)
	}
	id := ident("AKIAEXAMPLE1", "training-data", "ckpt/1", s3id.OpPutObject, "10.20.1.1")
	ex := l.Explain(id, 0)
	if !ex.Allowed || len(ex.Matches) != 1 || ex.Matches[0].Rule != "tenant-a-writes" || ex.Matches[0].AccessKey != "AKIAEXAMPLE1" || ex.Matches[0].Bucket != "training-data" || ex.Matches[0].Prefix != "" || ex.Matches[0].SourceIP != "" {
		t.Fatalf("%+v", ex)
	}
	if l.Keys() != 0 {
		t.Fatal("Explain consumed a token")
	}
	lst := ident("AK", "b", "", s3id.OpListObjectsV2, "10.9.9.9")
	admitted(l, lst, 0, 200)
	ex = l.Explain(lst, 0)
	if ex.Allowed || ex.DeniedBy != "lists" || ex.Mode != "too_many_requests" || ex.RetryAfter != 1 || ex.Matches[0].SourceIP != "10.9.9.9" || ex.Matches[0].LiveKeys != 1 {
		t.Fatalf("%+v", ex)
	}
	if New(nil).Explain(id, 0).Allowed != true || len(New(nil).Status()) != 0 {
		t.Fatal("empty limiter")
	}
}

// Compile rejects what validation would have, so a reload can never publish a half-built set.
func TestCompileErrors(t *testing.T) {
	bad := func(r config.Rule) error {
		c := cfg(t, "")
		c.RateLimits.Rules = []config.Rule{r}
		_, err := compile(c)
		return err
	}
	cases := map[string]config.Rule{
		"bad cidr":    {ID: "a", Match: config.Match{SourceCIDRs: []string{"nope"}}, Limit: config.Limit{Rate: 1, Burst: 1}, OnLimit: "slowdown"},
		"bad op":      {ID: "a", Match: config.Match{Ops: []string{"Nope"}}, Limit: config.Limit{Rate: 1, Burst: 1}, OnLimit: "slowdown"},
		"bad mode":    {ID: "a", Limit: config.Limit{Rate: 1, Burst: 1}, OnLimit: "hold"},
		"bad groupby": {ID: "a", GroupBy: []string{"key"}, Limit: config.Limit{Rate: 1, Burst: 1}, OnLimit: "slowdown"},
		"prefix dim":  {ID: "a", GroupBy: []string{"prefix"}, Limit: config.Limit{Rate: 1, Burst: 1}, OnLimit: "slowdown"},
		"empty pfx":   {ID: "a", Match: config.Match{Prefixes: []string{""}}, Limit: config.Limit{Rate: 1, Burst: 1}, OnLimit: "slowdown"},
		"neg rate":    {ID: "a", Limit: config.Limit{Rate: -1, Burst: 1}, OnLimit: "slowdown"},
		"tiny rate":   {ID: "a", Limit: config.Limit{Rate: 1e-9, Burst: 1}, OnLimit: "slowdown"},
		"huge burst":  {ID: "a", Limit: config.Limit{Rate: 0.001, Burst: 2_000_000_000}, OnLimit: "slowdown"},
	}
	for name, r := range cases {
		if err := bad(r); err == nil {
			t.Errorf("%s: compiled", name)
		}
	}
	c := cfg(t, "")
	c.RateLimits.Rules = []config.Rule{{ID: "a", Limit: config.Limit{Rate: 1, Burst: 1}, OnLimit: "slowdown"}, {ID: "a", Limit: config.Limit{Rate: 1, Burst: 1}, OnLimit: "slowdown"}}
	if _, err := compile(c); err == nil {
		t.Error("duplicate id compiled")
	}
	c.RateLimits.DefaultOnLimit = "hold"
	c.RateLimits.Rules = nil
	if _, err := compile(c); err == nil {
		t.Error("bad default mode compiled")
	}
	if err := New(nil).Apply(c); err == nil {
		t.Error("Apply accepted a bad set")
	}
}

// Every rule in the examples of docs/SPEC.md compiles and classifies as documented; a rule
// without ops covers every class and one with a class covers only it.
func TestClassLists(t *testing.T) {
	l, _ := lim(t, `
rate_limits:
  rules:
    - id: any
      limit: {rate: 1}
    - id: writes
      match: {ops: [WRITE]}
      limit: {rate: 1}
    - id: one-op
      match: {ops: [HeadBucket]}
      limit: {rate: 1}
`)
	rs := l.rules.Load()
	if len(rs.rules) != 3 {
		t.Fatal(len(rs.rules))
	}
	for c := 0; c < s3id.NumClasses; c++ {
		ids := []string{}
		for _, r := range rs.byClass[c] {
			ids = append(ids, r.ID)
		}
		got := strings.Join(ids, ",")
		want := "any"
		switch s3id.Class(c) {
		case s3id.ClassWrite:
			want = "any,writes"
		case s3id.ClassBucket:
			want = "any,one-op"
		}
		if got != want {
			t.Errorf("class %s candidates %q, want %q", s3id.Class(c), got, want)
		}
	}
}

// Unknown operations and out-of-range classes are clamped, never indexed past the arrays.
func TestOutOfRangeEnums(t *testing.T) {
	l, _ := lim(t, "rate_limits:\n  rules:\n    - id: any\n      limit: {rate: 0}\n")
	id := &s3id.RequestIdentity{Op: s3id.Op(250), Class: s3id.Class(99)}
	if d := l.Decide(id, 0, true); d.Allowed {
		t.Fatalf("an any-op rule denies Unknown too: %+v", d)
	}
}

func BenchmarkDecide100Rules(b *testing.B) {
	l, _ := lim(b, hundredRules(false))
	id := getObject("AKIA1", "b7", "ckpt/7/x")
	b.ReportAllocs()
	for b.Loop() {
		l.Decide(id, 0, true)
	}
}

// Every one of the 100 rules matches: the worst case for one request.
func BenchmarkDecide100RulesAllMatch(b *testing.B) {
	l, _ := lim(b, hundredRules(true))
	id := getObject("AKIA1", "b7", "ckpt/7/x")
	b.ReportAllocs()
	for b.Loop() {
		l.Decide(id, 0, true)
	}
}

func BenchmarkDecideNoRules(b *testing.B) {
	l := New(nil)
	id := getObject("AKIA1", "b7", "ckpt/7/x")
	b.ReportAllocs()
	for b.Loop() {
		l.Decide(id, 0, true)
	}
}

func BenchmarkMatch100Rules(b *testing.B) {
	l, _ := lim(b, hundredRules(false))
	id := getObject("AKIA1", "b7", "ckpt/7/x")
	rs := l.rules.Load()
	b.ReportAllocs()
	for b.Loop() {
		for _, r := range rs.byClass[id.Class] {
			r.matches(id)
		}
	}
}

func BenchmarkGCRA(b *testing.B) {
	l, _ := lim(b, oneRule)
	id := getObject("AKIA1", "b", "k")
	r := l.rules.Load().rules[0]
	k := r.key(id, "")
	l.Decide(id, 0, true)
	b.ReportAllocs()
	for b.Loop() {
		l.gcra(r, k, 0, true)
	}
}

// Parallel decisions on distinct keys scale across shards.
func BenchmarkDecideParallel(b *testing.B) {
	l, _ := lim(b, oneRule)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		id := getObject(fmt.Sprintf("AKIA%d", b.N%1000), "b", "k")
		for pb.Next() {
			l.Decide(id, 0, true)
		}
	})
}
