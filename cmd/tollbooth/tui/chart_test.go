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

package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestChartShapeAndScale(t *testing.T) {
	// A ramp from 0 to 100 over 40 samples on a 20-column, 4-row chart: the title carries the
	// last value and the axis top, every row fits the width, and the braille climbs left to right.
	vals := make([]float64, 40)
	for i := range vals {
		vals[i] = float64(i) * 100 / 39
	}
	lines := chart("requests", []series{{name: "req", values: vals, unit: "/s"}}, 29, 4, 0)
	if len(lines) != 5 {
		t.Fatalf("%d lines, want 5", len(lines))
	}
	if !strings.Contains(lines[0], "req 100/s") {
		t.Fatalf("title: %q", lines[0])
	}
	for i, l := range lines {
		if lipgloss.Width(l) > 29 {
			t.Fatalf("line %d is %d cells wide: %q", i, lipgloss.Width(l), l)
		}
	}
	if !strings.HasPrefix(strings.TrimSpace(lines[1]), "100") || !strings.HasPrefix(strings.TrimSpace(lines[4]), "0 ") {
		t.Fatalf("axis labels: %q / %q", lines[1], lines[4])
	}
	// The top row has dots only at the right, the bottom row only at the left.
	topRow, bottomRow := []rune(lines[1]), []rune(lines[4])
	plot := func(r []rune) []rune { return r[9:] }
	if plot(topRow)[0] != 0x2800 || plot(topRow)[len(plot(topRow))-1] == 0x2800 {
		t.Fatalf("top row: %q", string(plot(topRow)))
	}
	if plot(bottomRow)[0] == 0x2800 || plot(bottomRow)[len(plot(bottomRow))-1] != 0x2800 {
		t.Fatalf("bottom row: %q", string(plot(bottomRow)))
	}
}

func TestChartEmptyAndFlat(t *testing.T) {
	lines := chart("ttfb", []series{{name: "a"}, {name: "b", unit: " ms"}}, 40, 3, 0)
	if len(lines) != 4 || !strings.Contains(lines[0], "a –") || !strings.Contains(lines[0], "b –") {
		t.Fatalf("%q", lines)
	}
	for _, l := range lines[1:] {
		if strings.ContainsFunc(l, func(r rune) bool { return r > 0x2800 && r <= 0x28FF }) {
			t.Fatalf("empty chart drew dots: %q", l)
		}
	}
	// A flat non-zero series sits on the top row (the axis scales to it).
	flat := chart("x", []series{{name: "f", values: []float64{5, 5, 5, 5}}}, 20, 2, 0)
	if !strings.ContainsFunc(flat[1], func(r rune) bool { return r > 0x2800 && r <= 0x28FF }) {
		t.Fatalf("flat series not on the top row: %q", flat)
	}
	// ymax raises the axis above the data.
	capped := chart("x", []series{{name: "f", values: []float64{5, 5}}}, 20, 2, 50)
	if !strings.HasPrefix(strings.TrimSpace(capped[1]), "50") {
		t.Fatalf("ymax ignored: %q", capped[1])
	}
}

func TestFmtVal(t *testing.T) {
	for _, c := range []struct {
		v    float64
		want string
	}{{0, "0"}, {0.25, "0.2"}, {2.5, "2.5"}, {99, "99"}, {150.4, "150"}, {12345, "12.3k"}, {2.5e6, "2.5M"}} {
		if got := fmtVal(c.v); got != c.want {
			t.Errorf("fmtVal(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestBrailleCellBits(t *testing.T) {
	// One cell, every dot set, is U+28FF.
	grid := make([][]bool, 4)
	for y := range grid {
		grid[y] = []bool{true, true}
	}
	if r := brailleCell(grid, 0, 0); r != 0x28FF {
		t.Fatalf("%U", r)
	}
	// Only the bottom-left dot (grid y=0, x=0) is dot 7 (0x40).
	grid = make([][]bool, 4)
	for y := range grid {
		grid[y] = []bool{false, false}
	}
	grid[0][0] = true
	if r := brailleCell(grid, 0, 0); r != 0x2840 {
		t.Fatalf("%U", r)
	}
}

func BenchmarkChart(b *testing.B) {
	vals := make([]float64, 600)
	for i := range vals {
		vals[i] = float64(i % 97)
	}
	ss := []series{{name: "a", values: vals}, {name: "b", values: vals}}
	b.ReportAllocs()
	for b.Loop() {
		chart("bench", ss, 120, 6, 0)
	}
}
