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

// Command compare is the benchmark regression gate (docs/SPEC.md, Performance targets,
// "Regression"; CLAUDE.md): it reads two `go test -bench -benchmem` outputs, takes the median of
// each benchmark's ns/op, B/op, and allocs/op over its -count runs, prints them side by side with
// the change, and exits 1 when any benchmark's median ns/op is slower than the baseline by more
// than -threshold percent or its allocs/op went up at all. Lines that are not benchmark results
// (package headers, `go: downloading`, PASS/ok) are ignored, which is what benchstat did not do:
// a `go:` line in one file and not the other made benchstat print two tables and compare nothing.
//
//	go run ./test/bench/compare -threshold 10 test/bench/baseline.txt test/bench/new.txt
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
)

type sample struct{ nsOp, bOp, allocsOp float64 }

type result struct {
	name                string
	nsOp, bOp, allocsOp float64 // medians
	nsMin, nsMax        float64
	runs                int
	hasAllocs, hasBytes bool
}

func main() {
	threshold := flag.Float64("threshold", 10, "percent slowdown of the median ns/op that fails the gate")
	flag.Usage = func() {
		_, _ = fmt.Fprintf(flag.CommandLine.Output(), "usage: compare [-threshold pct] baseline.txt new.txt\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}
	base, err := readFile(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	cur, err := readFile(flag.Arg(1))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	failed := report(os.Stdout, base, cur, *threshold)
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "compare: %d benchmark(s) regressed past %.0f%% or allocate more\n", failed, *threshold)
		os.Exit(1)
	}
}

func readFile(path string) (map[string]result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return parse(f)
}

// parse collects the result lines of a `go test -bench` run. A result line is
// "BenchmarkName-N  iterations  X ns/op [Y B/op] [Z allocs/op]"; everything else is skipped.
func parse(r io.Reader) (map[string]result, error) {
	samples := map[string][]sample{}
	hasB := map[string]bool{}
	hasA := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || !strings.HasPrefix(fields[0], "Benchmark") {
			continue
		}
		var s sample
		ok := false
		for i := 2; i+1 < len(fields); i += 2 {
			v, err := strconv.ParseFloat(fields[i], 64)
			if err != nil {
				break
			}
			switch fields[i+1] {
			case "ns/op":
				s.nsOp, ok = v, true
			case "B/op":
				s.bOp = v
				hasB[fields[0]] = true
			case "allocs/op":
				s.allocsOp = v
				hasA[fields[0]] = true
			}
		}
		if ok {
			samples[fields[0]] = append(samples[fields[0]], s)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]result, len(samples))
	for name, ss := range samples {
		ns := make([]float64, len(ss))
		bs := make([]float64, len(ss))
		as := make([]float64, len(ss))
		for i, s := range ss {
			ns[i], bs[i], as[i] = s.nsOp, s.bOp, s.allocsOp
		}
		out[name] = result{
			name: name, runs: len(ss),
			nsOp: median(ns), bOp: median(bs), allocsOp: median(as),
			nsMin: slices.Min(ns), nsMax: slices.Max(ns),
			hasBytes: hasB[name], hasAllocs: hasA[name],
		}
	}
	return out, nil
}

func median(v []float64) float64 {
	s := slices.Clone(v)
	slices.Sort(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// report prints one row per benchmark present in both files and returns how many failed the
// gate. Benchmarks only in one file are listed after the table and never fail it.
func report(w io.Writer, base, cur map[string]result, threshold float64) int {
	names := make([]string, 0, len(base))
	for n := range base {
		if _, ok := cur[n]; ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	_, _ = fmt.Fprintf(w, "%-40s %14s %14s %8s %16s %10s\n", "benchmark (median of runs)", "base ns/op", "new ns/op", "delta", "allocs base→new", "verdict")
	failed := 0
	for _, n := range names {
		b, c := base[n], cur[n]
		delta := 0.0
		if b.nsOp > 0 {
			delta = (c.nsOp - b.nsOp) / b.nsOp * 100
		}
		verdict := "ok"
		if delta > threshold {
			verdict = "SLOWER"
			failed++
		}
		if b.hasAllocs && c.hasAllocs && c.allocsOp > b.allocsOp {
			verdict = "MORE ALLOCS"
			failed++
		}
		_, _ = fmt.Fprintf(w, "%-40s %14.1f %14.1f %+7.1f%% %7.0f → %-6.0f %10s\n", n, b.nsOp, c.nsOp, delta, b.allocsOp, c.allocsOp, verdict)
	}
	for _, n := range sortedOnly(base, cur) {
		_, _ = fmt.Fprintf(w, "%-40s only in the baseline\n", n)
	}
	for _, n := range sortedOnly(cur, base) {
		_, _ = fmt.Fprintf(w, "%-40s only in the new run\n", n)
	}
	_, _ = fmt.Fprintf(w, "%d compared, %d failed (threshold %.0f%% on median ns/op; any increase in allocs/op fails)\n", len(names), failed, threshold)
	return failed
}

func sortedOnly(a, b map[string]result) []string {
	var out []string
	for n := range a {
		if _, ok := b[n]; !ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
