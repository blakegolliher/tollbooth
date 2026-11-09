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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/golden"
)

// synthHistory is a deterministic ten-minute history: a request rate that ramps and dips, a
// throttle burst in the middle, two backends with different TTFB, denials on one rule, and a heap
// that saw-tooths like a GC cycle.
func synthHistory(n int) []sample {
	h := make([]sample, n)
	var denied int64
	for i := range h {
		t := float64(i) / float64(n-1)
		req := 400 + 300*math.Sin(t*2*math.Pi)
		thr := 0.0
		if i > n/2-10 && i < n/2+10 {
			thr = 120
			denied += 240
		}
		h[i] = sample{
			at:         fixedNow.Add(time.Duration(i-n) * 2 * time.Second),
			requests:   req,
			throttled:  thr,
			inflight:   float64(5 + i%7),
			ttfb:       map[string]float64{"10.0.0.1:9000": 1.2 + 0.3*math.Sin(t*6), "10.0.0.2:9000": 2.5},
			denied:     map[string]int64{"tenant-a-writes": denied, "list-storm": int64(i / 20), "frozen": 3},
			heapMiB:    20 + float64(i%40)*0.5,
			rssMiB:     60,
			goroutines: 40,
		}
	}
	return h
}

// TestGoldenGraphs renders the Graphs screen with a synthetic history, paused, and empty, at both
// sizes (ADR-0006).
func TestGoldenGraphs(t *testing.T) {
	client, cfgPath := fixture(t)
	for _, sz := range []struct{ w, h int }{{80, 24}, {120, 40}} {
		base := New(Options{Client: client, Interval: 2 * time.Second, ConfigPath: cfgPath, Now: func() time.Time { return fixedNow }})
		t.Run(fmt.Sprintf("graphs-%dx%d", sz.w, sz.h), func(t *testing.T) {
			m := drive(t, base, size(sz.w, sz.h), key("6"))
			m.history = synthHistory(300)
			out := frame(m)
			assertSize(t, out, sz.w, sz.h)
			golden.RequireEqual(t, out)
		})
		t.Run(fmt.Sprintf("graphs-paused-%dx%d", sz.w, sz.h), func(t *testing.T) {
			m := drive(t, base, size(sz.w, sz.h), key("6"))
			m.history = synthHistory(40)
			cur, _ := m.Update(key(" "))
			m = cur.(Model)
			if !m.paused {
				t.Fatal("space must pause")
			}
			// A poll while paused does not extend the history.
			before := len(m.history)
			m = drive(t, m)
			if len(m.history) != before {
				t.Fatalf("history grew while paused: %d → %d", before, len(m.history))
			}
			out := frame(m)
			assertSize(t, out, sz.w, sz.h)
			golden.RequireEqual(t, out)
		})
		t.Run(fmt.Sprintf("graphs-empty-%dx%d", sz.w, sz.h), func(t *testing.T) {
			m := drive(t, base, size(sz.w, sz.h), key("6"), key("r")) // r resets the history on this screen
			if len(m.history) != 0 {
				t.Fatalf("history not reset: %d", len(m.history))
			}
			out := frame(m)
			assertSize(t, out, sz.w, sz.h)
			golden.RequireEqual(t, out)
		})
	}
}

// TestGoldenEditor renders the Config editor in NORMAL mode, INSERT mode after typing, with an
// invalid buffer, and with the save/discard prompt (ADR-0006).
func TestGoldenEditor(t *testing.T) {
	client, cfgPath := fixture(t)
	for _, sz := range []struct{ w, h int }{{80, 24}, {120, 40}} {
		base := New(Options{Client: client, Interval: 2 * time.Second, ConfigPath: cfgPath, Now: func() time.Time { return fixedNow }})
		cases := []struct {
			name string
			msgs []tea.Msg
		}{
			{"editor", []tea.Msg{size(sz.w, sz.h), key("5"), key("e"), key("j"), key("j")}},
			{"editor-insert", []tea.Msg{size(sz.w, sz.h), key("5"), key("e"), key("i"), key("#"), key(" "), key("x"), key("esc")}},
			{"editor-invalid", []tea.Msg{size(sz.w, sz.h), key("5"), key("e"), key("i"), key("b"), key("o"), key("g"), key("u"), key("s"), key(":"), key(" "), key("1"), key("enter"), key("esc")}},
			{"editor-prompt", []tea.Msg{size(sz.w, sz.h), key("5"), key("e"), key("i"), key("#"), key("esc"), key("esc")}},
		}
		for _, c := range cases {
			t.Run(fmt.Sprintf("%s-%dx%d", c.name, sz.w, sz.h), func(t *testing.T) {
				m := drive(t, base, c.msgs...)
				if !m.ed.open {
					t.Fatal("editor not open")
				}
				out := frame(m)
				assertSize(t, out, sz.w, sz.h)
				golden.RequireEqual(t, out)
			})
		}
	}
}

