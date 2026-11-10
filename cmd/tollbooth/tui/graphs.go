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
	"sort"
)

// The Graphs screen (ADR-0006): braille line charts over the kept history, newest at the right.
// Charts in priority order; as many as the terminal height holds are drawn. A chart whose legend
// wraps is taller, so the count is decided by the lines each one actually takes.

// graphsLines renders the screen for the current size.
func (m Model) graphsLines() []string {
	room := m.height - 2 // tabs and footer
	if m.stale {
		room--
	}
	rows := 4
	if room >= 30 {
		rows = 5
	}
	var lines []string
	span := ""
	if len(m.history) > 1 {
		span = fmt.Sprintf("%d polls over %s", len(m.history), m.history[len(m.history)-1].at.Sub(m.history[0].at).Round(1e9))
	} else {
		span = "no history yet"
	}
	if m.paused {
		span += " · PAUSED"
	}
	for i, c := range m.chartSet() {
		cl := chart(c.title, c.series, m.width-1, rows, c.ymax)
		if i > 0 && len(lines)+len(cl)+1 > room { // +1: the history line under the charts
			break
		}
		lines = append(lines, cl...)
	}
	lines = append(lines, styleDim.Render("  "+span))
	return lines
}

type chartSpec struct {
	title  string
	series []series
	ymax   float64
}

// chartSet builds the charts from the history.
func (m Model) chartSet() []chartSpec {
	h := m.history
	pick := func(f func(s sample) float64) []float64 {
		out := make([]float64, len(h))
		for i := range h {
			out[i] = f(h[i])
		}
		return out
	}
	specs := []chartSpec{
		{title: "requests and throttles per second", series: []series{
			{name: "req", values: pick(func(s sample) float64 { return s.requests }), unit: "/s"},
			{name: "throttled", values: pick(func(s sample) float64 { return s.throttled }), unit: "/s"},
		}},
	}
	// Backend TTFB, one series per endpoint, in endpoint order.
	if len(h) > 0 {
		eps := make([]string, 0, len(h[len(h)-1].ttfb))
		for ep := range h[len(h)-1].ttfb {
			eps = append(eps, ep)
		}
		sort.Strings(eps)
		ss := make([]series, 0, len(eps))
		for _, ep := range eps {
			ss = append(ss, series{name: ep, values: pick(func(s sample) float64 { return s.ttfb[ep] }), unit: " ms"}) // the full host:port, never shortened
		}
		if len(ss) > 0 {
			specs = append(specs, chartSpec{title: "backend time to first byte", series: ss})
		}
	}
	// Denied per second per rule: the top four rules by denials, from the cumulative counters.
	if len(h) > 1 {
		last := h[len(h)-1].denied
		rules := make([]string, 0, len(last))
		for r := range last {
			rules = append(rules, r)
		}
		sort.Slice(rules, func(i, j int) bool {
			if last[rules[i]] != last[rules[j]] {
				return last[rules[i]] > last[rules[j]]
			}
			return rules[i] < rules[j]
		})
		if len(rules) > 4 {
			rules = rules[:4]
		}
		ss := make([]series, 0, len(rules))
		for _, r := range rules {
			vals := make([]float64, len(h))
			for i := 1; i < len(h); i++ {
				dt := h[i].at.Sub(h[i-1].at).Seconds()
				if dt > 0 {
					vals[i] = float64(h[i].denied[r]-h[i-1].denied[r]) / dt
				}
			}
			ss = append(ss, series{name: r, values: vals, unit: "/s"})
		}
		if len(ss) > 0 {
			specs = append(specs, chartSpec{title: "denied per second, by rule", series: ss})
		}
	}
	specs = append(specs,
		chartSpec{title: "requests in flight", series: []series{{name: "in flight", values: pick(func(s sample) float64 { return s.inflight })}}},
		chartSpec{title: "process memory, MiB", series: []series{
			{name: "heap", values: pick(func(s sample) float64 { return s.heapMiB }), unit: " MiB"},
			{name: "rss", values: pick(func(s sample) float64 { return s.rssMiB }), unit: " MiB"},
		}},
		chartSpec{title: "goroutines", series: []series{{name: "goroutines", values: pick(func(s sample) float64 { return s.goroutines })}}},
	)
	return specs
}
