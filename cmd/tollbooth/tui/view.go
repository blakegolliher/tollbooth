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
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/blakegolliher/tollbooth/internal/limiter"
	"github.com/blakegolliher/tollbooth/internal/showback"
)

// limiterRule wraps a status row for the Limits table.
type limiterRule struct{ *limiter.RuleStatus }

var (
	styleTitle  = lipgloss.NewStyle().Bold(true)
	styleTab    = lipgloss.NewStyle().Reverse(true).Bold(true)
	styleHeader = lipgloss.NewStyle().Underline(true)
	styleStale  = lipgloss.NewStyle().Reverse(true)
	styleSel    = lipgloss.NewStyle().Reverse(true)
	styleDim    = lipgloss.NewStyle().Faint(true)
)

// View renders the current screen into exactly height lines of at most width cells.
func (m Model) View() string {
	var lines []string
	lines = append(lines, m.tabs())
	if m.stale {
		lines = append(lines, styleStale.Render(fit(fmt.Sprintf(" STALE: admin API unreachable (%s); showing the last state from %s ", m.lastErr, m.lastSeen()), m.width)))
	}
	body := m.body()
	footer := m.footer()
	room := m.height - len(lines) - 1
	if room < 1 {
		room = 1
	}
	if len(body) > room {
		body = body[:room]
	}
	for len(body) < room {
		body = append(body, "")
	}
	lines = append(lines, body...)
	lines = append(lines, footer)
	for i := range lines {
		lines[i] = fit(lines[i], m.width)
	}
	return strings.Join(lines, "\n")
}

func (m Model) lastSeen() string {
	if m.snap == nil {
		return "never"
	}
	return m.snap.at.UTC().Format("15:04:05") + "Z"
}

func (m Model) tabs() string {
	var parts []string
	for s := ScreenOverview; s <= ScreenConfig; s++ {
		label := fmt.Sprintf(" %d %s ", int(s), screenNames[s])
		if s == m.screen {
			parts = append(parts, styleTab.Render(label))
		} else {
			parts = append(parts, label)
		}
	}
	node := ""
	if m.snap != nil {
		node = styleDim.Render(" " + m.snap.status.NodeID + " · " + m.snap.status.Version)
	}
	return strings.Join(parts, "") + node
}

func (m Model) footer() string {
	if m.notice != "" {
		return styleTitle.Render(m.notice)
	}
	if m.help {
		return "? close help · q quit"
	}
	switch {
	case m.testing:
		return "tab next field · enter run · esc close"
	case m.filtering, m.usageFiltOn:
		return "type to filter · enter keep · esc clear"
	case m.usageEdit != 0:
		return "edit the range · tab other field · enter apply · esc apply"
	}
	switch m.screen {
	case ScreenOverview:
		return "r reload · 1-5 screens · ? help · q quit"
	case ScreenLimits:
		return "↑↓ select · enter detail · t test request · / filter · ? help · q quit"
	case ScreenUsage:
		return "g group-by · s sort · f range · / filter · enter hourly bars · x export CSV · ? help"
	case ScreenBackends:
		return "read-only · 1-5 screens · ? help · q quit"
	case ScreenConfig:
		return "r reload · v validate the file on disk · ? help · q quit"
	}
	return ""
}

func (m Model) body() []string {
	if m.help {
		return m.helpLines()
	}
	if m.snap == nil {
		if m.stale {
			return []string{"", "  No data yet: the admin API has not answered.", "  " + m.lastErr}
		}
		return []string{"", "  Connecting to the admin API…"}
	}
	switch m.screen {
	case ScreenLimits:
		return m.limitsLines()
	case ScreenUsage:
		return m.usageLines()
	case ScreenBackends:
		return m.backendsLines()
	case ScreenConfig:
		return m.configLines()
	}
	return m.overviewLines()
}

