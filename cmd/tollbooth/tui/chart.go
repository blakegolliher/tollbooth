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
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// A braille line chart (ADR-0006): each cell is a 2×4 dot matrix, so a chart of h rows and w
// columns plots 2w points at 4h vertical levels. Several series share one y axis, the newest
// sample at the right edge. No dependency: this is the whole rasterizer.

// series is one line on a chart.
type series struct {
	name   string
	values []float64 // oldest first
	unit   string    // suffix for the title's current value, e.g. "/s", " ms"
}

// chart renders a title line followed by h rows of braille, each at most w cells wide, from the
// last 2w samples of every series. The y axis runs from 0 to the maximum seen across the series
// (or ymax when larger), printed at the top-right of the title. With no samples it draws the empty
// grid so the layout does not jump when data arrives.
func chart(title string, ss []series, w, h int, ymax float64) []string {
	if w < 8 {
		w = 8
	}
	if h < 1 {
		h = 1
	}
	labelW := 9 // "  1.2k ⎹" axis label column
	plotW := w - labelW
	if plotW < 4 {
		plotW = 4
	}
	points := 2 * plotW
	levels := 4 * h
	top := ymax
	for _, s := range ss {
		for _, v := range tail(s.values, points) {
			if v > top {
				top = v
			}
		}
	}
	// Dot grid: grid[y][x], y=0 is the bottom level.
	grid := make([][]bool, levels)
	for y := range grid {
		grid[y] = make([]bool, points)
	}
	for _, s := range ss {
		vals := tail(s.values, points)
		offset := points - len(vals) // right-align: newest at the right edge
		prev := -1
		for i, v := range vals {
			y := level(v, top, levels)
			x := offset + i
			grid[y][x] = true
			// Connect consecutive samples vertically so a jump reads as a line, not two dots.
			if prev >= 0 {
				lo, hi := min(prev, y), max(prev, y)
				for yy := lo; yy <= hi; yy++ {
					grid[yy][x] = true
				}
			}
			prev = y
		}
	}
	lines := make([]string, 0, h+1)
	var cur []string
	for _, s := range ss {
		if len(s.values) == 0 {
			cur = append(cur, s.name+" –")
			continue
		}
		cur = append(cur, s.name+" "+fmtVal(s.values[len(s.values)-1])+s.unit)
	}
	lines = append(lines, fit(styleTitle.Render("  "+title)+"  "+styleOK.Render(strings.Join(cur, " · ")), w))
	for row := h - 1; row >= 0; row-- {
		var b strings.Builder
		for col := 0; col < plotW; col++ {
			b.WriteRune(brailleCell(grid, row, col))
		}
		label := ""
		switch row {
		case h - 1:
			label = fmtVal(top)
		case 0:
			label = "0"
		}
		lines = append(lines, styleDim.Render(fmt.Sprintf("%7s ⎹", label))+lipgloss.NewStyle().Foreground(colorBlue).Render(b.String()))
	}
	return lines
}

// level maps a value to a dot row in [0, levels-1].
func level(v, top float64, levels int) int {
	if top <= 0 || v <= 0 || math.IsNaN(v) {
		return 0
	}
	y := int(math.Round(v / top * float64(levels-1)))
	return max(0, min(levels-1, y))
}

// brailleCell builds the braille rune for chart cell (row, col) from the dot grid. Braille dot
// numbering: columns left/right, rows top to bottom; dots 1,2,3,7 are the left column, 4,5,6,8 the
// right column.
func brailleCell(grid [][]bool, row, col int) rune {
	var bits rune
	// (dx, dy) → bit; dy is 0 at the top of the cell.
	set := func(dx, dy int, bit rune) {
		y := row*4 + (3 - dy) // grid y: bottom-up
		x := col*2 + dx
		if y < len(grid) && x < len(grid[y]) && grid[y][x] {
			bits |= bit
		}
	}
	set(0, 0, 0x01)
	set(0, 1, 0x02)
	set(0, 2, 0x04)
	set(1, 0, 0x08)
	set(1, 1, 0x10)
	set(1, 2, 0x20)
	set(0, 3, 0x40)
	set(1, 3, 0x80)
	return 0x2800 + bits
}

func tail(v []float64, n int) []float64 {
	if len(v) > n {
		return v[len(v)-n:]
	}
	return v
}

// fmtVal prints a value for an axis or a title: integers under 10,000 as is, then k and M.
func fmtVal(v float64) string {
	switch {
	case math.IsNaN(v) || math.IsInf(v, 0):
		return "–"
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e4:
		return fmt.Sprintf("%.1fk", v/1e3)
	case v >= 100 || v == math.Trunc(v):
		return fmt.Sprintf("%.0f", v)
	default:
		return fmt.Sprintf("%.1f", v)
	}
}
