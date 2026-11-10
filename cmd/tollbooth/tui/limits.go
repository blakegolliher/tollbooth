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
	"strings"
	"time"

	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/limiter"
)

// The Limits screen (ADR-0007): every rule as the clause tollbooth reads back for it, with how
// much traffic it is looking at and how much of that it throttles right now; the selected rule in
// full; and the operations that are busiest this instant, which is where a limit usually starts.

// limiterRule wraps a status row for the Limits table.
type limiterRule struct{ *limiter.RuleStatus }

// quietAfter is how long a rule may have matched nothing, with traffic flowing, before the screen
// says so.
const quietAfter = time.Minute

func (m Model) limitsLines() []string {
	rules := m.visibleRules()
	var lines []string
	if m.filtering || m.filter.Value() != "" {
		lines = append(lines, "  "+m.filter.View())
	}
	if m.testing {
		return append(lines, m.testFormLines()...)
	}
	const nameW, numW = 20, 11
	sumW := m.width - 2 - nameW - 1 - 2*(numW+1)
	if sumW < 16 {
		sumW = 16
	}
	lines = append(lines, styleHeader.Render(fmt.Sprintf("  %-*s %-*s %*s %*s", nameW, "LIMIT", sumW, "MEANS", numW, "MATCHED/S", numW, "THROTTLED/S")))
	if len(rules) == 0 {
		lines = append(lines, styleDim.Render("  (no limits; press n to add one)"))
	}
	for i, r := range rules {
		matched, throttled := "–", "–"
		if rate, ok := m.ruleRate[r.Name]; ok {
			matched, throttled = fmtRate(rate[0]), fmtRate(rate[1])
		}
		if m.quiet(r.RuleStatus) {
			matched = "none ⚠"
		}
		mark := "  "
		if i == m.selected {
			mark = "▸ " // visible without color too
		}
		row := fmt.Sprintf("%s%-*s %-*s %*s %*s", mark, nameW, trunc(r.Name, nameW), sumW, trunc(r.Summary, sumW), numW, matched, numW, throttled)
		if i == m.selected {
			row = styleSel.Render(row)
		}
		lines = append(lines, row)
	}
	if m.selected < len(rules) {
		lines = append(lines, "")
		lines = append(lines, m.ruleDetail(rules[m.selected].RuleStatus)...)
	}
	lines = append(lines, "")
	return append(lines, m.busiestLines(m.height-len(lines)-4)...)
}

// quiet reports a rule that has matched nothing since it was loaded although requests are
// flowing: the rule that names the wrong key, bucket, or operation.
func (m Model) quiet(r *limiter.RuleStatus) bool {
	s := &m.snap.status
	return r.Matched == 0 && s.Rates.Requests60s > 0 && m.snap.at.Sub(s.LoadedAt) >= quietAfter
}

// ruleDetail is the selected rule in full: its sentence, its note, what is wrong with it if
// anything, and who it is holding right now.
func (m Model) ruleDetail(r *limiter.RuleStatus) []string {
	w := m.width - 4
	var lines []string
	sentence := wrap(r.Name+": "+r.Sentence, w)
	if len(sentence) > 3 {
		sentence = append(sentence[:2], trunc(strings.Join(sentence[2:], " "), w))
	}
	for _, l := range sentence {
		lines = append(lines, "  "+l)
	}
	if r.Note != "" {
		lines = append(lines, styleDim.Render("  note: "+trunc(r.Note, w-6)))
	}
	if m.quiet(r) {
		lines = append(lines,
			styleWarn.Render(fit(fmt.Sprintf("  ⚠ no request has matched this rule in the %s since it was loaded", ago(m.snap.at.Sub(m.snap.status.LoadedAt))), m.width)),
			styleWarn.Render(fit("    check the names it uses; t tests a request against the rules", m.width)))
	}
	var held []string
	for _, e := range m.snap.top.Top {
		if e.Rule != r.Name || len(held) == 3 {
			continue
		}
		who := dims(e.AccessKey, e.Bucket, e.Prefix, e.SourceIP)
		if e.AccessKey+e.Bucket+e.Prefix+e.SourceIP == "" {
			who = "requests" // the rule counts everyone together, so there is no one to name
		}
		held = append(held, fmt.Sprintf("%s (%d)", who, e.Denied))
	}
	if len(held) > 0 {
		lines = append(lines, styleDim.Render(fit("  held in the last minute: "+strings.Join(held, " · "), m.width)))
	}
	return lines
}

