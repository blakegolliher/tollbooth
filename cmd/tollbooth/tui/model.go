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

// Package tui is `tollbooth tui`: six screens over the admin API, polled every interval, keyboard
// only, 80×24 minimum, honoring NO_COLOR and TERM=dumb, showing the last state under a staleness
// banner when the admin API is unreachable (docs/SPEC.md, Operator surface; ADR-0003). Five are
// read-only; the Config screen's editor (ADR-0006) writes the config file only on an explicit save.
// It is the only package that imports charmbracelet (depguard).
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/blakegolliher/tollbooth/internal/admin"
	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/showback"
)

// Screen is one of the six.
type Screen int

// The screens, in key order.
const (
	ScreenOverview Screen = iota + 1
	ScreenLimits
	ScreenUsage
	ScreenBackends
	ScreenConfig
	ScreenGraphs
)

var screenNames = map[Screen]string{ScreenOverview: "Overview", ScreenLimits: "Limits", ScreenUsage: "Usage", ScreenBackends: "Backends", ScreenConfig: "Config", ScreenGraphs: "Graphs"}

// Options configure a Model.
type Options struct {
	Client     *admin.Client
	Interval   time.Duration
	ConfigPath string // the file on disk, for the Config screen's diff and `v`
	// Now is the clock; tests inject one.
	Now func() time.Time
	// Hours of usage the Usage screen shows by default.
	UsageHours int
	// History is how many polls the Graphs screen keeps (default 300: ten minutes at 2 s).
	History int
	// Editor is the external editor for `E` on the Config screen; empty means $EDITOR, then vi.
	Editor string
}

// usageGroupBys are the group-by choices `g` cycles through.
var usageGroupBys = [][]string{{"access_key"}, {"bucket"}, {"access_key", "bucket"}, {"operation"}, {"status_class"}, {"prefix"}, {"source_ip"}, {"node_id"}}

// snapshot is everything one poll returns.
type snapshot struct {
	status   admin.StatusView
	limits   admin.Limits
	top      admin.Top
	backends []admin.BackendStatus
	config   map[string]any
	view     configView
	usage    *showback.Result
	usageErr string
	metrics  map[string]float64 // the few process gauges the Graphs screen plots
	at       time.Time
}

type configView struct {
	Generation int                 `json:"generation"`
	LoadedAt   time.Time           `json:"loaded_at"`
	LastReload *admin.ReloadResult `json:"last_reload"`
}

// Messages.
type (
	pollMsg struct {
		snap *snapshot
		err  error
	}
	tickMsg   struct{}
	reloadMsg struct {
		res admin.ReloadResult
		err error
	}
	testMsg struct {
		res admin.TestResponse
		err error
	}
	barsMsg struct {
		res *showback.Result
		err error
	}
	exportMsg struct {
		path string
		err  error
	}
	noticeMsg   string
	clearNotice struct{}
)

// sample is one poll's numbers, for the Overview sparklines and the Graphs screen.
type sample struct {
	at                  time.Time
	requests, throttled float64
	inflight            float64
	ttfb                map[string]float64 // endpoint → last TTFB ms
	denied              map[string]int64   // rule → denied since start (cumulative)
	heapMiB, rssMiB     float64
	goroutines          float64
}

// Model is the Bubble Tea model.
type Model struct {
	opts   Options
	width  int
	height int
	screen Screen
	help   bool

	snap    *snapshot // last successful poll; nil before the first
	stale   bool      // the last poll failed; snap is old
	lastErr string
	history []sample // the last Options.History polls
	paused  bool     // Graphs: history frozen

	notice string // one-line message at the bottom

	// Config editor (ADR-0006)
	ed editorState

	// Limits
	filter     textinput.Model
	filtering  bool
	selected   int
	detail     bool
	testing    bool
	testFields []textinput.Model // access key, bucket, key, ip, op
	testFocus  int
	testResult *admin.TestResponse
	testErr    string

	// Usage
	usageGroup  int
	usageFrom   textinput.Model
	usageTo     textinput.Model
	usageEdit   int // 0 none, 1 from, 2 to
	usageSort   int // column: 0 keys, 1 requests, 2 bytes_in, 3 bytes_out
	usageSel    int
	bars        *showback.Result
	usageFilt   textinput.Model
	usageFiltOn bool

	// Config
	diff   []diffRow
	diffAt time.Time
}