func (m Model) helpLines() []string {
	return []string{
		styleTitle.Render("Keys"),
		"  1 2 3 4 5   Overview, Limits, Usage, Backends, Config",
		"  r           reload the config file on the proxy (Overview, Config)",
		"  v           validate the file on disk with check-config (Config)",
		"  ↑ ↓ enter   select a rule and open its detail (Limits); hourly bars for a row (Usage)",
		"  t           dry-run a request against the rules (Limits)",
		"  /           filter the table (Limits, Usage)",
		"  g s f x     cycle group-by, sort, edit the hour range, export CSV (Usage)",
		"  ?           this help · q quit",
		"",
		styleDim.Render("Read-only: nothing here writes the config file. Edit it, then press r."),
		styleDim.Render(fmt.Sprintf("Polling every %s; a STALE banner means the last poll failed and the screens show the last state.", m.opts.Interval)),
	}
}

// ---- Overview

func (m Model) overviewLines() []string {
	s := m.snap.status
	state := "ready"
	if !s.Ready {
		state = "NOT READY"
	}
	last := "none"
	if s.LastReload != nil {
		if s.LastReload.OK {
			last = fmt.Sprintf("ok at %s (%d rules)", s.LastReload.At.UTC().Format("15:04:05"), s.LastReload.Rules)
		} else {
			last = "FAILED: " + s.LastReload.Error
		}
	}
	w := m.width - 24
	if w < 10 {
		w = 10
	}
	lines := make([]string, 0, 12+len(s.Backends))
	lines = append(lines,
		fmt.Sprintf("  %-14s %s", "state", state),
		fmt.Sprintf("  %-14s %s", "uptime", time.Duration(s.UptimeSeconds*float64(time.Second)).Truncate(time.Second)),
		fmt.Sprintf("  %-14s %d", "in flight", s.Inflight),
		fmt.Sprintf("  %-14s %.0f/s now, %.0f/s over 60 s, %d total", "requests", s.Rates.Requests1s, s.Rates.Requests60s, s.Requests),
		"  "+sparkline(m.history, false, w),
		fmt.Sprintf("  %-14s %.0f/s now, %.0f/s over 60 s, %d total", "throttled", s.Rates.Throttled1s, s.Rates.Throttled60s, s.Throttled),
		"  "+sparkline(m.history, true, w),
		fmt.Sprintf("  %-14s %d of %d up", "backends", s.BackendsUp, len(s.Backends)),
	)
	for i := range s.Backends {
		b := &s.Backends[i]
		st := "up"
		if !b.Up {
			st = "DOWN"
		}
		lines = append(lines, fmt.Sprintf("  %-14s %s %s, %d in flight, ttfb %.1f ms", "", b.Endpoint, st, b.Inflight, b.TTFBMeanMS))
	}
	lines = append(lines,
		fmt.Sprintf("  %-14s %d in force, %d live keys, %d denied since start", "rules", s.Rules, s.LiveKeys, s.Denied),
		fmt.Sprintf("  %-14s hour %s, %d tuples", "showback", s.ShowbackHour, s.Tuples),
		fmt.Sprintf("  %-14s generation %d, loaded %s", "config", s.Generation, s.LoadedAt.UTC().Format("2006-01-02 15:04:05")),
		fmt.Sprintf("  %-14s %s", "last reload", last),
	)
	return lines
}

// sparkline draws the last width samples of requests (or throttled) per second.
func sparkline(h []sample, throttled bool, width int) string {
	if len(h) == 0 {
		return styleDim.Render("(no samples yet)")
	}
	if len(h) > width {
		h = h[len(h)-width:]
	}
	maxV := 0.0
	vals := make([]float64, len(h))
	for i, s := range h {
		v := s.requests
		if throttled {
			v = s.throttled
		}
		vals[i] = v
		if v > maxV {
			maxV = v
		}
	}
	blocks := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	for _, v := range vals {
		if maxV == 0 {
			b.WriteRune(blocks[0])
			continue
		}
		i := int(v / maxV * float64(len(blocks)-1))
		b.WriteRune(blocks[i])
	}
	return fmt.Sprintf("%s  max %.0f/s over the last %s", b.String(), maxV, (h[len(h)-1].at.Sub(h[0].at)).Round(time.Second))
}

// ---- Limits