// busiestLines lists the operations with the most requests in the last poll interval and the
// rules that limit each, in at most room lines.
func (m Model) busiestLines(room int) []string {
	if room < 2 {
		return nil
	}
	lines := []string{styleHeader.Render(fmt.Sprintf("  %-34s %10s %12s  %s", "BUSIEST OPERATIONS RIGHT NOW", "REQ/S", "THROTTLED/S", "LIMITED BY"))}
	ops := m.busiest()
	if m.opRate == nil {
		return append(lines, styleDim.Render("  (measured between two polls; one more moment)"))
	}
	if len(ops) == 0 {
		return append(lines, styleDim.Render("  (no requests in the last poll)"))
	}
	for i, op := range ops {
		if i >= room-1 {
			break
		}
		by := m.limitedBy(op)
		if by == "" {
			by = styleDim.Render("no limit")
		}
		lines = append(lines, fit(fmt.Sprintf("  %-34s %10s %12s  %s", op, fmtRate(m.opRate[op]), fmtRate(m.opThrottle[op]), by), m.width))
	}
	return lines
}

// busiest is the operations with traffic in the last poll interval, most requests first.
func (m Model) busiest() []string {
	ops := make([]string, 0, len(m.opRate))
	for op, r := range m.opRate {
		if r > 0 {
			ops = append(ops, op)
		}
	}
	sort.Slice(ops, func(i, j int) bool {
		if m.opRate[ops[i]] != m.opRate[ops[j]] {
			return m.opRate[ops[i]] > m.opRate[ops[j]]
		}
		return ops[i] < ops[j]
	})
	return ops
}

// limitedBy names the rules in force that are about op: the ones whose `operation` covers it, and
// the ones that limit every request of every client. A rule that names no operation but narrows
// by key, bucket, prefix, or address is about those, not about the operation, and is left out. One
// that names the operation and also narrows sees only part of its traffic and is marked "(part)",
// so the column never says an operation is limited when most of its requests are not.
func (m Model) limitedBy(op string) string {
	var names []string
	for i := range m.snap.limits.Rules {
		r := &m.snap.limits.Rules[i]
		narrowed := len(r.AccessKey)+len(r.Bucket)+len(r.Prefix)+len(r.SourceIP) > 0
		covers := len(r.Operation) == 0 && !narrowed
		for _, entry := range r.Operation {
			if ops, _, err := config.ResolveOperation(entry); err == nil {
				for _, o := range ops {
					if o.String() == op {
						covers = true
					}
				}
			}
		}
		switch {
		case covers && narrowed:
			// Only some of the operation's requests: one access key's, one bucket's.
			names = append(names, r.Name+" (part)")
		case covers:
			names = append(names, r.Name)
		}
	}
	if len(names) > 2 {
		return fmt.Sprintf("%s, %s +%d", names[0], names[1], len(names)-2)
	}
	return strings.Join(names, ", ")
}

// ago prints a span the way a person says it: 4 min, 1 h, 2 h 10 min.
func ago(d time.Duration) string {
	mins := int(d.Round(time.Minute) / time.Minute)
	switch {
	case mins < 60:
		return fmt.Sprintf("%d min", mins)
	case mins%60 == 0:
		return fmt.Sprintf("%d h", mins/60)
	}
	return fmt.Sprintf("%d h %d min", mins/60, mins%60)
}

// fmtRate prints a per-second rate: whole numbers with separators, small ones with one decimal.
func fmtRate(r float64) string {
	switch {
	case r <= 0:
		return "0"
	case r < 10:
		return fmt.Sprintf("%.1f", r)
	}
	return config.FormatCount(float64(int64(r + 0.5)))
}

// wrap breaks s into lines of at most w cells at spaces; a word longer than w is cut.
func wrap(s string, w int) []string {
	if w < 8 {
		w = 8
	}
	var lines []string
	line := ""
	for _, word := range strings.Fields(s) {
		for len([]rune(word)) > w {
			if line != "" {
				lines = append(lines, line)
				line = ""
			}
			r := []rune(word)
			lines = append(lines, string(r[:w]))
			word = string(r[w:])
		}
		switch {
		case line == "":
			line = word
		case len([]rune(line))+1+len([]rune(word)) <= w:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}
