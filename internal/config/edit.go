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
	"bytes"
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v4"
)

// Editing rules in the config file's text (ADR-0007): AddRule, ReplaceRule, and DeleteRule take
// the file as the operator wrote it and return it with one rule added, changed, or removed, and
// every other byte as it was: comments, blank lines, key order, the other rules. They are pure
// text-to-text functions; the TUI's form calls them and writes the result through ADR-0006's save.
//
// How: the YAML parser gives each node's line and column, so a rule is the run of lines from its
// dash to its last value. A change of values is patched into those lines token by token (the
// number after `limit_per_second:` and nothing else), a key that goes loses its line, a key that
// comes gets one; only a rule that is not written one key per line is re-rendered whole; a new
// rule is appended after the last one. Whatever the route, the result is parsed
// again and compared with the intended list of rules, and anything else is an error: a layout the
// functions do not understand is refused, never guessed at.

// ErrLayout is returned when the `limits` section is written in a way these functions do not
// rewrite safely; the file is still editable by hand.
var ErrLayout = errors.New("the limits section is laid out in a way the form does not rewrite; edit the file in the Config editor instead")

// RulesIn returns the rules in a config file's text, in order, without loading the rest of it.
func RulesIn(text []byte) ([]Rule, error) {
	f, err := locate(text)
	if err != nil {
		return nil, err
	}
	return f.rules()
}

// AddRule appends r to the `limits` list.
func AddRule(text []byte, r *Rule) ([]byte, error) {
	r = canonical(r)
	f, err := locate(text)
	if err != nil {
		return nil, err
	}
	old, err := f.rules()
	if err != nil {
		return nil, err
	}
	for i := range old {
		if old[i].Name == r.Name {
			return nil, fmt.Errorf("a rule named %q already exists", r.Name)
		}
	}
	want := append(append([]Rule(nil), old...), *r)
	var out []byte
	if items := f.blockItems(); len(items) > 0 {
		last := items[len(items)-1]
		block, rerr := renderItem(r, last.dash)
		if rerr != nil {
			return nil, rerr
		}
		if out, err = f.splice(last.end, last.end, block); err != nil {
			return nil, err
		}
	} else if out, err = f.rewriteAll(want); err != nil {
		return nil, err
	}
	return verify(out, want)
}