func (m Model) limitsLines() []string {
	rules := m.visibleRules()
	var lines []string
	if m.filtering || m.filter.Value() != "" {
		lines = append(lines, "  "+m.filter.View())
	}
	if m.testing {
		return append(lines, m.testFormLines()...)
	}
	hdr := fmt.Sprintf("  %-24s %-28s %-20s %-14s %-18s %8s %8s", "ID", "MATCH", "GROUP_BY", "RATE/BURST", "MODE", "KEYS", "DENIED")
	lines = append(lines, styleHeader.Render(hdr))
	if len(rules) == 0 {
		lines = append(lines, styleDim.Render("  (no rules)"))
	}
	for i, r := range rules {
		row := fmt.Sprintf("  %-24s %-28s %-20s %-14s %-18s %8d %8d", trunc(r.ID, 24), trunc(matchSummary(r.RuleStatus), 28), trunc(strings.Join(r.GroupBy, ","), 20),
			trunc(rateBurst(r.RuleStatus), 14), r.OnLimit, r.LiveKeys, r.Denied)
		if i == m.selected {
			row = styleSel.Render(row)
		}
		lines = append(lines, row)
	}
	if m.detail && m.selected < len(rules) {
		lines = append(lines, "", styleTitle.Render("  Top denied keys, last 60 s, for "+rules[m.selected].ID))
		n := 0
		for _, e := range m.snap.top.Top {
			if e.Rule != rules[m.selected].ID {
				continue
			}
			n++
			lines = append(lines, fmt.Sprintf("    %6d  %s  (%.0fs ago)", e.Denied, dims(e.AccessKey, e.Bucket, e.Prefix, e.SourceIP), e.LastSeconds))
		}
		if n == 0 {
			lines = append(lines, styleDim.Render("    none"))
		}
	}
	return lines
}

func matchSummary(r *limiter.RuleStatus) string {
	var parts []string
	add := func(name string, vs []string) {
		switch len(vs) {
		case 0:
		case 1:
			parts = append(parts, name+"="+vs[0])
		default:
			parts = append(parts, fmt.Sprintf("%s(%d)", name, len(vs)))
		}
	}
	add("key", r.AccessKeys)
	add("bucket", r.Buckets)
	add("prefix", r.Prefixes)
	add("cidr", r.SourceCIDRs)
	add("ops", r.Ops)
	if len(parts) == 0 {
		return "any"
	}
	return strings.Join(parts, " ")
}

func rateBurst(r *limiter.RuleStatus) string {
	if r.Rate == 0 {
		return "0 (block)"
	}
	s := strconv.FormatFloat(r.Rate, 'f', -1, 64) + "/" + strconv.Itoa(r.Burst)
	if r.EffectiveRate != r.Rate {
		s += "*"
	}
	return s
}

func (m Model) testFormLines() []string {
	labels := []string{"access key", "bucket", "key", "source ip", "operation"}
	lines := []string{styleTitle.Render("  Test a request against the rules in force (nothing is consumed)")}
	for i := range m.testFields {
		lines = append(lines, fmt.Sprintf("  %-11s %s", labels[i], m.testFields[i].View()))
	}
	switch {
	case m.testErr != "":
		lines = append(lines, "", "  error: "+m.testErr)
	case m.testResult != nil:
		r := m.testResult
		verdict := "ALLOWED"
		if !r.Result.Allowed {
			verdict = fmt.Sprintf("DENIED by %s (%s, Retry-After %ds)", r.Result.DeniedBy, r.Result.Mode, r.Result.RetryAfter)
		}
		lines = append(lines, "", fmt.Sprintf("  %s %s (%s level): %s; %d rules match", r.Operation, r.Class, r.Level, verdict, len(r.Result.Matches)))
		for _, mt := range r.Result.Matches {
			v := "allow"
			if !mt.Allowed {
				v = fmt.Sprintf("deny (%ds)", mt.RetryAfter)
			}
			lines = append(lines, fmt.Sprintf("    %-24s %-40s keys %d  %s", mt.Rule, dims(mt.AccessKey, mt.Bucket, mt.Prefix, mt.SourceIP), mt.LiveKeys, v))
		}
	}
	return lines
}

