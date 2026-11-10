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
	"bytes"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/charmbracelet/x/exp/teatest"
	"github.com/muesli/termenv"

	"github.com/blakegolliher/tollbooth/internal/admin"
	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/limiter"
	"github.com/blakegolliher/tollbooth/internal/s3id"
	"github.com/blakegolliher/tollbooth/internal/showback"
)

func TestMain(m *testing.M) {
	// Frames are compared as plain text: no color sequences, whatever the terminal.
	lipgloss.SetColorProfile(termenv.Ascii)
	os.Exit(m.Run())
}

const fixtureConfig = `node_id: tui-node
backend: {scheme: http, endpoints: [10.0.0.1:9000, 10.0.0.2:9000]}
showback: {dir: /tmp/x}
admin: {token: tok}
limits:
  - name: tenant-a-writes
    operation: [PutObject, UploadPart, CopyObject]
    access_key: AKIAEXAMPLE1
    bucket: training-data
    prefix: ckpt/
    limit_per_second: 2000
    burst: 4000
    per: [access_key, bucket]
  - name: list-storm
    operation: [ListObjectsV2, ListObjects]
    limit_per_second: 50
    burst: 200
    per: source_ip
    respond: 429
  - name: frozen
    access_key: AKIAREVOKED
    blocked: true
`

var fixedNow = time.Date(2026, 11, 9, 12, 30, 0, 0, time.UTC)

var agoRE = regexp.MustCompile(`\(\d+s ago\)`)

