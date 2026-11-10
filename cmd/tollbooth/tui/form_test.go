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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/limiter"
)

// typed is a string as key presses.
func typed(s string) []tea.Msg {
	out := make([]tea.Msg, 0, len(s))
	for _, r := range s {
		out = append(out, key(string(r)))
	}
	return out
}

func seq(parts ...any) []tea.Msg {
	var out []tea.Msg
	for _, p := range parts {
		switch v := p.(type) {
		case string:
			out = append(out, key(v))
		case []tea.Msg:
			out = append(out, v...)
		case tea.Msg:
			out = append(out, v)
		}
	}
	return out
}

// withRates gives a model the per-second rates two polls a moment apart would have produced.
func withRates(m Model) Model {
	m.opRate = map[string]float64{"GetObject": 1204, "ListObjectsV2": 312, "PutObject": 96, "HeadObject": 4.2}
	m.opThrottle = map[string]float64{"ListObjectsV2": 170}
	m.ruleRate = map[string][2]float64{"tenant-a-writes": {80, 0}, "list-storm": {312, 170}, "frozen": {0, 0}}
	return m
}

func baseModel(t *testing.T) (Model, string) {
	t.Helper()
	client, cfgPath := fixture(t)
	return New(Options{Client: client, Interval: 2 * time.Second, ConfigPath: cfgPath, Now: func() time.Time { return fixedNow }}), cfgPath
}

// TestGoldenLimits renders the Limits screen with live rates, a rule that matches nothing while
// traffic flows, and no rates yet, at both sizes (ADR-0007).
func TestGoldenLimits(t *testing.T) {
	base, _ := baseModel(t)
	for _, sz := range []struct{ w, h int }{{80, 24}, {120, 40}} {
		t.Run(fmt.Sprintf("rates-%dx%d", sz.w, sz.h), func(t *testing.T) {
			m := withRates(drive(t, base, size(sz.w, sz.h), key("2"), key("down")))
			out := frame(m)
			assertSize(t, out, sz.w, sz.h)
			golden.RequireEqual(t, out)
		})
		t.Run(fmt.Sprintf("matches-nothing-%dx%d", sz.w, sz.h), func(t *testing.T) {
			m := withRates(drive(t, base, size(sz.w, sz.h), key("2")))
			// Traffic is flowing and the first rule, loaded an hour ago, has matched none of it.
			m.snap.status.Rates.Requests60s = 1600
			m.ruleRate["tenant-a-writes"] = [2]float64{0, 0}
			m.snap.limits.Rules[1].Matched = 5000 // the second rule is seeing its traffic
			if !m.quiet(&m.snap.limits.Rules[0]) || m.quiet(&m.snap.limits.Rules[2]) == (m.snap.limits.Rules[2].Matched > 0) {
				t.Fatalf("quiet: %v %v (matched %d)", m.quiet(&m.snap.limits.Rules[0]), m.quiet(&m.snap.limits.Rules[2]), m.snap.limits.Rules[2].Matched)
			}
			out := frame(m)
			assertSize(t, out, sz.w, sz.h)
			if !strings.Contains(string(out), "none ⚠") || !strings.Contains(string(out), "no request has matched this rule in the 1 h since it was loaded") {
				t.Fatalf("the rule that matches nothing is not flagged:\n%s", out)
			}
			golden.RequireEqual(t, out)
		})
	}
}

