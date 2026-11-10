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

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// editFixture is a config written by a person: comments above and beside rules, quoted values, an
// inline list, a blank line between rules, and a section after the rules.
const editFixture = `# the cluster's front door
node_id: a
backend: {scheme: http, endpoints: [h:1]}
showback: {dir: /d}

limits:
  # ops asked for this on 2026-11-10
  - name: list-storm
    operation: ListObjectsV2
    limit_per_second: 20        # the number ops asked for
    per: source_ip

  # tenant A
  - name: tenant-a
    operation: [GetObject, HeadObject]   # reads
    access_key: "AKIAEXAMPLE1"
    limit_per_minute: 600
    per: [access_key, bucket]
    note: 'it''s fine'

admin: {address: "127.0.0.1:9900"}   # after the rules
`

func num(f float64) *Number { n := Number(f); return &n }

// changed returns the 1-indexed line numbers where two texts differ, requiring equal line counts.
func changed(t *testing.T, a, b string) []int {
	t.Helper()
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	if len(la) != len(lb) {
		t.Fatalf("line count changed: %d → %d\n%s", len(la), len(lb), b)
	}
	var out []int
	for i := range la {
		if la[i] != lb[i] {
			out = append(out, i+1)
		}
	}
	return out
}

func rule(t *testing.T, text []byte, name string) Rule {
	t.Helper()
	rs, err := RulesIn(text)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rs {
		if rs[i].Name == name {
			return rs[i]
		}
	}
	t.Fatalf("no rule %q", name)
	return Rule{}
}

// Changing the number is one token on one line: the comment beside it, its alignment, and every
// other byte of the file stay (the case the form is opened for most).
func TestReplaceRulePatchesOnlyTheChangedValue(t *testing.T) {
	r := rule(t, []byte(editFixture), "list-storm")
	r.LimitPerSecond = num(10)
	out, err := ReplaceRule([]byte(editFixture), "list-storm", &r)
	if err != nil {
		t.Fatal(err)
	}
	if got := changed(t, editFixture, string(out)); len(got) != 1 || got[0] != 10 {
		t.Fatalf("lines changed: %v\n%s", got, out)
	}
	if line := strings.Split(string(out), "\n")[9]; line != "    limit_per_second: 10        # the number ops asked for" {
		t.Fatalf("%q", line)
	}
	if _, err := Parse(out); err != nil {
		t.Fatalf("result does not load: %v", err)
	}

	// An inline list, a double-quoted value, and a single-quoted value with an escaped quote.
	a := rule(t, out, "tenant-a")
	a.Operation = StringList{"GetObject"}
	a.AccessKey = StringList{"AKIAEXAMPLE2"}
	a.Note = "changed"
	out2, err := ReplaceRule(out, "tenant-a", &a)
	if err != nil {
		t.Fatal(err)
	}
	if got := changed(t, string(out), string(out2)); len(got) != 3 {
		t.Fatalf("lines changed: %v\n%s", got, out2)
	}
	for _, want := range []string{"    operation: GetObject   # reads\n", "    access_key: AKIAEXAMPLE2\n", "    note: changed\n", "  # tenant A\n", "admin: {address: \"127.0.0.1:9900\"}   # after the rules\n"} {
		if !strings.Contains(string(out2), want) {
			t.Errorf("result lacks %q:\n%s", want, out2)
		}
	}
	// A rename is the name's token.
	a.Name = "tenant-a-reads"
	out3, err := ReplaceRule(out2, "tenant-a", &a)
	if err != nil || len(changed(t, string(out2), string(out3))) != 1 || !strings.Contains(string(out3), "  - name: tenant-a-reads\n") {
		t.Fatalf("%v\n%s", err, out3)
	}
}