func dims(ak, b, p, ip string) string {
	var parts []string
	if ak != "" {
		parts = append(parts, "key="+ak)
	}
	if b != "" {
		parts = append(parts, "bucket="+b)
	}
	if p != "" {
		parts = append(parts, "prefix="+p)
	}
	if ip != "" {
		parts = append(parts, "ip="+ip)
	}
	if len(parts) == 0 {
		return "(shared)"
	}
	return strings.Join(parts, " ")
}

// ---- Usage

func (m Model) usageLines() []string {
	keys := usageGroupBys[m.usageGroup]
	sortName := []string{"keys", "requests", "bytes_in", "bytes_out"}[m.usageSort]
	lines := []string{
		fmt.Sprintf("  range %s → %s   group by %s   sort %s", m.usageFrom.View(), m.usageTo.View(), strings.Join(keys, ","), sortName),
	}
	if m.usageFiltOn || m.usageFilt.Value() != "" {
		lines = append(lines, "  "+m.usageFilt.View())
	}
	if m.snap.usageErr != "" {
		return append(lines, "", "  usage query failed: "+m.snap.usageErr)
	}
	rows := m.visibleUsage()
	keyW := m.width - 2 - 3*14 - 8
	if keyW < 16 {
		keyW = 16
	}
	hdr := fmt.Sprintf("  %-*s %13s %13s %13s %7s", keyW, strings.ToUpper(strings.Join(keys, " / ")), "REQUESTS", "BYTES_IN", "BYTES_OUT", "PARTIAL")
	lines = append(lines, styleHeader.Render(hdr))
	if len(rows) == 0 {
		lines = append(lines, styleDim.Render("  (no rows in range)"))
	}
	limit := m.height - len(lines) - 4
	for i, g := range rows {
		if i >= limit && limit > 0 {
			lines = append(lines, styleDim.Render(fmt.Sprintf("  … %d more rows (filter with /)", len(rows)-i)))
			break
		}
		var kv []string
		for _, k := range keys {
			v := g.Keys[k]
			if v == "" {
				v = `""`
			}
			kv = append(kv, v)
		}
		partial := ""
		if g.Partial {
			partial = "yes"
		}
		row := fmt.Sprintf("  %-*s %13d %13d %13d %7s", keyW, trunc(strings.Join(kv, " / "), keyW), g.Requests, g.BytesIn, g.BytesOut, partial)
		if i == m.usageSel {
			row = styleSel.Render(row)
		}
		lines = append(lines, row)
	}
	if u := m.snap.usage; u != nil {
		lines = append(lines, styleDim.Render(fmt.Sprintf("  %d files, %d groups, %d requests, %d bytes in, %d bytes out", u.Files, len(u.Groups), u.Requests, u.BytesIn, u.BytesOut)))
	}
	if m.bars != nil {
		lines = append(lines, "", styleTitle.Render("  Requests per hour for the selected row"))
		lines = append(lines, m.barLines()...)
	}
	return lines
}

// barLines renders one line per hour of the bars query, scaled to the width.
func (m Model) barLines() []string {
	groups := make([]showback.Group, len(m.bars.Groups))
	copy(groups, m.bars.Groups)
	sort.Slice(groups, func(i, j int) bool { return groups[i].Keys["hour_utc"] < groups[j].Keys["hour_utc"] })
	maxV := int64(1)
	for _, g := range groups {
		maxV = max(maxV, g.Requests)
	}
	w := m.width - 44
	if w < 10 {
		w = 10
	}
	var lines []string
	for _, g := range groups {
		n := int(float64(g.Requests) / float64(maxV) * float64(w))
		lines = append(lines, fmt.Sprintf("  %-20s %10d %s", g.Keys["hour_utc"], g.Requests, strings.Repeat("█", n)))
	}
	if len(lines) == 0 {
		lines = append(lines, styleDim.Render("  (no hours)"))
	}
	return lines
}

// ---- Backends