// TestGoldenForm renders the guided form in the states an operator passes through: empty with the
// busiest operations offered, a family among the suggestions, the required "counted" choice, a
// valid rule read back, the choice between a burst and an even pace, a typo answered with the right name, an access key the proxy has not
// seen, a block, an existing rule opened on its number, and the discard prompt.
func TestGoldenForm(t *testing.T) {
	base, _ := baseModel(t)
	for _, sz := range []struct{ w, h int }{{80, 24}, {120, 40}} {
		open := seq(size(sz.w, sz.h), "2", "n")
		filled := seq(open, typed("listobjectsv2"), "tab", typed("20"), "tab", "tab", "tab", "right", "right")
		cases := []struct {
			name string
			msgs []tea.Msg
		}{
			{"new", open},
			{"operation-family", seq(open, typed("List"))},
			{"counted-required", seq(open, typed("listobjectsv2"), "tab", typed("20"), "tab", "tab", "tab")},
			{"valid", filled},
			{"at-once", seq(open, typed("listobjectsv2"), "tab", typed("20"), "tab", "right", "right", "tab", "down", "down")},
			{"typo", seq(open, typed("ListObjectV2"), "tab", typed("20"))},
			{"unseen-key", seq(filled, "tab", typed("DEMO-TENANT-A"))},
			{"key-picker", seq(filled, "tab", typed("akia"), "down")},
			{"blocked", seq(open, "tab", "tab", "left", "tab", "tab", "tab", typed("scratch"))},
			{"change", seq(size(sz.w, sz.h), "2", "enter")},
			{"discard", seq(filled, "esc")},
		}
		for _, c := range cases {
			t.Run(fmt.Sprintf("%s-%dx%d", c.name, sz.w, sz.h), func(t *testing.T) {
				// The rates are set before the keys so the pickers rank by them.
				m := withRates(drive(t, base, size(sz.w, sz.h)))
				m = drive(t, m, c.msgs...)
				if !m.form.open {
					t.Fatal("the form is not open")
				}
				out := frame(m)
				assertSize(t, out, sz.w, sz.h)
				golden.RequireEqual(t, out)
			})
		}
	}
}

// The form writes what a person would have written and nothing else: a new limit is appended in
// the list's own layout, a changed number is one token, a delete removes that rule's lines; each
// save leaves a backup, and apply reloads the proxy.
func TestFormSavesThroughTheFile(t *testing.T) {
	base, cfgPath := baseModel(t)
	before, _ := os.ReadFile(cfgPath)

	// New: "too many ListObjectsV2 calls; limit them to 20 a second", counted per source IP.
	m := drive(t, base, seq(size(100, 30), "2", "n", typed("listobjectsv2"), "tab", typed("20"), "tab", "tab", "tab", "right", "right")...)
	if m.form.err != "" || m.form.r.Sentence() != "At most 20 ListObjectsV2 calls per second from each source IP. Over the limit, the client gets 503 SlowDown." {
		t.Fatalf("verdict %q, sentence %q", m.form.err, m.form.r.Sentence())
	}
	if got := m.form.inputs[fName].Value(); got != "listobjectsv2-per-source-ip" {
		t.Fatalf("derived name %q", got)
	}
	m = drive(t, m, key("ctrl+s"))
	if m.form.open || !strings.HasPrefix(m.notice, "saved limit listobjectsv2-per-source-ip in ") {
		t.Fatalf("open %v notice %q", m.form.open, m.notice)
	}
	after, _ := os.ReadFile(cfgPath)
	want := string(before) + "  - name: listobjectsv2-per-source-ip\n    operation: ListObjectsV2\n    limit_per_second: 20\n    per: source_ip\n"
	if string(after) != want {
		t.Fatalf("file after the save:\n%s\nwant:\n%s", after, want)
	}
	if baks, _ := filepath.Glob(cfgPath + ".*.bak"); len(baks) != 1 {
		t.Fatalf("backups: %v", baks)
	}

	// Change: open the first rule, replace its number, save and apply. One token changes.
	_ = os.Remove(cfgPath + "." + fixedNow.UTC().Format("20060102T150405Z") + ".bak")
	m = drive(t, m, seq("enter", "backspace", "backspace", "backspace", "backspace", typed("30"), "ctrl+r")...)
	if m.form.open || !strings.Contains(m.notice, "reloaded") {
		t.Fatalf("save and apply: open %v notice %q", m.form.open, m.notice)
	}
	changedText, _ := os.ReadFile(cfgPath)
	if got, want := string(changedText), strings.Replace(want, "limit_per_second: 2000", "limit_per_second: 30", 1); got != want {
		t.Fatalf("file after the change:\n%s\nwant:\n%s", got, want)
	}

	// The same rule's "at once", which a lowered limit usually wants lowered with it.
	_ = os.Remove(cfgPath + "." + fixedNow.UTC().Format("20060102T150405Z") + ".bak")
	m = drive(t, m, seq("enter", "tab", "tab", "backspace", "backspace", "backspace", "backspace", typed("30"), "ctrl+s")...)
	burstText, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(burstText), "    limit_per_second: 30\n    burst: 30\n") || m.form.open {
		t.Fatalf("burst not changed (open %v, notice %q):\n%s", m.form.open, m.notice, burstText)
	}

	// Delete: the third rule, after a yes.
	_ = os.Remove(cfgPath + "." + fixedNow.UTC().Format("20060102T150405Z") + ".bak")
	m = drive(t, m, key("down"), key("down"), key("d"))
	if m.confirmDelete != "frozen" || !strings.Contains(m.View(), "delete limit frozen from the file and apply?") {
		t.Fatalf("confirm %q\n%s", m.confirmDelete, m.View())
	}
	kept := drive(t, m, key("n"))
	if kept.confirmDelete != "" {
		t.Fatal("n must dismiss the prompt")
	}
	if text, _ := os.ReadFile(cfgPath); !strings.Contains(string(text), "name: frozen") {
		t.Fatal("n deleted the rule")
	}
	m = drive(t, m, key("y"))
	deleted, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(deleted), "frozen") || !strings.HasPrefix(m.notice, "deleted limit frozen") && !strings.Contains(m.notice, "REJECTED") && !strings.Contains(m.notice, "reloaded") {
		t.Fatalf("notice %q\n%s", m.notice, deleted)
	}
	if rules, err := config.RulesIn(deleted); err != nil || len(rules) != 3 {
		t.Fatalf("%d rules, %v", len(rules), err)
	}
	if _, err := config.Load(cfgPath); err != nil {
		t.Fatalf("the file no longer loads: %v", err)
	}
}

