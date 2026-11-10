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

	"github.com/charmbracelet/lipgloss"

	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/showback"
)

// The palette (the same one as vast-bucket-policy-manager, so the two tools look related): blue
// for titles and focus, green for good, orange for warnings and INSERT, red for errors, grey for
// hints. lipgloss drops the colors under NO_COLOR and TERM=dumb; the goldens render without them.
var (
	colorBlue   = lipgloss.Color("39")
	colorGreen  = lipgloss.Color("82")
	colorOrange = lipgloss.Color("214")
	colorRed    = lipgloss.Color("196")
	colorGrey   = lipgloss.Color("240")

	styleTitle  = lipgloss.NewStyle().Bold(true).Foreground(colorBlue)
	styleTab    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(colorBlue)
	styleHeader = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(colorBlue)
	styleStale  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(colorRed)
	styleSel    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(colorBlue)
	styleDim    = lipgloss.NewStyle().Faint(true).Foreground(colorGrey)
	styleOK     = lipgloss.NewStyle().Foreground(colorGreen)
	styleWarn   = lipgloss.NewStyle().Foreground(colorOrange)
	styleErr    = lipgloss.NewStyle().Foreground(colorRed)
	badgeNormal = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(colorBlue).Padding(0, 1)
	badgeInsert = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(colorOrange).Padding(0, 1)
	badgeAlert  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(colorRed).Padding(0, 1)
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
	for s := ScreenOverview; s <= ScreenGraphs; s++ {
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
	// A question waiting for y or n outranks a notice from the last action.
	switch {
	case m.form.open && m.form.discard:
		return styleWarn.Render("discard this limit? y discard · n keep editing")
	case m.confirmDelete != "":
		return styleWarn.Render("delete limit " + m.confirmDelete + " from the file and apply? y delete · n keep")
	}
	if m.notice != "" {
		if strings.Contains(m.notice, "REJECTED") || strings.Contains(m.notice, "INVALID") || strings.Contains(m.notice, "not saved") || strings.Contains(m.notice, "changed on disk") || strings.HasPrefix(m.notice, "save: ") || strings.HasPrefix(m.notice, "reload: ") {
			return styleErr.Render(m.notice)
		}
		return styleOK.Render(m.notice)
	}
	if m.help {
		return "? close help · q quit"
	}
	switch {
	case m.form.open:
		return "tab next · arrows pick · enter accept · ctrl+r apply · ctrl+s save · esc cancel"
	case m.ed.open && m.ed.prompt:
		return "unsaved changes: y save · n discard · esc keep editing"
	case m.ed.open && m.ed.insert:
		return "INSERT · esc normal mode"
	case m.ed.open:
		return "NORMAL · i insert · ctrl+s save · ctrl+r save and reload · E $EDITOR · L reload buffer · esc close"
	case m.testing:
		return "tab next field · enter run · esc close"
	case m.filtering, m.usageFiltOn:
		return "type to filter · enter keep · esc clear"
	case m.usageEdit != 0:
		return "edit the range · tab other field · enter apply · esc apply"
	}
	switch m.screen {
	case ScreenOverview:
		return "r reload · tab or 1-6 screens · ? help · q quit"
	case ScreenLimits:
		return "↑↓ select · enter change · n new · d delete · t test · / filter · ? help"
	case ScreenUsage:
		return "g group-by · s sort · f range · / filter · enter hourly bars · x export CSV · ? help"
	case ScreenBackends:
		return "read-only · tab or 1-6 screens · ? help · q quit"
	case ScreenConfig:
		return "e edit the file · r reload · v validate the file on disk · ? help · q quit"
	case ScreenGraphs:
		return "space pause · r reset history · tab or 1-6 screens · ? help · q quit"
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
		if m.form.open {
			return m.formLines()
		}
		return m.limitsLines()
	case ScreenUsage:
		return m.usageLines()
	case ScreenBackends:
		return m.backendsLines()
	case ScreenConfig:
		if m.ed.open {
			return m.editorLines()
		}
		return m.configLines()
	case ScreenGraphs:
		return m.graphsLines()
	}
	return m.overviewLines()
}

func (m Model) helpLines() []string {
	return []string{
		styleTitle.Render("Keys"),
		"  1 … 6       Overview, Limits, Usage, Backends, Config, Graphs; tab / → next, shift+tab / ← previous",
		"  r           reload the config file on the proxy (Overview, Config); reset the history (Graphs)",
		"  n enter d   add a limit, change the selected one, delete it: a guided form that reads the rule",
		"              back as a sentence; ctrl+r saves and applies, ctrl+s saves only (Limits)",
		"  t /         dry-run a request against the rules; filter the table (Limits)",
		"  e v         edit the file on disk (i insert, esc normal, ctrl+s save, ctrl+r save and reload,",
		"              E $EDITOR); validate it with check-config (Config)",
		"  g s f x     cycle group-by, sort, edit the hour range, export CSV; enter: hourly bars (Usage)",
		"  space       pause the graphs (Graphs)",
		"  ?           this help · q quit",
		"",
		styleDim.Render("The config file is written only by an explicit save in the form or the editor, with a .bak beside it."),
		styleDim.Render(fmt.Sprintf("Polling every %s; a STALE banner means the last poll failed and the screens show the last state.", m.opts.Interval)),
	}
}

// ---- Overview

func (m Model) overviewLines() []string {
	s := m.snap.status
	state := styleOK.Render("ready")
	if !s.Ready {
		state = styleErr.Render("NOT READY")
	}
	last := styleDim.Render("none")
	if s.LastReload != nil {
		if s.LastReload.OK {
			last = styleOK.Render(fmt.Sprintf("ok at %s (%d rules)", s.LastReload.At.UTC().Format("15:04:05"), s.LastReload.Rules))
		} else {
			last = styleErr.Render("FAILED: " + s.LastReload.Error)
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
		st := styleOK.Render("up")
		if !b.Up {
			st = styleErr.Render("DOWN")
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

// ---- Limits: the rules table and the busiest operations are in limits.go, the form in form.go.

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
			verdict = fmt.Sprintf("DENIED by %s (%s, Retry-After %ds)", r.Result.DeniedBy, config.ModeText(r.Result.Mode), r.Result.RetryAfter)
		}
		matched := fmt.Sprintf("%d rules match", len(r.Result.Matches))
		switch len(r.Result.Matches) {
		case 0:
			matched = "no rule matches"
		case 1:
			matched = "1 rule matches"
		}
		lines = append(lines, "", fmt.Sprintf("  %s (%s level): %s; %s", r.Operation, r.Level, verdict, matched))
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
		st := styleOK.Render("up   ")
		if !b.Up {
			st = styleErr.Render("DOWN ")
		}
		lines = append(lines, fmt.Sprintf("  %-22s %s %9d %9d %7d %6d %6d %8d %6d %6d %8.1fms %8.1fms", trunc(b.Endpoint, 22), st, b.Inflight, b.Requests, b.Errors, b.ErrorsDial, b.ErrorsTLS, b.ErrorsTime, b.ErrorsRst, b.ErrorsOth, b.TTFBMeanMS, b.TTFBLastMS))
		if b.ResolverFailingSince != "" {
			// ADR-0008: the name does not resolve; the endpoint is reached at what it last resolved to.
			how := "no address is known for it"
			if len(b.Addresses) > 0 {
				how = "reached at its last known address " + strings.Join(b.Addresses, ", ")
			}
			lines = append(lines,
				styleWarn.Render(fit(fmt.Sprintf("    ⚠ name not resolving since %s (%d failed lookups)", b.ResolverFailingSince, b.ErrorsResolve), m.width)),
				styleWarn.Render(fit("      "+how, m.width)))
		}
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
	for _, kv := range config.Flatten(m.snap.config) {
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
	for _, kv := range config.Flatten(m.snap.config) {
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