// ReplaceRule changes the rule called name into r (which may carry a new name).
func ReplaceRule(text []byte, name string, r *Rule) ([]byte, error) {
	r = canonical(r)
	f, err := locate(text)
	if err != nil {
		return nil, err
	}
	old, err := f.rules()
	if err != nil {
		return nil, err
	}
	idx := -1
	for i := range old {
		switch {
		case old[i].Name == name && idx < 0:
			idx = i
		case old[i].Name == r.Name && r.Name != name:
			return nil, fmt.Errorf("a rule named %q already exists", r.Name)
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("no rule named %q in the file", name)
	}
	want := append([]Rule(nil), old...)
	want[idx] = *r
	items := f.blockItems()
	if len(items) != len(old) {
		out, rerr := f.rewriteAll(want)
		if rerr != nil {
			return nil, rerr
		}
		return verify(out, want)
	}
	it := items[idx]
	if out, ok := f.patchItem(it, &old[idx], r); ok {
		if res, verr := verify(out, want); verr == nil {
			return res, nil
		}
	}
	block, err := renderItem(r, it.dash)
	if err != nil {
		return nil, err
	}
	out, err := f.splice(it.start-1, it.end, block)
	if err != nil {
		return nil, err
	}
	return verify(out, want)
}

// DeleteRule removes the rule called name.
func DeleteRule(text []byte, name string) ([]byte, error) {
	f, err := locate(text)
	if err != nil {
		return nil, err
	}
	old, err := f.rules()
	if err != nil {
		return nil, err
	}
	idx := -1
	for i := range old {
		if old[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("no rule named %q in the file", name)
	}
	want := append(append([]Rule(nil), old[:idx]...), old[idx+1:]...)
	items := f.blockItems()
	var out []byte
	if len(items) == len(old) && len(old) > 1 {
		if out, err = f.splice(items[idx].start-1, items[idx].end, nil); err != nil {
			return nil, err
		}
	} else if out, err = f.rewriteAll(want); err != nil {
		return nil, err
	}
	return verify(out, want)
}

// canonical returns a copy of r with its operation names in S3's spelling.
func canonical(r *Rule) *Rule {
	c := *r
	c.canonicalize()
	return &c
}

// file is a config text with its `limits` section located.
type file struct {
	lines []string   // the text split on "\n"; the last element is "" when the text ends in a newline
	key   *yaml.Node // the `limits` key, nil when the file has none
	seq   *yaml.Node // its value
}

// item is one rule's place in the text: lines start..end (1-indexed, inclusive) and the column of
// its dash (0-indexed).
type item struct {
	node       *yaml.Node
	start, end int
	dash       int
}

func locate(text []byte) (*file, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(text, &root); err != nil {
		return nil, fmt.Errorf("the file does not parse: %w", err)
	}
	if oddLineBreaks(text) {
		return nil, ErrLayout
	}
	f := &file{lines: strings.Split(string(text), "\n")}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return f, nil // an empty document: no limits
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil, errors.New("the file is not a mapping of keys")
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "limits" {
			f.key, f.seq = top.Content[i], top.Content[i+1]
		}
	}
	return f, nil
}

// oddLineBreaks reports a text with a line break other than "\n" or "\r\n". YAML also ends a line
// at a lone carriage return and at U+0085, U+2028, and U+2029; the parser's line numbers then
// disagree with a split on "\n", and every position these functions use would be wrong.
func oddLineBreaks(text []byte) bool {
	for i, c := range text {
		if c == '\r' && (i+1 == len(text) || text[i+1] != '\n') {
			return true
		}
	}
	return bytes.ContainsAny(text, "\u0085\u2028\u2029")
}

func (f *file) rules() ([]Rule, error) {
	if f.seq == nil || (f.seq.Kind == yaml.ScalarNode && f.seq.Tag == "!!null") {
		return nil, nil
	}
	if f.seq.Kind != yaml.SequenceNode {
		return nil, errors.New("limits: must be a list of rules")
	}
	var out []Rule
	if err := f.seq.Decode(&out); err != nil {
		return nil, fmt.Errorf("limits: %w", err)
	}
	for i := range out {
		out[i].canonicalize()
	}
	return out, nil
}

// blockItems returns the rules' places when the list is written one rule per dash in block style
// with every rule starting on its own line; otherwise nil, and the caller rewrites the section.
func (f *file) blockItems() []item {
	if f.seq == nil || f.seq.Kind != yaml.SequenceNode || f.seq.Style&yaml.FlowStyle != 0 {
		return nil
	}
	out := make([]item, 0, len(f.seq.Content))
	prevEnd := 0
	for _, n := range f.seq.Content {
		if n.Kind != yaml.MappingNode || n.Line < 1 || n.Line > len(f.lines) {
			return nil
		}
		line := f.lines[n.Line-1]
		dash := strings.Index(line, "-")
		if dash < 0 || strings.TrimSpace(line[:dash]) != "" || n.Line <= prevEnd {
			return nil
		}
		it := item{node: n, start: n.Line, end: lastLine(n), dash: dash}
		if it.end > len(f.lines) {
			return nil
		}
		out = append(out, it)
		prevEnd = it.end
	}
	return out
}

// lastLine is the last line a node's own text occupies.
func lastLine(n *yaml.Node) int {
	m := n.Line
	if n.Kind == yaml.ScalarNode && n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		// The header line, then one line per line of content. A block scalar with no content
		// (`note: |` at the end of the file) is its header alone.
		if body := strings.TrimRight(n.Value, "\n"); body != "" {
			m += strings.Count(body, "\n") + 1
		}
	}
	for _, c := range n.Content {
		if l := lastLine(c); l > m {
			m = l
		}
	}
	return m
}

// splice returns the text with lines (after, upTo] replaced by block: splice(n, n, b) inserts b
// after line n, splice(a-1, b, nil) deletes lines a..b. A range that is not inside the text means
// the parser's positions and the text's lines disagree, and nothing is spliced.
func (f *file) splice(after, upTo int, block []string) ([]byte, error) {
	if after < 0 || upTo < after || upTo > len(f.lines) {
		return nil, ErrLayout
	}
	out := make([]string, 0, len(f.lines)+len(block))
	out = append(out, f.lines[:after]...)
	out = append(out, block...)
	out = append(out, f.lines[upTo:]...)
	return []byte(strings.Join(out, "\n")), nil
}