// What the form refuses: a rule that does not validate, a name already taken, and a file that
// changed on disk after the form was opened. And what it asks before dropping typed work.
func TestFormRefusalsAndPrompts(t *testing.T) {
	base, cfgPath := baseModel(t)
	before, _ := os.ReadFile(cfgPath)
	unchanged := func(why string) {
		t.Helper()
		if now, _ := os.ReadFile(cfgPath); string(now) != string(before) {
			t.Fatalf("%s: the file changed:\n%s", why, now)
		}
	}

	// "Counted" was never chosen.
	m := drive(t, base, seq(size(100, 30), "2", "n", typed("GetObject"), "tab", typed("5"), "ctrl+s")...)
	if !m.form.open || !strings.Contains(m.notice, "not saved: Counted: choose who gets their own budget") {
		t.Fatalf("open %v notice %q", m.form.open, m.notice)
	}
	unchanged("no counted choice")

	// A typo is answered with the name S3 uses.
	m = drive(t, base, seq(size(100, 30), "2", "n", typed("ListObjectV2"))...)
	if !strings.Contains(m.form.err, "did you mean ListObjectsV2?") {
		t.Fatalf("verdict %q", m.form.err)
	}
	// A few letters and enter are enough: the first suggestion is taken when what is typed is not
	// an operation yet.
	m = drive(t, base, seq(size(100, 30), "2", "n", typed("listobjectsv"), "enter")...)
	if got := m.form.inputs[fOperation].Value(); got != "ListObjectsV2" || m.form.focus != fOperation {
		t.Fatalf("accepted suggestion: %q, focus %v", got, m.form.focus)
	}
	// A comma starts another; the arrows choose further down; what is already chosen is not offered.
	m = drive(t, m, seq(typed(", getobj"), "down", "enter")...)
	if got := m.form.inputs[fOperation].Value(); got != "ListObjectsV2, GetObjectLockConfiguration" {
		t.Fatalf("second operation: %q", got)
	}
	// A complete name and enter move on; so does a family.
	m = drive(t, base, seq(size(100, 30), "2", "n", typed("Get*"), "enter")...)
	if got := m.form.inputs[fOperation].Value(); got != "Get*" || m.form.focus != fAmount {
		t.Fatalf("a family must be left as typed: %q, focus %v", got, m.form.focus)
	}
	// An access key is never completed unasked: enter keeps what was typed and moves on.
	m = drive(t, base, seq(size(100, 30), "2", "n", "tab", "tab", "tab", "tab", "tab", typed("AKIAEX"), "enter")...)
	if got := m.form.inputs[fAccessKey].Value(); got != "AKIAEX" || m.form.focus != fBucket {
		t.Fatalf("access key %q, focus %v", got, m.form.focus)
	}

	// A name that is taken.
	m = drive(t, base, seq(size(100, 30), "2", "n", typed("GetObject"), "tab", typed("5"), "tab", "tab", "tab", "right",
		"tab", "tab", "tab", "tab", "tab", "tab", typed("-x"))...)
	m.form.inputs[fName].SetValue("frozen")
	m.form.nameEdited = true
	m = drive(t, m, key("ctrl+s"))
	if !m.form.open || !strings.Contains(m.notice, "not saved: Name: a limit named frozen already exists") {
		t.Fatalf("duplicate name: open %v notice %q", m.form.open, m.notice)
	}
	unchanged("duplicate name")

	// The file changed on disk after the form opened.
	m = drive(t, base, seq(size(100, 30), "2", "n", typed("GetObject"), "tab", typed("5"), "tab", "tab", "tab", "right")...)
	future := time.Now().Add(3 * time.Second)
	if err := os.Chtimes(cfgPath, future, future); err != nil {
		t.Fatal(err)
	}
	m = drive(t, m, key("ctrl+s"))
	if !m.form.open || !strings.Contains(m.notice, "changed on disk") {
		t.Fatalf("stale file: open %v notice %q", m.form.open, m.notice)
	}
	unchanged("changed on disk")

	// esc on typed work asks first; n keeps editing, y drops it. esc on an untouched form closes.
	m = drive(t, m, key("esc"))
	if !m.form.open || !m.form.discard {
		t.Fatal("esc on a changed form must ask")
	}
	if k := drive(t, m, key("n")); !k.form.open || k.form.discard {
		t.Fatal("n must return to the form")
	}
	if y := drive(t, m, key("y")); y.form.open {
		t.Fatal("y must close the form")
	}
	if c := drive(t, base, size(100, 30), key("2"), key("n"), key("esc")); c.form.open {
		t.Fatal("esc on an untouched form must close it")
	}

	// Tab order, and a blocked rule skips the amount and "at once".
	m = drive(t, base, size(100, 30), key("2"), key("n"))
	var order []formField
	for i := 0; i < int(numFormFields); i++ {
		order = append(order, m.form.focus)
		m = drive(t, m, key("tab"))
	}
	if fmt.Sprint(order) != fmt.Sprint([]formField{fOperation, fAmount, fUnit, fBurst, fPer, fAccessKey, fBucket, fPrefix, fSource, fRespond, fName, fNote}) || m.form.focus != fOperation {
		t.Fatalf("tab order %v, then %v", order, m.form.focus)
	}
	m = drive(t, m, key("tab"), key("tab"), key("left")) // the unit, set to blocked
	if m.form.unit != unitBlocked {
		t.Fatalf("unit %d", m.form.unit)
	}
	if m = drive(t, m, key("tab")); m.form.focus != fPer {
		t.Fatalf("after the unit of a blocked rule comes %v, want the refusals listing", m.form.focus)
	}
	if m = drive(t, m, key("shift+tab"), key("shift+tab")); m.form.focus != fOperation {
		t.Fatalf("back from the unit of a blocked rule: %v", m.form.focus)
	}

	// Without a config file there is nothing to change.
	client, _ := fixture(t)
	none := New(Options{Client: client, Interval: time.Second, Now: func() time.Time { return fixedNow }})
	for _, k := range []string{"n", "enter", "d"} {
		if n := drive(t, none, size(80, 24), key("2"), key(k)); n.form.open || n.confirmDelete != "" || !strings.Contains(n.notice, "no --config given") {
			t.Fatalf("%s without a config: open %v confirm %q notice %q", k, n.form.open, n.confirmDelete, n.notice)
		}
	}
}