type diffRow struct{ key, running, file string }

// New builds the model.
func New(opts Options) Model {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Interval <= 0 {
		opts.Interval = 2 * time.Second
	}
	if opts.UsageHours <= 0 {
		opts.UsageHours = 24
	}
	if opts.History <= 0 {
		opts.History = 300
	}
	m := Model{opts: opts, screen: ScreenOverview, width: 80, height: 24}
	m.filter = newInput("filter rules by id, bucket, key…", 40)
	m.usageFilt = newInput("filter rows…", 40)
	for _, ph := range []string{"access key", "bucket", "key", "source ip", "operation (e.g. GetObject)"} {
		m.testFields = append(m.testFields, newInput(ph, 32))
	}
	m.testFields[4].SetValue("GetObject")
	now := opts.Now().UTC().Truncate(time.Hour)
	m.usageFrom = newInput("from (RFC 3339)", 22)
	m.usageFrom.SetValue(now.Add(-time.Duration(opts.UsageHours) * time.Hour).Format(time.RFC3339))
	m.usageTo = newInput("to (RFC 3339)", 22)
	m.usageTo.SetValue(now.Add(time.Hour).Format(time.RFC3339))
	return m
}

func newInput(placeholder string, width int) textinput.Model {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.CharLimit = 128
	ti.Width = width
	ti.Prompt = "> "
	return ti
}

// Init starts the first poll and the tick.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.poll(), m.tick())
}

func (m Model) tick() tea.Cmd {
	return tea.Tick(m.opts.Interval, func(time.Time) tea.Msg { return tickMsg{} })
}

// poll fetches every screen's data in one command. Any failure marks the model stale and keeps
// the last snapshot.
func (m Model) poll() tea.Cmd {
	c := m.opts.Client
	from, to := m.usageFrom.Value(), m.usageTo.Value()
	groupBy := strings.Join(usageGroupBys[m.usageGroup], ",")
	now := m.opts.Now
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s := &snapshot{at: now()}
		if _, err := c.Do(ctx, http.MethodGet, "/admin/status", nil, &s.status); err != nil {
			return pollMsg{err: err}
		}
		if _, err := c.Do(ctx, http.MethodGet, "/admin/limits", nil, &s.limits); err != nil {
			return pollMsg{err: err}
		}
		if _, err := c.Do(ctx, http.MethodGet, "/admin/limits/top?n=20&window=60s", nil, &s.top); err != nil {
			return pollMsg{err: err}
		}
		var bl struct {
			Backends []admin.BackendStatus `json:"backends"`
		}
		if _, err := c.Do(ctx, http.MethodGet, "/admin/backends", nil, &bl); err != nil {
			return pollMsg{err: err}
		}
		s.backends = bl.Backends
		var cv struct {
			Config map[string]any `json:"config"`
			configView
		}
		if _, err := c.Do(ctx, http.MethodGet, "/admin/config", nil, &cv); err != nil {
			return pollMsg{err: err}
		}
		s.config, s.view = cv.Config, cv.configView
		if raw, err := c.Do(ctx, http.MethodGet, "/metrics", nil, nil); err == nil {
			s.metrics = parseGauges(raw, "go_memstats_heap_alloc_bytes", "go_goroutines", "process_resident_memory_bytes")
		}
		q := url.Values{"group_by": {groupBy}}
		if from != "" {
			q.Set("from", from)
		}
		if to != "" {
			q.Set("to", to)
		}
		var res showback.Result
		if _, err := c.Do(ctx, http.MethodGet, "/admin/usage?"+q.Encode(), nil, &res); err != nil {
			s.usageErr = err.Error() // the usage query can fail on its own (bad range) without the poll being stale
		} else {
			s.usage = &res
		}
		return pollMsg{snap: s}
	}
}