// rewriteAll replaces the whole `limits` section (or appends one) with the rules in canonical
// form. It is the route for an empty list, an inline list, and a missing key.
func (f *file) rewriteAll(rules []Rule) ([]byte, error) {
	block := []string{"limits: []"}
	if len(rules) > 0 {
		block = []string{"limits:"}
		for i := range rules {
			b, err := renderItem(&rules[i], 2)
			if err != nil {
				return nil, err
			}
			block = append(block, b...)
		}
	}
	if f.key == nil {
		lines := f.lines
		if n := len(lines); n > 0 && lines[n-1] == "" {
			lines = lines[:n-1] // the text ended in a newline
		}
		out := append(append([]string(nil), lines...), "")
		out = append(out, block...)
		return []byte(strings.Join(out, "\n") + "\n"), nil
	}
	if f.key.Column != 1 || f.key.Line < 1 || f.key.Line > len(f.lines) {
		return nil, ErrLayout
	}
	end := lastLine(f.seq)
	if end < f.key.Line {
		end = f.key.Line
	}
	// A comment after `limits: []` on the key's line stays with the key.
	if i := strings.Index(f.lines[f.key.Line-1], " #"); i >= 0 && f.seq.Line == f.key.Line {
		block[0] += "  " + strings.TrimSpace(f.lines[f.key.Line-1][i:])
	}
	return f.splice(f.key.Line-1, end, block)
}

// renderItem writes a rule as a block list item whose dash sits at column dash.
func renderItem(r *Rule, dash int) ([]string, error) {
	pairs, err := renderPairs(r)
	if err != nil {
		return nil, err
	}
	pad := strings.Repeat(" ", dash)
	out := make([]string, 0, len(pairs))
	for i, p := range pairs {
		lead := pad + "  "
		if i == 0 {
			lead = pad + "- "
		}
		out = append(out, lead+p[0]+": "+p[1])
	}
	return out, nil
}

// renderPairs is a rule as ordered (key, value) pairs in YAML's own spelling and quoting, one line
// each: the encoder writes the mapping and this splits it.
func renderPairs(r *Rule) ([][2]string, error) {
	b, err := yaml.Marshal(r)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	out := make([][2]string, 0, len(lines))
	for _, l := range lines {
		k, v, ok := strings.Cut(l, ": ")
		if !ok || strings.HasPrefix(l, " ") || strings.HasPrefix(l, "-") || v == "" {
			return nil, fmt.Errorf("rule %q has a value that does not fit on one line", r.Name)
		}
		out = append(out, [2]string{k, v})
	}
	return out, nil
}

// patchItem changes a rule in place, line by line: a value that differs is rewritten as a token on
// its line, a key the new rule no longer has loses its line, and a new key gets a line of its own
// after the key that precedes it in the canonical order. Everything else in the rule, comments
// beside values included, is left as written. ok is false when the rule is not laid out one key
// per line in block style, and the caller re-renders the rule instead.
func (f *file) patchItem(it item, old, want *Rule) ([]byte, bool) {
	if it.node.Style&yaml.FlowStyle != 0 {
		return nil, false
	}
	oldPairs, err1 := renderPairs(old)
	newPairs, err2 := renderPairs(want)
	if err1 != nil || err2 != nil {
		return nil, false
	}
	was := make(map[string]string, len(oldPairs))
	for _, p := range oldPairs {
		was[p[0]] = p[1]
	}
	// Where each key of the rule sits; every pair must own exactly one line.
	type place struct {
		k, v *yaml.Node
	}
	at := map[string]place{}
	owner := map[int]string{}
	for i := 0; i+1 < len(it.node.Content); i += 2 {
		k, v := it.node.Content[i], it.node.Content[i+1]
		if v.Line != k.Line || lastLine(v) != k.Line || k.Line < 1 || k.Line > len(f.lines) {
			return nil, false
		}
		if _, taken := owner[k.Line]; taken {
			return nil, false
		}
		at[k.Value], owner[k.Line] = place{k, v}, k.Value
	}
	if len(at) != len(oldPairs) {
		return nil, false
	}
	lines := append([]string(nil), f.lines...)
	drop := map[int]bool{}
	insert := map[int][]string{} // after this line
	keyPad := strings.Repeat(" ", it.dash+2)
	anchor := 0 // the line of the last key seen that the file already has
	has := map[string]bool{}
	for _, p := range newPairs {
		has[p[0]] = true
		pl, existed := at[p[0]]
		if !existed {
			if anchor == 0 {
				return nil, false // nothing to hang the new key under
			}
			insert[anchor] = append(insert[anchor], keyPad+p[0]+": "+p[1])
			continue
		}
		anchor = max(anchor, pl.k.Line)
		if was[p[0]] == p[1] {
			continue
		}
		line := lines[pl.v.Line-1]
		start, end, ok := valueSpan(line, pl.v)
		if !ok {
			return nil, false
		}
		lines[pl.v.Line-1] = line[:start] + p[1] + line[end:]
	}
	for key, pl := range at {
		if has[key] {
			continue
		}
		if pl.k.Line == it.start {
			return nil, false // the dash's own line cannot go
		}
		drop[pl.k.Line] = true
	}
	out := make([]string, 0, len(lines)+len(insert))
	for i, l := range lines {
		if !drop[i+1] {
			out = append(out, l)
		}
		out = append(out, insert[i+1]...)
	}
	return []byte(strings.Join(out, "\n")), true
}