func TestOperationCountersAndRates(t *testing.T) {
	raw := []byte(`# HELP tollbooth_requests_total x
tollbooth_requests_total{op_class="READ",operation="GetObject",result="forwarded",status_class="2xx"} 100
tollbooth_requests_total{op_class="READ",operation="GetObject",result="throttled",status_class="throttled"} 20
tollbooth_requests_total{op_class="LIST",operation="ListObjectsV2",result="forwarded",status_class="2xx"} 7
tollbooth_requests_total{op_class="LIST",operation="ListObjectsV2",result="forwarded",status_class="4xx"} 0
tollbooth_requests_total_other{operation="X"} 9
go_goroutines 12
`)
	req, thr := parseOpCounters(raw)
	if len(req) != 2 || req["GetObject"] != 120 || req["ListObjectsV2"] != 7 || len(thr) != 1 || thr["GetObject"] != 20 {
		t.Fatalf("requests %v throttled %v", req, thr)
	}
	if labelValue(`a_operation="no",operation="yes"`, "operation") != "yes" || labelValue(`x="1"`, "operation") != "" {
		t.Fatal("labelValue must match whole label names")
	}

	base, _ := baseModel(t)
	m := drive(t, base, size(80, 24))
	prev := *m.snap
	prev.opRequests, prev.opThrottled = map[string]float64{"GetObject": 100}, map[string]float64{}
	cur := prev
	cur.at = prev.at.Add(2 * time.Second)
	cur.opRequests, cur.opThrottled = map[string]float64{"GetObject": 300, "PutObject": 10}, map[string]float64{"GetObject": 50}
	cur.limits.Rules = append(cur.limits.Rules[:0:0], prev.limits.Rules...)
	cur.limits.Rules[2].Matched += 40
	cur.limits.Rules[2].Denied += 10
	m.observe(&prev, &cur)
	if m.opRate["GetObject"] != 100 || m.opRate["PutObject"] != 5 || m.opThrottle["GetObject"] != 25 || m.ruleRate["frozen"] != [2]float64{20, 5} || m.ruleRate["list-storm"] != [2]float64{0, 0} {
		t.Fatalf("op %v throttle %v rule %v", m.opRate, m.opThrottle, m.ruleRate)
	}
	// Two polls at the same instant, and a first poll, leave the rates as they were.
	m.observe(&cur, &cur)
	m.observe(nil, &cur)
	if m.opRate["GetObject"] != 100 {
		t.Fatal("rates were recomputed without elapsed time")
	}
}