// A change of keys (a unit, a filter added, a limit turned into a block) adds and removes whole
// lines inside that rule and touches nothing else: the comment above it, the comments beside the
// values that stay, the other rule, and the rest of the file.
func TestReplaceRuleAddsAndRemovesLines(t *testing.T) {
	r := rule(t, []byte(editFixture), "list-storm")
	r.LimitPerSecond, r.LimitPerHour = nil, num(7200)
	r.Bucket = StringList{"logs", "tmp"}
	r.Respond = 429
	out, err := ReplaceRule([]byte(editFixture), "list-storm", &r)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(editFixture, `  - name: list-storm
    operation: ListObjectsV2
    limit_per_second: 20        # the number ops asked for
    per: source_ip
`, `  - name: list-storm
    operation: ListObjectsV2
    bucket: [logs, tmp]
    limit_per_hour: 7200
    per: source_ip
    respond: 429
`, 1)
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	b := rule(t, out, "tenant-a")
	b.LimitPerMinute, b.Per, b.Blocked = nil, nil, true
	out2, err := ReplaceRule(out, "tenant-a", &b)
	if err != nil || strings.Contains(string(out2), "limit_per_minute") || !strings.Contains(string(out2),
		"  # tenant A\n  - name: tenant-a\n    operation: [GetObject, HeadObject]   # reads\n    access_key: \"AKIAEXAMPLE1\"\n    blocked: true\n    note: 'it''s fine'\n") {
		t.Fatalf("%v\n%s", err, out2)
	}
	if _, err := Parse(out2); err != nil {
		t.Fatalf("result does not load: %v", err)
	}
}

func TestAddRule(t *testing.T) {
	nr := Rule{Name: "freeze-scratch", Bucket: StringList{"scratch"}, Blocked: true, Note: "INC-7: runaway job, 2026-11-10"}
	out, err := AddRule([]byte(editFixture), &nr)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(editFixture, "    note: 'it''s fine'\n", "    note: 'it''s fine'\n  - name: freeze-scratch\n    bucket: scratch\n    blocked: true\n    note: 'INC-7: runaway job, 2026-11-10'\n", 1)
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	if _, err := AddRule(out, &nr); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("a duplicate name was added: %v", err)
	}
	bad := Rule{Name: "x", Operation: StringList{"ListObjectV2"}, LimitPerSecond: num(1), Per: StringList{"total"}}
	if _, err := AddRule(out, &bad); err == nil || !strings.Contains(err.Error(), "did you mean ListObjectsV2?") {
		t.Fatalf("an invalid rule was added: %v", err)
	}

	// No `limits` key, an empty list with a comment, and an inline list: the section is written out.
	base := "node_id: a\nbackend: {scheme: http, endpoints: [h:1]}\nshowback: {dir: /d}\n"
	one := Rule{Name: "one", Operation: StringList{"listobjectsv2"}, LimitPerSecond: num(20), Per: StringList{"source_ip"}}
	for name, c := range map[string]struct{ in, want string }{
		"no key":      {base, base + "\nlimits:\n  - name: one\n    operation: ListObjectsV2\n    limit_per_second: 20\n    per: source_ip\n"},
		"no newline":  {strings.TrimSuffix(base, "\n"), base + "\nlimits:\n  - name: one\n    operation: ListObjectsV2\n    limit_per_second: 20\n    per: source_ip\n"},
		"empty list":  {base + "limits: []   # none yet\nadmin: {pprof: true}\n", base + "limits:  # none yet\n  - name: one\n    operation: ListObjectsV2\n    limit_per_second: 20\n    per: source_ip\nadmin: {pprof: true}\n"},
		"null":        {base + "limits:\nadmin: {pprof: true}\n", base + "limits:\n  - name: one\n    operation: ListObjectsV2\n    limit_per_second: 20\n    per: source_ip\nadmin: {pprof: true}\n"},
		"inline list": {base + "limits: [{name: a, blocked: true}]\n", base + "limits:\n  - name: a\n    blocked: true\n  - name: one\n    operation: ListObjectsV2\n    limit_per_second: 20\n    per: source_ip\n"},
		"dash at 0":   {base + "limits:\n- name: a\n  blocked: true\n", base + "limits:\n- name: a\n  blocked: true\n- name: one\n  operation: ListObjectsV2\n  limit_per_second: 20\n  per: source_ip\n"},
		"dash at 4":   {base + "limits:\n    - name: a\n      blocked: true\n", base + "limits:\n    - name: a\n      blocked: true\n    - name: one\n      operation: ListObjectsV2\n      limit_per_second: 20\n      per: source_ip\n"},
		"flow rule":   {base + "limits:\n  - {name: a, blocked: true}\n", base + "limits:\n  - {name: a, blocked: true}\n  - name: one\n    operation: ListObjectsV2\n    limit_per_second: 20\n    per: source_ip\n"},
		"no last \\n": {base + "limits:\n  - name: a\n    blocked: true", base + "limits:\n  - name: a\n    blocked: true\n  - name: one\n    operation: ListObjectsV2\n    limit_per_second: 20\n    per: source_ip"},
	} {
		got, err := AddRule([]byte(c.in), &one)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
		if _, err := Parse(got); err != nil {
			t.Errorf("%s: result does not load: %v", name, err)
		}
	}
}