// valueSpan finds the bytes [start, end) of a value node's own text on its line: a plain scalar up
// to a trailing comment, a quoted scalar to its closing quote, an inline list to its bracket.
func valueSpan(line string, v *yaml.Node) (start, end int, ok bool) {
	start = v.Column - 1
	if start < 0 || start >= len(line) {
		return 0, 0, false
	}
	for i := 0; i < start; i++ {
		if line[i] >= 0x80 {
			return 0, 0, false // columns count characters; keep to lines whose lead is ASCII
		}
	}
	switch {
	case v.Kind == yaml.SequenceNode && v.Style&yaml.FlowStyle != 0 && line[start] == '[':
		depth, quote := 0, byte(0)
		for i := start; i < len(line); i++ {
			c := line[i]
			switch {
			case quote != 0:
				if c == quote {
					quote = 0
				} else if c == '\\' && quote == '"' {
					i++
				}
			case c == '"' || c == '\'':
				quote = c
			case c == '[':
				depth++
			case c == ']':
				depth--
				if depth == 0 {
					return start, i + 1, lastLine(v) == v.Line
				}
			}
		}
		return 0, 0, false
	case v.Kind != yaml.ScalarNode:
		return 0, 0, false
	case v.Style&yaml.DoubleQuotedStyle != 0 && line[start] == '"':
		for i := start + 1; i < len(line); i++ {
			switch line[i] {
			case '\\':
				i++
			case '"':
				return start, i + 1, true
			}
		}
		return 0, 0, false
	case v.Style&yaml.SingleQuotedStyle != 0 && line[start] == '\'':
		for i := start + 1; i < len(line); i++ {
			if line[i] == '\'' {
				if i+1 < len(line) && line[i+1] == '\'' {
					i++
					continue
				}
				return start, i + 1, true
			}
		}
		return 0, 0, false
	case v.Style == 0:
		end = len(line)
		if i := strings.Index(line[start:], " #"); i >= 0 {
			end = start + i
		}
		for end > start && (line[end-1] == ' ' || line[end-1] == '\t' || line[end-1] == '\r') {
			end--
		}
		return start, end, end > start
	}
	return 0, 0, false
}

// verify parses the edited text and requires its rules to be exactly want, each valid.
func verify(out []byte, want []Rule) ([]byte, error) {
	got, err := RulesIn(out)
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrLayout, err)
	}
	if len(got) != len(want) {
		return nil, ErrLayout
	}
	for i := range want {
		a, err1 := yaml.Marshal(&got[i])
		w := want[i]
		w.canonicalize()
		b, err2 := yaml.Marshal(&w)
		if err1 != nil || err2 != nil || !bytes.Equal(a, b) {
			return nil, ErrLayout
		}
		if cerr := CheckRule(&got[i]); cerr != nil {
			return nil, fmt.Errorf("rule %q: %w", got[i].Name, cerr)
		}
	}
	return out, nil
}