func TestWrapAndRate(t *testing.T) {
	if got := wrap("At most 20 ListObjectsV2 calls per second", 16); fmt.Sprint(got) != fmt.Sprint([]string{"At most 20", "ListObjectsV2", "calls per second"}) {
		t.Fatalf("%q", got)
	}
	if got := wrap("abcdefghijkl mn", 8); fmt.Sprint(got) != fmt.Sprint([]string{"abcdefgh", "ijkl mn"}) {
		t.Fatalf("%q", got)
	}
	for d, want := range map[time.Duration]string{4 * time.Minute: "4 min", time.Hour: "1 h", 130 * time.Minute: "2 h 10 min", 20 * time.Second: "0 min"} {
		if got := ago(d); got != want {
			t.Errorf("ago(%v) = %q, want %q", d, got, want)
		}
	}
	for r, want := range map[float64]string{0: "0", 0.44: "0.4", 4.2: "4.2", 9.96: "10.0", 312.4: "312", 1204: "1,204"} {
		if got := fmtRate(r); got != want {
			t.Errorf("fmtRate(%v) = %q, want %q", r, got, want)
		}
	}
}

func BenchmarkFormView(b *testing.B) {
	m := New(Options{Now: func() time.Time { return fixedNow }})
	m.width, m.height, m.screen = 120, 40, ScreenLimits
	m.snap = &snapshot{}
	m.form = formState{open: true, per: 1, sug: -1}
	m.form.inputs[fOperation].SetValue("ListObjectsV2")
	m.form.inputs[fAmount].SetValue("20")
	m.form.check()
	m = withRates(m)
	b.ReportAllocs()
	for b.Loop() {
		_ = m.View()
	}
}

// The busiest-operations table names a rule beside an operation only when the rule is about that
// operation, and says so when the rule sees only part of it.
func TestLimitedBy(t *testing.T) {
	m := Model{snap: &snapshot{}}
	m.snap.limits.Rules = []limiter.RuleStatus{
		{Name: "all-lists", Operation: []string{"List*"}},
		{Name: "one-key-reads", Operation: []string{"GetObject", "HeadObject"}, AccessKey: []string{"AKIAEXAMPLE1"}},
		{Name: "frozen", Bucket: []string{"frozen"}, Blocked: true},
		{Name: "everything", Limit: "1000 per second"},
	}
	for op, want := range map[string]string{
		"ListObjectsV2": "all-lists, everything",
		"GetObject":     "one-key-reads (part), everything",
		"PutObject":     "everything",
	} {
		if got := m.limitedBy(op); got != want {
			t.Errorf("limitedBy(%s) = %q, want %q", op, got, want)
		}
	}
	m.snap.limits.Rules = m.snap.limits.Rules[:3]
	if got := m.limitedBy("PutObject"); got != "" {
		t.Errorf("limitedBy(PutObject) with no rule about it = %q", got)
	}
}