// TestEditorSaveAndReload: ctrl+s writes the buffer atomically with a backup beside the file; a
// buffer that does not validate is not written; a file changed on disk since it was loaded is
// refused; ctrl+r saves and reloads; esc on a clean buffer closes; n discards.
func TestEditorSaveAndReload(t *testing.T) {
	client, cfgPath := fixture(t)
	base := New(Options{Client: client, Interval: time.Second, ConfigPath: cfgPath, Now: func() time.Time { return fixedNow }})
	// Type a comment at the top and save.
	m := drive(t, base, size(100, 30), key("5"), key("e"), key("i"), key("#"), key(" "), key("z"), key("enter"), key("esc"), key("ctrl+s"))
	if !strings.HasPrefix(m.notice, "saved ") || !strings.Contains(m.notice, "; backup ") {
		t.Fatalf("notice %q", m.notice)
	}
	b, err := os.ReadFile(cfgPath)
	if err != nil || !strings.HasPrefix(string(b), "# z\n") {
		t.Fatalf("file not written: %v %q", err, string(b[:min(len(b), 20)]))
	}
	backups, _ := filepath.Glob(cfgPath + ".*.bak")
	if len(backups) != 1 {
		t.Fatalf("backups %v", backups)
	}
	old, _ := os.ReadFile(backups[0])
	if strings.HasPrefix(string(old), "# z") || !strings.Contains(string(old), "tenant-a-writes") {
		t.Fatalf("backup is not the previous content")
	}
	if m.ed.dirty() {
		t.Fatal("buffer must be clean after a save")
	}
	// An invalid buffer is refused.
	m2 := drive(t, m, key("i"), key("b"), key("o"), key("g"), key("u"), key("s"), key(":"), key(" "), key("1"), key("enter"), key("esc"), key("ctrl+s"))
	if !strings.Contains(m2.notice, "not saved") {
		t.Fatalf("invalid buffer saved? %q", m2.notice)
	}
	// Changed on disk since loaded: refused with a hint.
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(cfgPath, future, future)
	m3 := drive(t, m, key("i"), key("#"), key("esc"), key("ctrl+s"))
	if !strings.Contains(m3.notice, "changed on disk") {
		t.Fatalf("stale file not refused: %q", m3.notice)
	}
	// L reloads the buffer and clears the staleness; ctrl+r then saves and reloads the proxy.
	m4 := drive(t, m3, key("L"), key("i"), key("#"), key("esc"), key("ctrl+r"))
	if !strings.Contains(m4.notice, "reloaded") && !strings.Contains(m4.notice, "REJECTED") && !strings.HasPrefix(m4.notice, "saved") {
		t.Fatalf("ctrl+r: %q", m4.notice)
	}
	// esc on a clean buffer closes the editor; a dirty one prompts, n discards.
	m5 := drive(t, m4, key("esc"))
	if m5.ed.open {
		t.Fatal("esc on a clean buffer must close the editor")
	}
	m6 := drive(t, m5, key("e"), key("i"), key("#"), key("esc"), key("esc"))
	if !m6.ed.prompt {
		t.Fatal("esc on a dirty buffer must prompt")
	}
	m7 := drive(t, m6, key("n"))
	if m7.ed.open {
		t.Fatal("n must discard and close")
	}
	// Without a config path there is nothing to edit.
	none := New(Options{Client: client, Interval: time.Second, Now: func() time.Time { return fixedNow }})
	m8 := drive(t, none, size(80, 24), key("5"), key("e"))
	if m8.ed.open || !strings.Contains(m8.notice, "nothing to edit") {
		t.Fatalf("%v %q", m8.ed.open, m8.notice)
	}
}

func TestWriteConfigRefusesWithoutFile(t *testing.T) {
	if _, _, err := writeConfig(filepath.Join(t.TempDir(), "missing.yaml"), "x", time.Time{}, fixedNow); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestParseGaugesAndSampleOf(t *testing.T) {
	raw := []byte("# HELP go_goroutines x\n# TYPE go_goroutines gauge\ngo_goroutines 29\ngo_memstats_heap_alloc_bytes 5.24288e+06\nprocess_resident_memory_bytes 31457280\ntollbooth_requests_total{a=\"b\"} 7\n")
	g := parseGauges(raw, "go_goroutines", "go_memstats_heap_alloc_bytes", "process_resident_memory_bytes", "absent")
	if g["go_goroutines"] != 29 || g["go_memstats_heap_alloc_bytes"] != 5242880 || g["process_resident_memory_bytes"] != 31457280 {
		t.Fatalf("%v", g)
	}
	if _, ok := g["absent"]; ok {
		t.Fatal("absent must stay absent")
	}
	if _, ok := g["tollbooth_requests_total{a=\"b\"}"]; ok {
		t.Fatal("unrequested series must be ignored")
	}
	client, cfgPath := fixture(t)
	m := drive(t, New(Options{Client: client, Interval: time.Second, ConfigPath: cfgPath, Now: func() time.Time { return fixedNow }}), size(80, 24))
	if len(m.history) != 1 {
		t.Fatalf("history %d", len(m.history))
	}
	s := m.history[0]
	if s.inflight != 4 || s.ttfb["10.0.0.1:9000"] != 1.1 || s.denied["frozen"] != 3 || s.goroutines <= 0 || s.heapMiB <= 0 {
		t.Fatalf("%+v", s)
	}
}

func BenchmarkGraphsView(b *testing.B) {
	m := New(Options{Client: nil, Interval: time.Second, Now: func() time.Time { return fixedNow }})
	m.width, m.height, m.screen = 120, 40, ScreenGraphs
	m.history = synthHistory(300)
	m.snap = &snapshot{}
	b.ReportAllocs()
	for b.Loop() {
		_ = m.View()
	}
}