func (m Model) backendsLines() []string {
	hdr := fmt.Sprintf("  %-22s %-5s %9s %9s %7s %6s %6s %8s %6s %6s %10s %10s", "ENDPOINT", "STATE", "IN_FLIGHT", "REQUESTS", "ERRORS", "DIAL", "TLS", "TIMEOUT", "RESET", "OTHER", "TTFB_MEAN", "TTFB_LAST")
	lines := []string{styleHeader.Render(hdr)}
	for i := range m.snap.backends {
		b := &m.snap.backends[i]
		st := "up"
		if !b.Up {
			st = "DOWN"
		}
		lines = append(lines, fmt.Sprintf("  %-22s %-5s %9d %9d %7d %6d %6d %8d %6d %6d %8.1fms %8.1fms", trunc(b.Endpoint, 22), st, b.Inflight, b.Requests, b.Errors, b.ErrorsDial, b.ErrorsTLS, b.ErrorsTime, b.ErrorsRst, b.ErrorsOth, b.TTFBMeanMS, b.TTFBLastMS))
		if b.LastError != "" {
			lines = append(lines, styleDim.Render(fmt.Sprintf("    last error %s: %s", b.LastErrAt, trunc(b.LastError, m.width-40))))
		}
	}
	return lines
}

// ---- Config

func (m Model) configLines() []string {
	v := m.snap.view
	last := "none"
	if v.LastReload != nil {
		if v.LastReload.OK {
			last = fmt.Sprintf("ok at %s (%d rules, %.1f ms)", v.LastReload.At.UTC().Format(time.RFC3339), v.LastReload.Rules, v.LastReload.DurationMS)
		} else {
			last = fmt.Sprintf("FAILED at %s: %s", v.LastReload.At.UTC().Format(time.RFC3339), v.LastReload.Error)
		}
	}
	lines := []string{
		fmt.Sprintf("  generation %d, loaded %s; last reload: %s", v.Generation, v.LoadedAt.UTC().Format(time.RFC3339), last),
	}
	if m.opts.ConfigPath != "" {
		lines = append(lines, "", styleTitle.Render("  Running config versus "+m.opts.ConfigPath))
		diff := m.configDiff()
		if len(diff) == 0 {
			lines = append(lines, styleDim.Render("  no differences"))
		} else {
			lines = append(lines, styleHeader.Render(fmt.Sprintf("  %-36s %-20s %-20s", "KEY", "RUNNING", "FILE")))
			for _, d := range diff {
				lines = append(lines, fmt.Sprintf("  %-36s %-20s %-20s", trunc(d.key, 36), trunc(d.running, 20), trunc(d.file, 20)))
			}
		}
	}
	lines = append(lines, "", styleTitle.Render("  Running config (redacted)"))
	for _, kv := range flatten("", m.snap.config) {
		lines = append(lines, fmt.Sprintf("  %-40s %s", trunc(kv[0], 40), trunc(kv[1], m.width-44)))
	}
	return lines
}

// configDiff compares the running config with the file on disk, like `config diff`.
func (m Model) configDiff() []diffRow {
	if m.opts.ConfigPath == "" || m.snap == nil {
		return nil
	}
	disk, err := loadFlat(m.opts.ConfigPath)
	if err != nil {
		return []diffRow{{key: "(file)", running: "", file: firstLine(err.Error())}}
	}
	running := map[string]string{}
	for _, kv := range flatten("", m.snap.config) {
		running[kv[0]] = kv[1]
	}
	keys := map[string]bool{}
	for k := range running {
		keys[k] = true
	}
	for k := range disk {
		keys[k] = true
	}
	var sorted []string
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var out []diffRow
	for _, k := range sorted {
		r, inR := running[k]
		f, inF := disk[k]
		if inR && inF && r == f {
			continue
		}
		if !inR {
			r = "(absent)"
		}
		if !inF {
			f = "(absent)"
		}
		out = append(out, diffRow{k, r, f})
	}
	return out
}

func trunc(s string, n int) string {
	if n <= 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// fit truncates a rendered line to width cells.
func fit(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(s)
}
