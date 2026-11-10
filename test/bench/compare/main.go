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
// (`go: downloading`, PASS/ok) are ignored, which is what benchstat did not do: a `go:` line in
// one file and not the other made benchstat print two tables and compare nothing.
//
// Two things the first version of this command got wrong, found by the nightly of 2026-11-10:
//
//   - A benchmark is its package and its name. Three packages have a BenchmarkParse; keyed by name
//     alone they were pooled into one median, and the gate reported an allocation increase that no
//     package had.
//
//   - Timings are only comparable on the same CPU. The hosted runner is not one machine: the
//     baseline was recorded on an AMD EPYC and that nightly ran on an Intel Xeon, where code
//     untouched since the baseline measured 30 to 99% slower. When the `cpu:` lines of the two
//     files differ, the timing deltas are printed and marked, and do not fail the gate.
//     Allocation counts do not depend on the CPU and are always enforced.
//
//     go run ./test/bench/compare -threshold 10 test/bench/baseline.txt test/bench/new.txt
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

// run is one file: its benchmarks by "package name" and the CPU the run reports.
type run struct {
	results map[string]result
	cpu     string // the `cpu:` line; "" when the file has none, "mixed" when it has several
}

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

func readFile(path string) (run, error) {
	f, err := os.Open(path)
	if err != nil {
		return run{}, err
	}
	defer func() { _ = f.Close() }()
	return parse(f)
}

// parse collects the result lines of a `go test -bench` run. A result line is
// "BenchmarkName-N  iterations  X ns/op [Y B/op] [Z allocs/op]" and belongs to the package named
// by the last `pkg:` line before it; the `cpu:` lines say what the run ran on; everything else is
// skipped.
func parse(r io.Reader) (run, error) {
	samples := map[string][]sample{}
	hasB := map[string]bool{}
	hasA := map[string]bool{}
	pkg, cpu := "", ""
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "pkg:"); ok {
			pkg = strings.TrimSpace(v)
			continue
		}
		if v, ok := strings.CutPrefix(line, "cpu:"); ok {
			switch v = strings.Join(strings.Fields(v), " "); {
			case cpu == "":
				cpu = v
			case cpu != v:
				cpu = "mixed"
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 || !strings.HasPrefix(fields[0], "Benchmark") {
			continue
		}
		key := fields[0]
		if pkg != "" {
			key = pkg + " " + fields[0]
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
				hasB[key] = true
			case "allocs/op":
				s.allocsOp = v
				hasA[key] = true
			}
		}
		if ok {
			samples[key] = append(samples[key], s)
		}
	}
	if err := sc.Err(); err != nil {
		return run{}, err
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
			name: display(name), runs: len(ss),
			nsOp: median(ns), bOp: median(bs), allocsOp: median(as),
			nsMin: slices.Min(ns), nsMax: slices.Max(ns),
			hasBytes: hasB[name], hasAllocs: hasA[name],
		}
	}
	return run{results: out, cpu: cpu}, nil
}

// display shortens "github.com/owner/repo/internal/config BenchmarkParse-4" to
// "internal/config.BenchmarkParse-4": the path inside the module is what tells two benchmarks of
// one name apart.
func display(key string) string {
	pkg, name, ok := strings.Cut(key, " ")
	if !ok {
		return key
	}
	if parts := strings.SplitN(pkg, "/", 4); len(parts) == 4 {
		pkg = parts[3]
	}
	return pkg + "." + name
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
// gate. Benchmarks only in one file are listed after the table and never fail it. A slowdown past
// the threshold fails only when both runs report the same CPU; an allocation increase always fails.
func report(w io.Writer, base, cur run, threshold float64) int {
	names := make([]string, 0, len(base.results))
	for n := range base.results {
		if _, ok := cur.results[n]; ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	sameCPU := base.cpu == cur.cpu && base.cpu != "mixed"
	if !sameCPU {
		_, _ = fmt.Fprintf(w, "different CPUs: baseline %q, this run %q. Timings are shown and NOT enforced; allocations are.\n", base.cpu, cur.cpu)
	}
	_, _ = fmt.Fprintf(w, "%-52s %14s %14s %8s %16s %12s\n", "benchmark (median of runs)", "base ns/op", "new ns/op", "delta", "allocs base→new", "verdict")
	failed, slower := 0, 0
	for _, n := range names {
		b, c := base.results[n], cur.results[n]
		delta := 0.0
		if b.nsOp > 0 {
			delta = (c.nsOp - b.nsOp) / b.nsOp * 100
		}
		verdict, fail := "ok", false
		if delta > threshold {
			slower++
			if sameCPU {
				verdict, fail = "SLOWER", true
			} else {
				verdict = "(other CPU)"
			}
		}
		if b.hasAllocs && c.hasAllocs && c.allocsOp > b.allocsOp {
			verdict, fail = "MORE ALLOCS", true
		}
		if fail {
			failed++
		}
		_, _ = fmt.Fprintf(w, "%-52s %14.1f %14.1f %+7.1f%% %7.0f → %-6.0f %12s\n", b.name, b.nsOp, c.nsOp, delta, b.allocsOp, c.allocsOp, verdict)
	}
	for _, n := range sortedOnly(base.results, cur.results) {
		_, _ = fmt.Fprintf(w, "%-52s only in the baseline\n", base.results[n].name)
	}
	for _, n := range sortedOnly(cur.results, base.results) {
		_, _ = fmt.Fprintf(w, "%-52s only in the new run\n", cur.results[n].name)
	}
	if sameCPU {
		_, _ = fmt.Fprintf(w, "%d compared, %d failed (threshold %.0f%% on median ns/op; any increase in allocs/op fails)\n", len(names), failed, threshold)
	} else {
		_, _ = fmt.Fprintf(w, "%d compared, %d failed on allocations; %d past %.0f%% on another CPU, not enforced\n", len(names), failed, slower, threshold)
	}
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
