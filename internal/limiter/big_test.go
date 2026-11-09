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
	"os"
	"runtime"
	"slices"
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