// Update handles messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m2, cmd, ok := m.updateEditor(msg); ok {
		return m2, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.ed.open {
			m.sizeEditor()
		}
		return m, nil
	case tickMsg:
		return m, tea.Batch(m.poll(), m.tick())
	case pollMsg:
		if msg.err != nil {
			m.stale, m.lastErr = true, msg.err.Error()
			return m, nil
		}
		m.stale, m.lastErr = false, ""
		m.snap = msg.snap
		if !m.paused {
			m.history = append(m.history, sampleOf(msg.snap))
			for len(m.history) > m.opts.History {
				m.history = m.history[1:]
			}
		}
		if m.selected >= len(m.visibleRules()) {
			m.selected = max(0, len(m.visibleRules())-1)
		}
		return m, nil
	case reloadMsg:
		switch {
		case msg.err != nil:
			m.notice = "reload: " + msg.err.Error()
		case msg.res.OK:
			m.notice = fmt.Sprintf("reloaded: generation %d, %d rules, %.1f ms", msg.res.Generation, msg.res.Rules, msg.res.DurationMS)
		default:
			m.notice = "reload REJECTED: " + msg.res.Error
		}
		return m, tea.Batch(m.poll(), clearAfter(8*time.Second))
	case testMsg:
		if msg.err != nil {
			m.testErr, m.testResult = msg.err.Error(), nil
		} else {
			r := msg.res
			m.testResult, m.testErr = &r, ""
		}
		return m, nil
	case barsMsg:
		if msg.err != nil {
			m.notice = "bars: " + msg.err.Error()
			return m, clearAfter(8 * time.Second)
		}
		m.bars = msg.res
		return m, nil
	case exportMsg:
		if msg.err != nil {
			m.notice = "export: " + msg.err.Error()
		} else {
			m.notice = "exported " + msg.path
		}
		return m, clearAfter(8 * time.Second)
	case noticeMsg:
		m.notice = string(msg)
		return m, clearAfter(8 * time.Second)
	case clearNotice:
		m.notice = ""
		return m, nil
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func clearAfter(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return clearNotice{} })
}