// "At once" offers the two ways a limit can be kept: in a burst (the default, no `burst` key) or
// evenly spaced (strict pacing, `burst: 1`). Choosing one writes exactly that, and choosing the
// default again removes the line.
func TestFormStrictPacing(t *testing.T) {
	base, cfgPath := baseModel(t)
	before, _ := os.ReadFile(cfgPath)

	// 20 ListObjectsV2 an hour for each source IP, evenly spaced.
	m := drive(t, base, seq(size(100, 30), "2", "n", typed("listobjectsv2"), "tab", typed("20"), "tab", "right", "right", "tab")...)
	if m.form.focus != fBurst {
		t.Fatalf("focus %d, want the at-once field", m.form.focus)
	}
	choices := m.suggestions()
	if len(choices) != 2 || choices[0].label != "(•) in a burst" || choices[0].detail != "20 may arrive at once, then refill" ||
		choices[1].label != "( ) evenly spaced (strict)" || choices[1].detail != "one at a time, one every 3 minutes" {
		t.Fatalf("choices %+v", choices)
	}
	m = drive(t, m, seq("down", "down", "enter", "tab", "right", "right")...)
	if got := m.form.inputs[fBurst].Value(); got != "1" {
		t.Fatalf("at once %q after choosing evenly spaced", got)
	}
	const want = "At most 20 ListObjectsV2 calls per hour from each source IP, evenly spaced (one every 3 minutes). Over the limit, the client gets 503 SlowDown."
	if m.form.err != "" || m.form.r.Sentence() != want {
		t.Fatalf("verdict %q, sentence %q", m.form.err, m.form.r.Sentence())
	}
	if !strings.Contains(m.View(), "(evenly spaced)") {
		t.Fatalf("the row does not say the limit is evenly spaced:\n%s", m.View())
	}
	m = drive(t, m, key("ctrl+s"))
	block := "  - name: listobjectsv2-per-source-ip\n    operation: ListObjectsV2\n    limit_per_hour: 20\n    burst: 1\n    per: source_ip\n"
	if after, _ := os.ReadFile(cfgPath); string(after) != string(before)+block {
		t.Fatalf("file after the save (notice %q):\n%s", m.notice, after)
	}

	// An existing rule with a number of its own at once: strict replaces the number in place.
	saved := string(before) + block
	m = drive(t, m, seq("enter", "tab", "tab")...)
	if !m.form.open || m.form.editing != "tenant-a-writes" || m.form.focus != fBurst || m.form.inputs[fBurst].Value() != "8000" {
		t.Fatalf("open %v editing %q focus %d at once %q", m.form.open, m.form.editing, m.form.focus, m.form.inputs[fBurst].Value())
	}
	if c := m.suggestions(); c[0].label != "( ) in a burst" || c[1].label != "( ) evenly spaced (strict)" {
		t.Fatalf("a typed number is neither named choice: %+v", c)
	}
	m = drive(t, m, seq("down", "down", "enter", "ctrl+s")...)
	strict := strings.Replace(saved, "    burst: 8000\n", "    burst: 1\n", 1)
	if after, _ := os.ReadFile(cfgPath); string(after) != strict {
		t.Fatalf("file after choosing evenly spaced (notice %q):\n%s", m.notice, after)
	}

	// Back to a burst: the default is the absence of the key.
	m = drive(t, m, seq("enter", "tab", "tab")...)
	if c := m.suggestions(); c[1].label != "(•) evenly spaced (strict)" {
		t.Fatalf("the choice in effect is not marked: %+v", c)
	}
	m = drive(t, m, seq("down", "enter", "ctrl+s")...)
	relaxed := strings.Replace(saved, "    burst: 8000\n", "", 1)
	if after, _ := os.ReadFile(cfgPath); string(after) != relaxed {
		t.Fatalf("file after choosing a burst again (notice %q):\n%s", m.notice, after)
	}
}
