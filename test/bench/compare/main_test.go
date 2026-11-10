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
	"strings"
	"testing"
)

const baseText = `goos: linux
go: downloading example.com/x v1.0.0
pkg: example.com/p
cpu: Example CPU   9000
BenchmarkA-4   1000000   100.0 ns/op   16 B/op   1 allocs/op
BenchmarkA-4   1000000   110.0 ns/op   16 B/op   1 allocs/op
BenchmarkA-4   1000000   120.0 ns/op   16 B/op   1 allocs/op
BenchmarkB-4   1000000   50.0 ns/op   0 B/op   0 allocs/op
BenchmarkOnlyBase-4   1   1.0 ns/op
PASS
ok  	example.com/p	1.0s
`

const newText = `goos: linux
pkg: example.com/p
cpu: Example CPU 9000
BenchmarkA-4   1000000   125.0 ns/op   16 B/op   1 allocs/op
BenchmarkA-4   1000000   121.0 ns/op   16 B/op   1 allocs/op
BenchmarkA-4   1000000   130.0 ns/op   16 B/op   1 allocs/op
BenchmarkB-4   1000000   49.0 ns/op   8 B/op   1 allocs/op
BenchmarkOnlyNew-4   1   1.0 ns/op
`

func TestParseTakesMediansAndIgnoresNoise(t *testing.T) {
	base, err := parse(strings.NewReader(baseText))
	if err != nil {
		t.Fatal(err)
	}
	if len(base.results) != 3 {
		t.Fatalf("parsed %d benchmarks, want 3 (noise lines must be ignored)", len(base.results))
	}
	if base.cpu != "Example CPU 9000" {
		t.Fatalf("cpu %q", base.cpu)
	}
	a := base.results["example.com/p BenchmarkA-4"]
	if a.nsOp != 110 || a.runs != 3 || a.allocsOp != 1 || !a.hasAllocs || a.nsMin != 100 || a.nsMax != 120 {
		t.Fatalf("BenchmarkA: %+v", a)
	}
	if ob := base.results["example.com/p BenchmarkOnlyBase-4"]; ob.hasAllocs || ob.hasBytes || ob.nsOp != 1 {
		t.Fatalf("BenchmarkOnlyBase: %+v", ob)
	}
}

func TestReportFailsOnSlowdownAndOnMoreAllocs(t *testing.T) {
	base, _ := parse(strings.NewReader(baseText))
	cur, _ := parse(strings.NewReader(newText))
	var out strings.Builder
	// A: 110 → 125 is +13.6%: slower than 10%. B: faster, but 0 → 1 allocs: more allocs.
	if failed := report(&out, base, cur, 10); failed != 2 {
		t.Fatalf("failed = %d, want 2\n%s", failed, out.String())
	}
	s := out.String()
	for _, want := range []string{"example.com/p.BenchmarkA-4", "+13.6%", "SLOWER", "MORE ALLOCS", "BenchmarkOnlyBase-4", "only in the baseline", "BenchmarkOnlyNew-4", "only in the new run", "2 compared, 2 failed"} {
		if !strings.Contains(s, want) {
			t.Errorf("report lacks %q:\n%s", want, s)
		}
	}
	// A 20% threshold lets the slowdown through; the allocation increase still fails.
	out.Reset()
	if failed := report(&out, base, cur, 20); failed != 1 {
		t.Fatalf("failed at 20%% = %d, want 1\n%s", failed, out.String())
	}
}

// Three packages of this repository have a BenchmarkParse. Keyed by name alone they were pooled
// into one median and the gate reported "457 → 913 allocs" when no package allocated more
// (nightly 42885875806).
func TestSameNameInTwoPackagesStaysApart(t *testing.T) {
	const two = `pkg: github.com/o/r/internal/config
cpu: X
BenchmarkParse-4   1000   60000 ns/op   900 B/op   914 allocs/op
pkg: github.com/o/r/internal/s3id
BenchmarkParse-4   1000   130 ns/op   0 B/op   0 allocs/op
`
	const twoPlusOne = `pkg: github.com/o/r/internal/config
cpu: X
BenchmarkParse-4   1000   61000 ns/op   900 B/op   913 allocs/op
pkg: github.com/o/r/internal/s3id
BenchmarkParse-4   1000   131 ns/op   0 B/op   0 allocs/op
pkg: github.com/o/r/test/bench/compare
BenchmarkParse-4   1000   270000 ns/op   900 B/op   1045 allocs/op
`
	base, _ := parse(strings.NewReader(two))
	cur, _ := parse(strings.NewReader(twoPlusOne))
	if len(base.results) != 2 || len(cur.results) != 3 {
		t.Fatalf("parsed %d and %d benchmarks, want 2 and 3", len(base.results), len(cur.results))
	}
	var out strings.Builder
	if failed := report(&out, base, cur, 10); failed != 0 {
		t.Fatalf("failed = %d, want 0\n%s", failed, out.String())
	}
	for _, want := range []string{"internal/config.BenchmarkParse-4", "internal/s3id.BenchmarkParse-4", "test/bench/compare.BenchmarkParse-4", "only in the new run", "2 compared, 0 failed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}

// On another CPU a slowdown is shown and not enforced; an allocation increase still fails.
func TestDifferentCPUEnforcesAllocationsOnly(t *testing.T) {
	base, _ := parse(strings.NewReader(baseText))
	cur, _ := parse(strings.NewReader(strings.Replace(newText, "cpu: Example CPU 9000", "cpu: Another CPU", 1)))
	var out strings.Builder
	// A is 13.6% slower (not enforced here); B went from 0 to 1 allocs (enforced).
	if failed := report(&out, base, cur, 10); failed != 1 {
		t.Fatalf("failed = %d, want 1\n%s", failed, out.String())
	}
	for _, want := range []string{`different CPUs: baseline "Example CPU 9000", this run "Another CPU"`, "NOT enforced", "(other CPU)", "MORE ALLOCS", "1 failed on allocations; 1 past 10% on another CPU, not enforced"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
	// A file whose packages report different CPUs is never "the same CPU" as anything.
	mixed, _ := parse(strings.NewReader("pkg: a/b/c/d\ncpu: X\nBenchmarkA-4 1 1.0 ns/op\npkg: a/b/c/e\ncpu: Y\nBenchmarkB-4 1 1.0 ns/op\n"))
	if mixed.cpu != "mixed" {
		t.Fatalf("cpu %q, want mixed", mixed.cpu)
	}
}

func TestMedian(t *testing.T) {
	if m := median([]float64{3, 1, 2}); m != 2 {
		t.Fatal(m)
	}
	if m := median([]float64{4, 1, 3, 2}); m != 2.5 {
		t.Fatal(m)
	}
	if m := median(nil); m != 0 {
		t.Fatal(m)
	}
}

func BenchmarkParse(b *testing.B) {
	text := strings.Repeat(baseText, 50)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := parse(strings.NewReader(text)); err != nil {
			b.Fatal(err)
		}
	}
}