// key handles a key press: text inputs first when one is active, then the global keys, then the
// screen's own.
func (m Model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.ed.open {
		return m.keyEditor(msg)
	}
	if m.testing {
		return m.keyTestForm(msg)
	}
	if m.filtering {
		switch msg.Type {
		case tea.KeyEsc:
			m.filtering = false
			m.filter.SetValue("")
			m.filter.Blur()
			return m, nil
		case tea.KeyEnter:
			m.filtering = false
			m.filter.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(msg)
		m.selected = 0
		return m, cmd
	}
	if m.usageFiltOn {
		switch msg.Type {
		case tea.KeyEsc:
			m.usageFiltOn = false
			m.usageFilt.SetValue("")
			m.usageFilt.Blur()
			return m, nil
		case tea.KeyEnter:
			m.usageFiltOn = false
			m.usageFilt.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		m.usageFilt, cmd = m.usageFilt.Update(msg)
		return m, cmd
	}
	if m.usageEdit != 0 {
		switch msg.Type {
		case tea.KeyEsc, tea.KeyEnter:
			m.usageFrom.Blur()
			m.usageTo.Blur()
			m.usageEdit = 0
			m.bars = nil
			return m, m.poll()
		case tea.KeyTab:
			if m.usageEdit == 1 {
				m.usageFrom.Blur()
				m.usageEdit = 2
				return m, m.usageTo.Focus()
			}
			m.usageTo.Blur()
			m.usageEdit = 1
			return m, m.usageFrom.Focus()
		}
		var cmd tea.Cmd
		if m.usageEdit == 1 {
			m.usageFrom, cmd = m.usageFrom.Update(msg)
		} else {
			m.usageTo, cmd = m.usageTo.Update(msg)
		}
		return m, cmd
	}
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "?":
		m.help = !m.help
		return m, nil
	case "1", "2", "3", "4", "5", "6":
		m.screen = Screen(msg.String()[0] - '0')
		m.help = false
		m.detail = false
		return m, nil
	case "r":
		switch m.screen {
		case ScreenOverview, ScreenConfig:
			return m, m.reload()
		case ScreenGraphs:
			m.history = nil
			return m, nil
		default:
		}
	case " ":
		if m.screen == ScreenGraphs {
			m.paused = !m.paused
			return m, nil
		}
	case "esc":
		m.detail = false
		m.bars = nil
		m.help = false
		return m, nil
	}
	switch m.screen {
	case ScreenLimits:
		return m.keyLimits(msg)
	case ScreenUsage:
		return m.keyUsage(msg)
	case ScreenConfig:
		switch msg.String() {
		case "v":
			return m, m.validate()
		case "e":
			return m.openEditor()
		}
	default:
	}
	return m, nil
}

// sampleOf reduces a poll to the numbers the graphs keep.
func sampleOf(s *snapshot) sample {
	out := sample{at: s.at, requests: s.status.Rates.Requests1s, throttled: s.status.Rates.Throttled1s, inflight: float64(s.status.Inflight),
		ttfb: make(map[string]float64, len(s.status.Backends)), denied: make(map[string]int64, len(s.limits.Rules))}
	for i := range s.status.Backends {
		out.ttfb[s.status.Backends[i].Endpoint] = s.status.Backends[i].TTFBLastMS
	}
	for i := range s.limits.Rules {
		out.denied[s.limits.Rules[i].ID] = s.limits.Rules[i].Denied
	}
	out.heapMiB = s.metrics["go_memstats_heap_alloc_bytes"] / (1 << 20)
	out.rssMiB = s.metrics["process_resident_memory_bytes"] / (1 << 20)
	out.goroutines = s.metrics["go_goroutines"]
	return out
}

// parseGauges picks named series out of a Prometheus text exposition; absent names are absent.
func parseGauges(raw []byte, names ...string) map[string]float64 {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	out := map[string]float64{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		name, val, ok := strings.Cut(line, " ")
		if !ok || !want[name] {
			continue
		}
		var f float64
		if _, err := fmt.Sscanf(val, "%g", &f); err == nil {
			out[name] = f
		}
	}
	return out
}

func (m Model) keyLimits(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	rules := m.visibleRules()
	switch msg.String() {
	case "/":
		m.filtering = true
		return m, m.filter.Focus()
	case "t":
		m.testing = true
		m.testResult, m.testErr = nil, ""
		m.testFocus = 0
		for i := range m.testFields {
			m.testFields[i].Blur()
		}
		return m, m.testFields[0].Focus()
	case "enter":
		m.detail = !m.detail
		return m, nil
	case "up", "k":
		if m.selected > 0 {
			m.selected--
		}
	case "down", "j":
		if m.selected < len(rules)-1 {
			m.selected++
		}
	}
	return m, nil
}

func (m Model) keyTestForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.testing = false
		m.testFields[m.testFocus].Blur()
		return m, nil
	case tea.KeyTab, tea.KeyDown:
		m.testFields[m.testFocus].Blur()
		m.testFocus = (m.testFocus + 1) % len(m.testFields)
		return m, m.testFields[m.testFocus].Focus()
	case tea.KeyShiftTab, tea.KeyUp:
		m.testFields[m.testFocus].Blur()
		m.testFocus = (m.testFocus + len(m.testFields) - 1) % len(m.testFields)
		return m, m.testFields[m.testFocus].Focus()
	case tea.KeyEnter:
		return m, m.runTest()
	}
	var cmd tea.Cmd
	m.testFields[m.testFocus], cmd = m.testFields[m.testFocus].Update(msg)
	return m, cmd
}

func (m Model) keyUsage(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	rows := m.visibleUsage()
	switch msg.String() {
	case "g":
		m.usageGroup = (m.usageGroup + 1) % len(usageGroupBys)
		m.usageSel, m.bars = 0, nil
		return m, m.poll()
	case "s":
		m.usageSort = (m.usageSort + 1) % 4
		return m, nil
	case "/":
		m.usageFiltOn = true
		return m, m.usageFilt.Focus()
	case "f":
		m.usageEdit = 1
		return m, m.usageFrom.Focus()
	case "x":
		return m, m.export()
	case "enter":
		if len(rows) > 0 {
			return m, m.barsFor(rows[m.usageSel])
		}
	case "up", "k":
		if m.usageSel > 0 {
			m.usageSel--
		}
	case "down", "j":
		if m.usageSel < len(rows)-1 {
			m.usageSel++
		}
	}
	return m, nil
}

// visibleRules applies the Limits filter.
func (m Model) visibleRules() []limiterRule {
	if m.snap == nil {
		return nil
	}
	f := strings.ToLower(m.filter.Value())
	var out []limiterRule
	for i := range m.snap.limits.Rules {
		r := &m.snap.limits.Rules[i]
		if f != "" && !strings.Contains(strings.ToLower(r.ID+" "+strings.Join(r.Buckets, " ")+" "+strings.Join(r.AccessKeys, " ")+" "+strings.Join(r.Prefixes, " ")), f) {
			continue
		}
		out = append(out, limiterRule{r})
	}
	return out
}

