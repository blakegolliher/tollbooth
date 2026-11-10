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
	"fmt"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blakegolliher/tollbooth/internal/s3id"
)

// TestDecisionLatencyAtOneMillionKeys is the performance-target row "limiter decision, 100 rules,
// 1,000,000 live keys: ≤ 10 µs p99" and the memory row's per-key cost (docs/SPEC.md, Performance
// targets; docs/DESIGN.md §9.4). It runs only with TOLLBOOTH_BIG=1 because it needs ~300 MiB and
// a minute; the gate report quotes its output.
func TestDecisionLatencyAtOneMillionKeys(t *testing.T) {
	if os.Getenv("TOLLBOOTH_BIG") == "" {
		t.Skip("set TOLLBOOTH_BIG=1 to run the 1,000,000-key latency and memory measurement")
	}
	l, _ := lim(t, hundredRules(false))
	const keys = 1_000_000
	ids := make([]*s3id.RequestIdentity, 0, 10_000)
	for i := 0; i < 10_000; i++ {
		ids = append(ids, getObject("AKIA1", fmt.Sprintf("b%d", i%100), "ckpt/x"))
		ids[i].SourceIP = netip.AddrFrom4(ipFor(i))
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	// 100 rules × (2 access keys are matched but the key is per access_key, bucket, prefix, ip);
	// vary the ip so each decision inserts a new key: 1,000,000 distinct (rule, ak, bucket, prefix, ip).
	for i := 0; i < keys; i++ {
		id := ids[i%len(ids)]
		id.SourceIP = netip.AddrFrom4(ipFor(i))
		l.Decide(id, 0, true)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	live := l.Keys()
	perKey := float64(after.HeapAlloc-before.HeapAlloc) / float64(live)
	t.Logf("live keys %d; heap delta %.1f MiB; %.0f bytes per key", live, float64(after.HeapAlloc-before.HeapAlloc)/(1<<20), perKey)
	if live != keys {
		t.Fatalf("expected %d live keys, got %d", keys, live)
	}

	// Latency: 1,000,000 steady-state decisions on existing keys, each timed.
	samples := make([]time.Duration, 0, keys)
	for i := 0; i < keys; i++ {
		id := ids[i%len(ids)]
		id.SourceIP = netip.AddrFrom4(ipFor(i))
		t0 := time.Now()
		l.Decide(id, 0, true)
		samples = append(samples, time.Since(t0))
	}
	slices.Sort(samples)
	p := func(q float64) time.Duration { return samples[int(float64(len(samples)-1)*q)] }
	t.Logf("decision latency at %d live keys, 100 rules: p50 %v p90 %v p99 %v p99.9 %v max %v", live, p(0.5), p(0.9), p(0.99), p(0.999), samples[len(samples)-1])
	if p99 := p(0.99); p99 > 10*time.Microsecond {
		t.Errorf("p99 %v exceeds the 10 µs target", p99)
	}
	// The sweep over 1,000,000 keys at TAT ≤ now removes them all.
	t0 := time.Now()
	removed := l.sweep(1 << 62)
	t.Logf("sweep of %d keys took %v", removed, time.Since(t0))
	if removed != keys || l.Keys() != 0 {
		t.Fatalf("sweep removed %d, live %d", removed, l.Keys())
	}
}

func ipFor(i int) (a [4]byte) {
	a[0] = 10
	a[1] = byte(i >> 16)
	a[2] = byte(i >> 8)
	a[3] = byte(i)
	return a
}

// TestUniqueKeyFlood is the chaos row "10,000 new unique keys per second for 60 s, RSS stays
// under cap" (docs/SPEC.md, Test plan category 9; docs/DESIGN.md §11): a rule whose buckets fill
// in 10 ms, a key cap of 100,000, the sweeper on a 1 s interval, and 600,000 distinct source
// addresses over a minute. Live keys never exceed the cap, the overflow path stays allowed, and the
// heap stays within the cap's footprint. Runs with TOLLBOOTH_BIG=1 only (it takes the full minute).
func TestUniqueKeyFlood(t *testing.T) {
	if os.Getenv("TOLLBOOTH_BIG") == "" {
		t.Skip("set TOLLBOOTH_BIG=1 to run the 60 s unique-key flood")
	}
	l, m := lim(t, `
limits:
  - name: per-ip
    operation: [GetObject, HeadObject]
    limit_per_second: 100
    burst: 1
    per: source_ip
limiter: {max_keys: 100000, sweep_interval: 1s}
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)
	id := getObject("AKIA1", "b", "k")
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	const perSecond, seconds = 10_000, 60
	var maxLive int64
	var denied int
	start := time.Now()
	for s := 0; s < seconds; s++ {
		tick := start.Add(time.Duration(s+1) * time.Second)
		for i := 0; i < perSecond; i++ {
			id.SourceIP = netip.AddrFrom4(ipFor(s*perSecond + i))
			if !l.Decide(id, l.Now(time.Now()), true).Allowed {
				denied++
			}
		}
		maxLive = max(maxLive, l.Keys())
		time.Sleep(time.Until(tick))
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	rss := vmRSS()
	t.Logf("60 s × 10,000 new keys/s: peak live keys %d (cap 100,000), %.0f overflow (fail-open) decisions, %d denied, heap now %.1f MiB (was %.1f), heap sys %.1f MiB, RSS %.1f MiB, sweeps kept the store at %d keys",
		maxLive, value(m.Overflow), denied, float64(after.HeapAlloc)/(1<<20), float64(before.HeapAlloc)/(1<<20), float64(after.HeapSys)/(1<<20), rss, l.Keys())
	if maxLive > 100_000 {
		t.Fatalf("live keys %d exceeded the cap", maxLive)
	}
	if denied != 0 {
		t.Fatalf("%d requests denied: a new key is a full bucket and must be admitted", denied)
	}
	// 100,000 keys at ~226 B is ~22 MiB; the heap must not have grown past a few times that.
	if grow := after.HeapAlloc - before.HeapAlloc; grow > 128<<20 {
		t.Fatalf("heap grew %.1f MiB over the flood", float64(grow)/(1<<20))
	}
}

// vmRSS reads the process RSS in MiB from /proc; 0 when unavailable.
func vmRSS() float64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			f := strings.Fields(l)
			if len(f) >= 2 {
				kb, _ := strconv.ParseFloat(f[1], 64)
				return kb / 1024
			}
		}
	}
	return 0
}