func TestDeleteRule(t *testing.T) {
	out, err := DeleteRule([]byte(editFixture), "list-storm")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(editFixture, `  - name: list-storm
    operation: ListObjectsV2
    limit_per_second: 20        # the number ops asked for
    per: source_ip
`, "", 1)
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
	// The last rule goes by rewriting the section to an empty list; the rest of the file stays.
	out2, err := DeleteRule(out, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if rs, _ := RulesIn(out2); len(rs) != 0 || !strings.Contains(string(out2), "limits: []\n") || !strings.Contains(string(out2), "admin: {address: \"127.0.0.1:9900\"}   # after the rules\n") || !strings.HasPrefix(string(out2), "# the cluster's front door\nnode_id: a\n") {
		t.Fatalf("%s", out2)
	}
	if _, err := Parse(out2); err != nil {
		t.Fatalf("result does not load: %v", err)
	}
	if _, err := DeleteRule(out2, "tenant-a"); err == nil {
		t.Fatal("deleted a rule that is not there")
	}
	if _, err := ReplaceRule(out2, "nope", &Rule{Name: "nope", Blocked: true}); err == nil {
		t.Fatal("replaced a rule that is not there")
	}
}

// A layout the functions cannot rewrite safely is refused with ErrLayout, never guessed at; text
// that is not YAML, and a `limits` that is not a list, are plain errors.
func TestEditRefusesWhatItCannotVerify(t *testing.T) {
	base := "node_id: a\nbackend: {scheme: http, endpoints: [h:1]}\nshowback: {dir: /d}\n"
	// A rule whose list spans lines and closes on its own line: the last-line arithmetic would
	// leave the bracket behind, and the verification catches it.
	multi := base + "limits:\n  - name: a\n    operation: [\n      GetObject,\n      HeadObject\n    ]\n    limit_per_second: 1\n    per: total\n"
	r := rule(t, []byte(multi), "a")
	r.LimitPerSecond = num(2)
	out, err := ReplaceRule([]byte(multi), "a", &r)
	if err == nil {
		if got := rule(t, out, "a"); got.Rate() != 2 || len(got.Operation) != 2 {
			t.Fatalf("a multi-line list was rewritten wrongly:\n%s", out)
		}
	} else if !errors.Is(err, ErrLayout) {
		t.Fatalf("want ErrLayout or a correct edit, got %v", err)
	}
	if _, err := RulesIn([]byte("limits: [unterminated")); err == nil {
		t.Error("unparsable text accepted")
	}
	if _, err := AddRule([]byte(base+"limits: 7\n"), &Rule{Name: "x", Blocked: true}); err == nil {
		t.Error("a scalar `limits` accepted")
	}
	if _, err := AddRule([]byte("- a\n- b\n"), &Rule{Name: "x", Blocked: true}); err == nil {
		t.Error("a file that is a list accepted")
	}
	if rs, err := RulesIn(nil); err != nil || rs != nil {
		t.Errorf("empty text: %v %v", rs, err)
	}
}

// The valid fixture survives a round of edits with every other rule unchanged, and long values
// stay on one line.
func TestEditFixtureFile(t *testing.T) {
	text, err := os.ReadFile(filepath.Join("testdata", "valid", "rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := RulesIn(text)
	if err != nil || len(before) != 6 {
		t.Fatalf("%d rules, %v", len(before), err)
	}
	long := Rule{Name: "long", Operation: OperationNames()[1:40], LimitPerSecond: num(1000000), Per: StringList{"total"}, Note: strings.Repeat("why this rule exists, at length; ", 6)}
	out, err := AddRule(text, &long)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "    limit_per_second: 1000000\n") {
		t.Fatalf("the count must be written plainly:\n%s", out)
	}
	for _, name := range []string{"list-storm", "hourly-quota"} {
		r := rule(t, out, name)
		r.Respond = 429
		r.Note = "edited"
		if out, err = ReplaceRule(out, name, &r); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if !strings.Contains(string(out), "    operation: listobjectsv2          # any letter case; shown and stored in S3's spelling\n") ||
		!strings.Contains(string(out), "    access_key: 1234567890           # an access key of digits stays a string\n    bucket: \"no\"\n    limit_per_hour: 20\n    per: access_key\n    respond: 429\n    note: edited\n") {
		t.Fatalf("a line of an edited rule that did not change was rewritten:\n%s", out)
	}
	if out, err = DeleteRule(out, "revoked-key"); err != nil {
		t.Fatal(err)
	}
	after, err := RulesIn(out)
	if err != nil || len(after) != 6 {
		t.Fatalf("%d rules, %v", len(after), err)
	}
	if after[0].Sentence() != before[0].Sentence() || after[2].Sentence() != before[3].Sentence() || after[5].Name != "long" || len(after[5].Operation) != 39 {
		t.Fatalf("an untouched rule changed:\n%s", out)
	}
	if _, err := Parse(out); err != nil {
		t.Fatalf("result does not load: %v", err)
	}
}

// FuzzEditRules: on any text whose rules load, adding, changing, and deleting a rule either
// fails cleanly or yields text whose rules are exactly the intended list; nothing panics.
func FuzzEditRules(f *testing.F) {
	for _, glob := range []string{"testdata/valid/*.yaml", "testdata/invalid/rule-*.yaml"} {
		files, _ := filepath.Glob(glob)
		for _, p := range files {
			if b, err := os.ReadFile(p); err == nil {
				f.Add(b)
			}
		}
	}
	f.Add([]byte(editFixture))
	f.Add([]byte("limits: [{name: a, blocked: true}]"))
	f.Add([]byte("limits:\n- name: a\n  operation: [\n    GetObject]\n  blocked: true\n"))
	f.Add([]byte("limits:\n  - name: a\n    note: |\n      two\n      lines\n    blocked: true\n"))
	f.Fuzz(func(t *testing.T, text []byte) {
		before, err := RulesIn(text)
		if err != nil {
			return
		}
		add := Rule{Name: "zz-fuzz-added", Operation: StringList{"ListObjectsV2"}, LimitPerSecond: num(20), Per: StringList{"source_ip"}}
		if out, err := AddRule(text, &add); err == nil {
			after, rerr := RulesIn(out)
			if rerr != nil || len(after) != len(before)+1 || after[len(after)-1].Name != add.Name {
				t.Fatalf("AddRule returned text whose rules are not the old ones plus one: %v\n%s", rerr, out)
			}
		}
		if len(before) == 0 {
			return
		}
		first := before[0]
		first.Blocked, first.LimitPerSecond, first.LimitPerMinute, first.LimitPerHour, first.Burst, first.Per = true, nil, nil, nil, 0, nil
		if out, err := ReplaceRule(text, before[0].Name, &first); err == nil {
			after, rerr := RulesIn(out)
			if rerr != nil || len(after) != len(before) || !after[0].Blocked {
				t.Fatalf("ReplaceRule returned text whose first rule is not the new one: %v\n%s", rerr, out)
			}
		}
		if out, err := DeleteRule(text, before[0].Name); err == nil {
			if after, rerr := RulesIn(out); rerr != nil || len(after) != len(before)-1 {
				t.Fatalf("DeleteRule returned text with %d rules, want %d: %v\n%s", len(after), len(before)-1, rerr, out)
			}
		}
	})
}

func BenchmarkReplaceRule(b *testing.B) {
	r := Rule{Name: "list-storm", Operation: StringList{"ListObjectsV2"}, LimitPerSecond: num(10), Per: StringList{"source_ip"}}
	text := []byte(editFixture)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ReplaceRule(text, "list-storm", &r); err != nil {
			b.Fatal(err)
		}
	}
}