// visibleUsage applies the Usage filter and sort.
func (m Model) visibleUsage() []showback.Group {
	if m.snap == nil || m.snap.usage == nil {
		return nil
	}
	f := strings.ToLower(m.usageFilt.Value())
	var out []showback.Group
	for _, g := range m.snap.usage.Groups {
		if f != "" {
			hit := false
			for _, v := range g.Keys {
				if strings.Contains(strings.ToLower(v), f) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		}
		out = append(out, g)
	}
	keys := usageGroupBys[m.usageGroup]
	sort.SliceStable(out, func(i, j int) bool {
		switch m.usageSort {
		case 1:
			return out[i].Requests > out[j].Requests
		case 2:
			return out[i].BytesIn > out[j].BytesIn
		case 3:
			return out[i].BytesOut > out[j].BytesOut
		}
		for _, k := range keys {
			if out[i].Keys[k] != out[j].Keys[k] {
				return out[i].Keys[k] < out[j].Keys[k]
			}
		}
		return false
	})
	return out
}

func (m Model) reload() tea.Cmd {
	c := m.opts.Client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var res admin.ReloadResult
		raw, err := c.Do(ctx, http.MethodPost, "/admin/reload", nil, &res)
		if ae, ok := err.(*admin.Error); ok && ae.Status == http.StatusUnprocessableEntity {
			_ = json.Unmarshal(raw, &res)
			return reloadMsg{res: res}
		}
		return reloadMsg{res: res, err: err}
	}
}

func (m Model) runTest() tea.Cmd {
	c := m.opts.Client
	req := admin.TestRequest{AccessKey: m.testFields[0].Value(), Bucket: m.testFields[1].Value(), Key: m.testFields[2].Value(), IP: m.testFields[3].Value(), Op: m.testFields[4].Value()}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var res admin.TestResponse
		_, err := c.Do(ctx, http.MethodPost, "/admin/limits/test", &req, &res)
		return testMsg{res: res, err: err}
	}
}

// barsFor queries the selected row's hourly values across the range.
func (m Model) barsFor(g showback.Group) tea.Cmd {
	c := m.opts.Client
	q := url.Values{"group_by": {"hour_utc"}}
	if v := m.usageFrom.Value(); v != "" {
		q.Set("from", v)
	}
	if v := m.usageTo.Value(); v != "" {
		q.Set("to", v)
	}
	where := make([]string, 0, len(usageGroupBys[m.usageGroup]))
	for _, k := range usageGroupBys[m.usageGroup] {
		where = append(where, k+"="+g.Keys[k])
	}
	q.Set("where", strings.Join(where, ","))
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var res showback.Result
		_, err := c.Do(ctx, http.MethodGet, "/admin/usage?"+q.Encode(), nil, &res)
		return barsMsg{res: &res, err: err}
	}
}

// export writes the current Usage view as CSV to usage-<time>.csv in the working directory.
func (m Model) export() tea.Cmd {
	rows := m.visibleUsage()
	keys := usageGroupBys[m.usageGroup]
	now := m.opts.Now()
	return func() tea.Msg {
		res := &showback.Result{GroupBy: keys, Groups: rows}
		path := fmt.Sprintf("usage-%s.csv", now.UTC().Format("20060102T150405Z"))
		if err := os.WriteFile(path, showback.CSV(res), 0o600); err != nil {
			return exportMsg{err: err}
		}
		return exportMsg{path: path}
	}
}

// validate runs check-config on the file on disk.
func (m Model) validate() tea.Cmd {
	path := m.opts.ConfigPath
	return func() tea.Msg {
		if path == "" {
			return noticeMsg("no --config given; nothing to validate")
		}
		c, err := config.Load(path)
		if err != nil {
			return noticeMsg(fmt.Sprintf("%s: INVALID: %s", path, firstLine(err.Error())))
		}
		w := c.Warnings(0)
		return noticeMsg(fmt.Sprintf("%s: ok (%d rules, %d warnings)", path, len(c.RateLimits.Rules), len(w)))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}