// fixture is a deterministic admin server with a showback directory of two sealed hours.
func fixture(t *testing.T) (*admin.Client, string) {
	t.Helper()
	c, err := config.Parse([]byte(fixtureConfig))
	if err != nil {
		t.Fatal(err)
	}
	m := admin.NewMetrics()
	lim := limiter.New(&m.Limiter)
	if err := lim.Apply(c); err != nil {
		t.Fatal(err)
	}
	id := s3id.RequestIdentity{AccessKey: "AKIAREVOKED", Bucket: "b", Key: "k", Op: s3id.OpGetObject, Class: s3id.ClassRead, Level: s3id.LevelObject}
	for i := 0; i < 3; i++ {
		lim.Decide(&id, lim.Now(time.Now()), true)
	}
	m.TotalRequests.Store(12345)
	m.TotalThrottled.Store(12)
	m.InflightCount.Store(4)
	dir := t.TempDir()
	hour := fixedNow.Unix() / 3600
	write := func(h int64, node string, rows string, n, reqs, in, out int) {
		p := showback.SealedPath(dir, node, h)
		_ = os.MkdirAll(filepath.Dir(p), 0o750)
		hdr := fmt.Sprintf(`{"schema_version":1,"node_id":"%s","hour_utc":%d,"rows":%d,"requests":%d,"bytes_in":%d,"bytes_out":%d,"partial":false,"recovered":false,"written_at":0}`, node, h, n, reqs, in, out)
		_ = os.WriteFile(p, []byte(hdr+"\n"+rows), 0o600)
	}
	row := func(h int64, ak, bucket, op, sc string, reqs, in, out int) string {
		return fmt.Sprintf(`{"hour_utc":%d,"node_id":"tui-node","access_key":"%s","bucket":"%s","prefix":"","operation":"%s","status_class":"%s","requests":%d,"bytes_in":%d,"bytes_out":%d}`+"\n", h, ak, bucket, op, sc, reqs, in, out)
	}
	write(hour-2, "tui-node", row(hour-2, "AKIAEXAMPLE1", "training-data", "PutObject", "2xx", 400, 40000, 0)+row(hour-2, "AKIAEXAMPLE2", "logs", "GetObject", "2xx", 100, 0, 9000), 2, 500, 40000, 9000)
	write(hour-1, "tui-node", row(hour-1, "AKIAEXAMPLE1", "training-data", "PutObject", "2xx", 250, 25000, 0)+row(hour-1, "AKIAREVOKED", "b", "GetObject", "throttled", 3, 0, 0), 2, 253, 25000, 0)
	at := fixedNow.Add(-time.Hour)
	reloads := 0
	s := &admin.Server{Metrics: m, Limiter: lim, Token: "tok", NodeID: "tui-node", Version: "v0.0.0-tui", StartedAt: at,
		Ready: func() bool { return true },
		Config: func() admin.ConfigView {
			return admin.ConfigView{Config: c.Redacted(), Generation: 2, LoadedAt: at, LastReload: &admin.ReloadResult{OK: true, At: at, Generation: 2, Rules: 3, DurationMS: 0.7}}
		},
		Reload: func() admin.ReloadResult {
			reloads++
			if reloads == 1 {
				return admin.ReloadResult{OK: true, At: at, Generation: 3, Rules: 3, DurationMS: 0.4}
			}
			return admin.ReloadResult{Error: "limits[0].limit_per_second: must be a number above 0, got -1", At: at, Generation: 3}
		},
		Backends: func() []admin.BackendStatus {
			return []admin.BackendStatus{
				{Endpoint: "10.0.0.1:9000", Up: true, Inflight: 2, Requests: 12000, TTFBMeanMS: 1.3, TTFBLastMS: 1.1},
				{Endpoint: "10.0.0.2:9000", Up: false, Requests: 345, Errors: 6, ErrorsDial: 6, LastError: "dial tcp 10.0.0.2:9000: connect: connection refused", LastErrAt: "2026-11-09T12:28:00Z"},
			}
		},
		Showback: func() (string, int64) { return "2026-11-09T12:00:00Z", 2 },
		Usage:    &admin.Usage{Dir: dir},
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	cfgPath := filepath.Join(t.TempDir(), "tollbooth.yaml")
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(fixtureConfig, "burst: 4000", "burst: 8000", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	return &admin.Client{Base: srv.URL, Token: "tok"}, cfgPath
}

// drive applies a poll synchronously (running the model's own poll command), then a sequence of
// messages, and returns the model.
func drive(t *testing.T, m Model, msgs ...tea.Msg) Model {
	t.Helper()
	var cur tea.Model = m
	run := func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		// A batch holds the poll and the tick; frame tests run neither (the first poll is run
		// explicitly below), so only direct results are applied. A bare tick (notice clearing)
		// would block until it fires, so a command gets two seconds to answer: long enough for a
		// reload or a save against the fixture under -race on a loaded runner (CI run 42703131849
		// dropped a 250 ms budget), short enough that the 4 s and 8 s notice timers do not stall.
		ch := make(chan tea.Msg, 1)
		go func() { ch <- cmd() }()
		var got tea.Msg
		select {
		case got = <-ch:
		case <-time.After(2 * time.Second):
			return
		}
		switch msg := got.(type) {
		case pollMsg, reloadMsg, testMsg, barsMsg, exportMsg, noticeMsg, editorSavedMsg, editorExternalMsg, seenMsg, formSavedMsg:
			var next tea.Cmd
			cur, next = cur.Update(msg)
			follow(&cur, next)
		}
	}
	// First poll, synchronous and unbounded: every frame depends on it.
	cur, _ = cur.Update(m.poll()())
	for _, msg := range msgs {
		var cmd tea.Cmd
		cur, cmd = cur.Update(msg)
		run(cmd)
	}
	return cur.(Model)
}

// follow runs what a message's handling asked for next, one level deep: a save-and-apply chains
// the reload, alone or in a batch beside the notice timer (which is not waited for).
func follow(cur *tea.Model, next tea.Cmd) {
	if next == nil {
		return
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- next() }()
	var got tea.Msg
	select {
	case got = <-ch:
	case <-time.After(500 * time.Millisecond):
		return
	}
	switch msg := got.(type) {
	case reloadMsg:
		*cur, _ = (*cur).Update(msg)
	case tea.BatchMsg:
		for _, c := range msg {
			follow(cur, c)
		}
	}
}

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "ctrl+r":
		return tea.KeyMsg{Type: tea.KeyCtrlR}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func size(w, h int) tea.WindowSizeMsg { return tea.WindowSizeMsg{Width: w, Height: h} }

// frame normalizes the few clock- and path-dependent cells.
func frame(m Model) []byte {
	v := m.View()
	v = strings.ReplaceAll(v, fixedNow.UTC().Format("15:04:05")+"Z", "HH:MM:SSZ")
	if m.opts.ConfigPath != "" {
		v = strings.ReplaceAll(v, m.opts.ConfigPath, "/etc/tollbooth/tollbooth.yaml")
	}
	v = agoRE.ReplaceAllString(v, "(Xs ago)") // the deny ring's age reads the real clock
	lines := strings.Split(v, "\n")
	for i, l := range lines {
		if strings.Contains(l, "uptime") {
			lines[i] = "  uptime         X"
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

// TestGoldenFrames renders every screen, the help, the Limits detail and test form, the Usage bars,
// and the admin-unreachable state at 80×24 and 120×40 (test plan category 12). -update rewrites
// testdata/*.golden.
func TestGoldenFrames(t *testing.T) {
	client, cfgPath := fixture(t)
	for _, sz := range []struct{ w, h int }{{80, 24}, {120, 40}} {
		base := New(Options{Client: client, Interval: 2 * time.Second, ConfigPath: cfgPath, Now: func() time.Time { return fixedNow }})
		cases := []struct {
			name string
			msgs []tea.Msg
		}{
			{"overview", []tea.Msg{size(sz.w, sz.h)}},
			{"help", []tea.Msg{size(sz.w, sz.h), key("?")}},
			{"limits", []tea.Msg{size(sz.w, sz.h), key("2")}},
			{"limits-selected", []tea.Msg{size(sz.w, sz.h), key("2"), key("down"), key("down")}},
			{"limits-filter", []tea.Msg{size(sz.w, sz.h), key("2"), key("/"), key("l"), key("i"), key("s"), key("t"), key("enter")}},
			{"limits-test", []tea.Msg{size(sz.w, sz.h), key("2"), key("t"), key("A"), key("K"), key("I"), key("A"), key("R"), key("E"), key("V"), key("O"), key("K"), key("E"), key("D"), key("enter")}},
			{"usage", []tea.Msg{size(sz.w, sz.h), key("3")}},
			{"usage-grouped-sorted", []tea.Msg{size(sz.w, sz.h), key("3"), key("g"), key("g"), key("s")}},
			{"usage-bars", []tea.Msg{size(sz.w, sz.h), key("3"), key("enter")}},
			{"backends", []tea.Msg{size(sz.w, sz.h), key("4")}},
			{"config", []tea.Msg{size(sz.w, sz.h), key("5")}},
			{"config-validated", []tea.Msg{size(sz.w, sz.h), key("5"), key("v")}},
			{"reloaded", []tea.Msg{size(sz.w, sz.h), key("r")}},
		}
		for _, c := range cases {
			t.Run(fmt.Sprintf("%s-%dx%d", c.name, sz.w, sz.h), func(t *testing.T) {
				m := drive(t, base, c.msgs...)
				out := frame(m)
				assertSize(t, out, sz.w, sz.h)
				golden.RequireEqual(t, out)
			})
		}
	}
}

// TestGoldenUnreachable: the admin API goes away after one good poll; every screen keeps the last
// state under the STALE banner; with no poll ever answered the screens say so.
func TestGoldenUnreachable(t *testing.T) {
	client, cfgPath := fixture(t)
	for _, sz := range []struct{ w, h int }{{80, 24}, {120, 40}} {
		base := New(Options{Client: client, Interval: 2 * time.Second, ConfigPath: cfgPath, Now: func() time.Time { return fixedNow }})
		t.Run(fmt.Sprintf("stale-%dx%d", sz.w, sz.h), func(t *testing.T) {
			m := drive(t, base, size(sz.w, sz.h))
			cur, _ := m.Update(pollMsg{err: errors.New(`admin API at http://127.0.0.1:9900: dial tcp 127.0.0.1:9900: connect: connection refused`)})
			stale := cur.(Model)
			if !stale.stale || stale.snap == nil {
				t.Fatal("a failed poll must mark the model stale and keep the snapshot")
			}
			out := frame(stale)
			assertSize(t, out, sz.w, sz.h)
			golden.RequireEqual(t, out)
		})
		t.Run(fmt.Sprintf("never-answered-%dx%d", sz.w, sz.h), func(t *testing.T) {
			fresh := New(Options{Client: &admin.Client{Base: "http://127.0.0.1:9"}, Interval: time.Second, Now: func() time.Time { return fixedNow }})
			cur, _ := fresh.Update(size(sz.w, sz.h))
			cur, _ = cur.Update(fresh.poll()())
			if !cur.(Model).stale {
				t.Fatal("expected stale")
			}
			out := frame(cur.(Model))
			assertSize(t, out, sz.w, sz.h)
			golden.RequireEqual(t, out)
		})
	}
}

func assertSize(t *testing.T, out []byte, w, h int) {
	t.Helper()
	lines := bytes.Split(out, []byte("\n"))
	if len(lines) != h {
		t.Fatalf("frame has %d lines, want %d", len(lines), h)
	}
	for i, l := range lines {
		if lipgloss.Width(string(l)) > w {
			t.Fatalf("line %d is %d cells wide, over %d: %q", i, lipgloss.Width(string(l)), w, l)
		}
	}
}

// TestKeysAndNotices covers the interactions the goldens do not: the rejected reload notice, the
// notice clearing, the export writing a CSV file, the usage range editor, filter clearing, and
// quitting.
func TestKeysAndNotices(t *testing.T) {
	client, cfgPath := fixture(t)
	wd := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old) //nolint:errcheck // test
	base := New(Options{Client: client, Interval: time.Second, ConfigPath: cfgPath, Now: func() time.Time { return fixedNow }})
	m := drive(t, base, size(100, 30), key("r"), key("r"))
	if !strings.Contains(m.notice, "REJECTED") {
		t.Fatalf("second reload must be rejected: %q", m.notice)
	}
	cur, _ := m.Update(clearNotice{})
	if cur.(Model).notice != "" {
		t.Fatal("notice not cleared")
	}
	m = drive(t, base, size(100, 30), key("3"), key("x"))
	if !strings.HasPrefix(m.notice, "exported usage-") {
		t.Fatalf("export: %q", m.notice)
	}
	files, _ := filepath.Glob(filepath.Join(wd, "usage-*.csv"))
	if len(files) != 1 {
		t.Fatalf("csv files %v", files)
	}
	b, _ := os.ReadFile(files[0])
	if !strings.HasPrefix(string(b), "access_key,requests,bytes_in,bytes_out,partial\n") {
		t.Fatalf("csv:\n%s", b)
	}
	// Range editor: f, type into from, tab to to, enter applies and re-polls.
	m = drive(t, base, size(100, 30), key("3"), key("f"), key("tab"), key("enter"))
	if m.usageEdit != 0 {
		t.Fatal("range editor still open")
	}
	// Filter then esc clears it.
	m = drive(t, base, size(100, 30), key("2"), key("/"), key("z"), key("z"), key("esc"))
	if m.filter.Value() != "" || m.filtering {
		t.Fatal("esc must clear the filter")
	}
	// A filter that matches nothing leaves an empty table and a valid frame.
	m = drive(t, base, size(100, 30), key("2"), key("/"), key("z"), key("z"), key("enter"))
	if len(m.visibleRules()) != 0 || !strings.Contains(m.View(), "(no limits; press n to add one)") {
		t.Fatalf("%s", m.View())
	}
	// q quits.
	cur, cmd := base.Update(key("q"))
	if cmd == nil {
		t.Fatal("q must quit")
	}
	_ = cur
	// Usage query error from a bad range shows on the screen without going stale.
	m = drive(t, base, size(100, 30))
	m.usageFrom.SetValue("yesterday")
	m = drive(t, m, key("3"))
	if m.stale || m.snap.usageErr == "" || !strings.Contains(m.View(), "usage query failed") {
		t.Fatalf("stale %v err %q", m.stale, m.snap.usageErr)
	}
}

// TestProgramSmoke runs the real program headlessly with teatest: it polls the fixture, renders
// the Overview, switches to Backends, and quits.
func TestProgramSmoke(t *testing.T) {
	client, cfgPath := fixture(t)
	m := New(Options{Client: client, Interval: 50 * time.Millisecond, ConfigPath: cfgPath})
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return bytes.Contains(b, []byte("tui-node")) }, teatest.WithDuration(5*time.Second))
	tm.Send(key("4"))
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool { return bytes.Contains(b, []byte("ENDPOINT")) }, teatest.WithDuration(5*time.Second))
	tm.Send(key("q"))
	tm.WaitFinished(t, teatest.WithFinalTimeout(5*time.Second))
	final := tm.FinalModel(t).(Model)
	if final.screen != ScreenBackends || final.snap == nil {
		t.Fatalf("final screen %d, snapshot nil=%v", final.screen, final.snap == nil)
	}
}
