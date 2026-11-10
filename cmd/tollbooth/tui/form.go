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
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/showback"
)

// The guided form (ADR-0007): the Limits screen's `n` (new), `enter` (change), and `d` (delete).
// It asks the four questions a limit answers, in the order an operator is asked them: which
// requests, how many, counted how, and what the client gets over the limit. Operations, access
// keys, and buckets are picked from what the proxy is seeing; the rule is read back as a sentence
// on every keystroke; and a save goes through internal/config's text edits and ADR-0006's write
// (validated, atomic, backed up, refused if the file changed on disk), so what the form writes is
// what a person would have written and nothing else in the file moves.

type formField int

// The fields, in tab order.
const (
	fOperation formField = iota
	fAmount
	fUnit
	fBurst
	fPer
	fAccessKey
	fBucket
	fPrefix
	fSource
	fRespond
	fName
	fNote
	numFormFields
)

// The unit choices; the last one turns the rule into a block.
const (
	unitSecond = iota
	unitMinute
	unitHour
	unitBlocked
)

var unitLabels = []string{"per second", "per minute", "per hour", "blocked"}

var respondLabels = []string{"503 SlowDown (what S3 itself answers)", "429 TooManyRequests"}

// seen is a value the proxy has recorded in showback lately, with how many requests carried it.
type seen struct {
	value    string
	requests int64
}

// suggestion is one row of a picker.
type suggestion struct{ value, detail string }

// perChoice is one answer to "counted how".
type perChoice struct {
	label string
	dims  []string // nil: write no `per` (a blocked rule that lists its refusals together)
	help  string
}

type formState struct {
	open    bool
	editing string    // the name of the rule being changed; "" for a new one
	text    string    // the config file as loaded
	mod     time.Time // its modification time then
	others  []string  // the names of the other rules in the file

	inputs    [numFormFields]textinput.Model // the text fields; the choice fields leave theirs unused
	unit      int
	per       int      // index into perChoices; -1 until the operator chooses
	customPer []string // a `per` from the file that the choices do not list
	respond   int

	focus      formField
	sug        int  // highlighted suggestion; -1 = none
	nameEdited bool // the operator typed a name, so it is no longer derived
	dirty      bool
	discard    bool // the "discard?" prompt is up

	keys, buckets []seen // seen in showback lately, busiest first

	// The verdict, recomputed on every key: the rule as the form stands and what is wrong with it.
	r   config.Rule
	err string
}

// Messages.
type (
	formSavedMsg struct {
		name    string
		backup  string
		deleted bool
		reload  bool
		err     error
	}
	seenMsg struct{ keys, buckets []seen }
)

// isText reports whether a field is typed into rather than chosen.
func (f formField) isText() bool { return f != fUnit && f != fPer && f != fRespond }

// isPicker reports whether a text field offers suggestions.
func (f formField) isPicker() bool { return f == fOperation || f == fAccessKey || f == fBucket }

// openForm opens the form on a rule in the file (name) or on a new one ("").
func (m Model) openForm(name string) (tea.Model, tea.Cmd) {
	fail := func(msg string) (tea.Model, tea.Cmd) {
		m.notice = msg
		return m, clearAfter(8 * time.Second)
	}
	if m.opts.ConfigPath == "" {
		return fail("no --config given; start the tui with the config file to change limits")
	}
	text, mod, err := readConfigText(m.opts.ConfigPath)
	if err != nil {
		return fail("limits: " + err.Error())
	}
	rules, err := config.RulesIn([]byte(text))
	if err != nil {
		return fail("limits: " + firstLine(err.Error()))
	}
	f := formState{open: true, editing: name, text: text, mod: mod, per: -1, sug: -1}
	for i := range f.inputs {
		ti := textinput.New()
		ti.Prompt = ""
		ti.CharLimit = 400
		ti.Cursor.SetMode(cursor.CursorStatic)
		f.inputs[i] = ti
	}
	var cur *config.Rule
	for i := range rules {
		if rules[i].Name == name && name != "" {
			cur = &rules[i]
		} else {
			f.others = append(f.others, rules[i].Name)
		}
	}
	switch {
	case name != "" && cur == nil:
		return fail(fmt.Sprintf("limit %s is not in %s; reload, or edit the file (5, e)", name, filepath.Base(m.opts.ConfigPath)))
	case cur != nil:
		if msg := f.load(cur); msg != "" {
			return fail(msg)
		}
	}
	f.focus = fOperation
	if cur != nil {
		f.focus = fAmount // changing a limit is usually changing its number
		if f.unit == unitBlocked {
			f.focus = fUnit
		}
	}
	f.inputs[f.focus].Focus()
	f.inputs[f.focus].CursorEnd()
	m.form = f
	m.form.check()
	return m, m.fetchSeen()
}

// load fills the form from a rule in the file; it returns a message when the rule holds something
// the form cannot show faithfully.
func (f *formState) load(r *config.Rule) string {
	for _, list := range [][]string{r.Operation, r.AccessKey, r.Bucket, r.Prefix, r.SourceIP} {
		for _, v := range list {
			if strings.Contains(v, ",") || v != strings.TrimSpace(v) || v == "" {
				return fmt.Sprintf("limit %s has a value the form cannot show (%q); edit the file (5, e)", r.Name, v)
			}
		}
	}
	f.inputs[fOperation].SetValue(strings.Join(r.Operation, ", "))
	f.inputs[fAccessKey].SetValue(strings.Join(r.AccessKey, ", "))
	f.inputs[fBucket].SetValue(strings.Join(r.Bucket, ", "))
	f.inputs[fPrefix].SetValue(strings.Join(r.Prefix, ", "))
	f.inputs[fSource].SetValue(strings.Join(r.SourceIP, ", "))
	f.inputs[fName].SetValue(r.Name)
	f.inputs[fNote].SetValue(r.Note)
	f.nameEdited = true
	if r.Burst > 0 {
		f.inputs[fBurst].SetValue(strconv.Itoa(r.Burst))
	}
	if r.Respond == config.RespondTooManyRequests {
		f.respond = 1
	}
	switch n, unit, ok := r.Count(); {
	case r.Blocked:
		f.unit = unitBlocked
	case !ok:
		return fmt.Sprintf("limit %s does not load cleanly; edit the file (5, e)", r.Name)
	default:
		f.inputs[fAmount].SetValue(strconv.FormatFloat(n, 'f', -1, 64))
		f.unit = map[string]int{config.UnitSecond: unitSecond, config.UnitMinute: unitMinute, config.UnitHour: unitHour}[unit]
	}
	want := strings.Join(sortedCopy(r.Per), ",")
	if r.Blocked && len(r.Per) == 0 {
		f.per = 0
	}
	for i, c := range f.perChoices() {
		if c.dims != nil && strings.Join(sortedCopy(c.dims), ",") == want {
			f.per = i
		}
	}
	if f.per < 0 && len(r.Per) > 0 {
		f.customPer = append([]string(nil), r.Per...)
		f.per = len(f.perChoices()) - 1
	}
	return ""
}

func sortedCopy(v []string) []string {
	out := append([]string(nil), v...)
	sort.Strings(out)
	return out
}

// perChoices are the answers to "counted how" that fit the form as it stands.
func (f *formState) perChoices() []perChoice {
	blocked := f.unit == unitBlocked
	out := []perChoice{{"everyone together", []string{config.PerTotal}, "all matching requests share one budget"}}
	if blocked {
		out[0] = perChoice{"together", nil, "one line for the rule in `rules top`"}
	}
	each := func(label string, dims []string, what string) {
		help := "a budget of its own for every " + what
		if blocked {
			help = "a line in `rules top` for every " + what
		}
		out = append(out, perChoice{label, dims, help})
	}
	each("each source IP", []string{config.DimSourceIP}, "client")
	each("each access key", []string{config.DimAccessKey}, "key")
	each("each bucket", []string{config.DimBucket}, "bucket")
	each("each access key on each bucket", []string{config.DimAccessKey, config.DimBucket}, "pair")
	if len(splitList(f.inputs[fPrefix].Value())) > 0 {
		each("each listed prefix", []string{config.DimPrefix}, "prefix")
	}
	if f.customPer != nil {
		out = append(out, perChoice{"as in the file: " + strings.Join(f.customPer, ", "), f.customPer, "kept as written"})
	}
	return out
}

// splitList reads a comma-separated field.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// lastToken is the entry being typed in a comma-separated field, and head is what precedes it.
func lastToken(s string) (head, tok string) {
	if i := strings.LastIndexByte(s, ','); i >= 0 {
		return s[:i+1] + " ", strings.TrimSpace(s[i+1:])
	}
	return "", strings.TrimSpace(s)
}

// check rebuilds the rule from the fields and records what is wrong with it, in the form's words.
func (f *formState) check() {
	r := config.Rule{
		Name:      strings.TrimSpace(f.inputs[fName].Value()),
		Operation: splitList(f.inputs[fOperation].Value()),
		AccessKey: splitList(f.inputs[fAccessKey].Value()),
		Bucket:    splitList(f.inputs[fBucket].Value()),
		Prefix:    splitList(f.inputs[fPrefix].Value()),
		SourceIP:  splitList(f.inputs[fSource].Value()),
		Note:      strings.TrimSpace(f.inputs[fNote].Value()),
	}
	if f.respond == 1 {
		r.Respond = config.RespondTooManyRequests
	}
	f.err = ""
	fail := func(format string, args ...any) {
		if f.err == "" {
			f.err = fmt.Sprintf(format, args...)
		}
	}
	// The fields are checked in the order they are asked, so the verdict names the first one that
	// needs attention; operation names are shown in S3's spelling as soon as they resolve.
	for i, entry := range r.Operation {
		_, canonical, err := config.ResolveOperation(entry)
		if err != nil {
			fail("Limit what: %s", err.Error())
			continue
		}
		r.Operation[i] = canonical
	}
	choices := f.perChoices()
	if f.per >= len(choices) {
		f.per = -1
	}
	if f.unit == unitBlocked {
		r.Blocked = true
		if f.per > 0 {
			r.Per = append(config.StringList(nil), choices[f.per].dims...)
		}
	} else {
		amount := strings.ReplaceAll(strings.TrimSpace(f.inputs[fAmount].Value()), ",", "")
		n, err := strconv.ParseFloat(amount, 64)
		switch {
		case amount == "":
			fail("To at most: type a number")
		case err != nil:
			fail("To at most: %q is not a number", amount)
		default:
			num := config.Number(n)
			switch f.unit {
			case unitMinute:
				r.LimitPerMinute = &num
			case unitHour:
				r.LimitPerHour = &num
			default:
				r.LimitPerSecond = &num
			}
		}
		if b := strings.ReplaceAll(strings.TrimSpace(f.inputs[fBurst].Value()), ",", ""); b != "" {
			if n, berr := strconv.Atoi(b); berr != nil || n < 1 {
				fail("At once: %q is not a whole number of 1 or more (leave it empty for the limit's own number)", b)
			} else {
				r.Burst = n
			}
		}
		if f.per < 0 {
			fail("Counted: choose who gets their own budget")
		} else {
			r.Per = append(config.StringList(nil), choices[f.per].dims...)
		}
	}
	if !f.nameEdited && f.editing == "" {
		f.inputs[fName].SetValue(f.autoName(&r))
		r.Name = f.inputs[fName].Value()
	}
	for _, other := range f.others {
		if other == r.Name {
			fail("Name: a limit named %s already exists", r.Name)
		}
	}
	if f.err == "" {
		if err := config.CheckRule(&r); err != nil {
			all := []error{err}
			if j, ok := err.(interface{ Unwrap() []error }); ok && len(j.Unwrap()) > 0 {
				all = j.Unwrap()
			}
			// The name is the last thing asked, so it is the last thing complained about.
			var name string
			for _, e := range all {
				var ce *config.Error
				switch {
				case !errors.As(e, &ce):
					fail("%s", e.Error())
				case ce.Key == "name":
					name = "Name: " + ce.Msg
				default:
					fail("%s: %s", fieldLabel(ce.Key), ce.Msg)
				}
			}
			if name != "" {
				fail("%s", name)
			}
		}
	}
	f.r = r
}

// fieldLabel maps a rule key from a validation error to the form's own label.
func fieldLabel(key string) string {
	if i := strings.IndexByte(key, '['); i >= 0 {
		key = key[:i]
	}
	switch {
	case key == "operation":
		return "Limit what"
	case key == "limit" || strings.HasPrefix(key, "limit_per_"):
		return "To at most"
	case key == "burst":
		return "At once"
	case key == "per":
		return "Counted"
	case key == "access_key":
		return "Only for access key"
	case key == "bucket":
		return "Only for bucket"
	case key == "prefix":
		return "Only for prefix"
	case key == "source_ip":
		return "Only for source IP"
	case key == "respond":
		return "Over the limit"
	case key == "name":
		return "Name"
	case key == "note":
		return "Note"
	}
	return key
}

// autoName derives a name from the form: the first operation, the first bucket or the tail of the
// first access key, and how it is counted, made unique among the file's rules.
func (f *formState) autoName(r *config.Rule) string {
	if len(r.Operation)+len(r.Bucket)+len(r.AccessKey)+len(r.Per) == 0 && !r.Blocked {
		return "" // nothing to derive a name from yet
	}
	parts := []string{"all"}
	if len(r.Operation) > 0 {
		parts[0] = strings.TrimSuffix(r.Operation[0], "*")
	}
	switch {
	case len(r.Bucket) > 0:
		parts = append(parts, r.Bucket[0])
	case len(r.AccessKey) > 0:
		k := r.AccessKey[0]
		if len(k) > 16 {
			k = k[len(k)-6:] // the tail of a long key tells keys apart without spelling one out
		}
		parts = append(parts, "key", k)
	}
	switch {
	case r.Blocked:
		parts = append(parts, "blocked")
	case len(r.Per) > 0:
		parts = append(parts, "per")
		parts = append(parts, r.Per...)
	}
	var b strings.Builder
	for _, c := range strings.ToLower(strings.Join(parts, "-")) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 56 {
		name = name[:56]
	}
	if name == "" {
		name = "limit"
	}
	taken := func(n string) bool {
		for _, o := range f.others {
			if o == n {
				return true
			}
		}
		return false
	}
	for i, base := 2, name; taken(name); i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

// suggestions are the picker rows for the focused field.
func (m Model) suggestions() []suggestion {
	f := &m.form
	if !f.focus.isPicker() {
		return nil
	}
	val := f.inputs[f.focus].Value()
	_, tok := lastToken(val)
	already := map[string]bool{}
	for _, v := range splitList(val) {
		if !strings.EqualFold(v, tok) {
			already[strings.ToLower(v)] = true
		}
	}
	low := strings.ToLower(strings.TrimSuffix(tok, "*"))
	var out []suggestion
	if f.focus != fOperation {
		list := f.keys
		if f.focus == fBucket {
			list = f.buckets
		}
		for _, s := range list {
			if s.value == "" || already[strings.ToLower(s.value)] || !strings.Contains(strings.ToLower(s.value), low) {
				continue
			}
			word := " requests lately"
			if s.requests == 1 {
				word = " request lately"
			}
			out = append(out, suggestion{s.value, config.FormatCount(float64(s.requests)) + word})
		}
		return out
	}
	type cand struct {
		name   string
		rate   float64
		prefix bool
		common int
	}
	var cands []cand
	family := 0
	for _, name := range config.OperationNames() {
		ln := strings.ToLower(name)
		if name == "Unknown" || already[ln] || !strings.Contains(ln, low) {
			continue
		}
		c := cand{name: name, rate: m.opRate[name], prefix: strings.HasPrefix(ln, low), common: commonRank(name)}
		if c.prefix {
			family++
		}
		cands = append(cands, c)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.prefix != b.prefix {
			return a.prefix
		}
		if a.rate != b.rate {
			return a.rate > b.rate
		}
		return a.common < b.common // without traffic to rank by, the everyday operations come first
	})
	for _, c := range cands {
		detail := ""
		if c.rate > 0 {
			detail = fmtRate(c.rate) + "/s right now"
		}
		out = append(out, suggestion{c.name, detail})
	}
	if low != "" && family > 1 {
		if ops, canonical, err := config.ResolveOperation(low + "*"); err == nil && !already[strings.ToLower(canonical)] {
			fam := suggestion{canonical, fmt.Sprintf("all %d operations that start with %s", len(ops), strings.TrimSuffix(canonical, "*"))}
			at := min(len(out), 3) // after the closest names, still on the first screenful
			out = append(out[:at], append([]suggestion{fam}, out[at:]...)...)
		}
	}
	return out
}

// commonOperations are the S3 calls that make up most traffic, in rough order; the picker offers
// them ahead of the other hundred when live rates do not decide.
var commonOperations = []string{
	"GetObject", "PutObject", "HeadObject", "ListObjectsV2", "DeleteObject", "ListObjects", "ListBuckets", "HeadBucket",
	"CreateMultipartUpload", "UploadPart", "CompleteMultipartUpload", "CopyObject", "DeleteObjects", "AbortMultipartUpload",
	"ListMultipartUploads", "ListParts", "ListObjectVersions",
}

func commonRank(name string) int {
	for i, c := range commonOperations {
		if c == name {
			return i
		}
	}
	return len(commonOperations)
}

// highlight is the suggestion enter would accept: the one the arrows chose, or, in the operation
// field, the first one when what is typed is not yet an operation (so a few letters and enter
// are enough). Access keys and buckets are never completed unasked: the operator may be typing
// one the proxy has not seen.
func (m Model) highlight(sugs []suggestion) int {
	f := &m.form
	switch {
	case len(sugs) == 0:
		return -1
	case f.sug >= 0:
		return min(f.sug, len(sugs)-1)
	case f.focus != fOperation:
		return -1
	}
	_, tok := lastToken(f.inputs[fOperation].Value())
	if tok == "" {
		return -1
	}
	if _, _, err := config.ResolveOperation(tok); err == nil {
		return -1
	}
	return 0
}

// fetchSeen asks showback which access keys and buckets it has recorded in this hour and the last.
func (m Model) fetchSeen() tea.Cmd {
	c, now := m.opts.Client, m.opts.Now
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hour := now().UTC().Truncate(time.Hour)
		list := func(dim string) []seen {
			q := url.Values{"group_by": {dim}, "from": {hour.Add(-time.Hour).Format(time.RFC3339)}, "to": {hour.Add(time.Hour).Format(time.RFC3339)}}
			var res showback.Result
			if _, err := c.Do(ctx, http.MethodGet, "/admin/usage?"+q.Encode(), nil, &res); err != nil {
				return nil
			}
			out := make([]seen, 0, len(res.Groups))
			for _, g := range res.Groups {
				out = append(out, seen{g.Keys[dim], g.Requests})
			}
			sort.SliceStable(out, func(i, j int) bool { return out[i].requests > out[j].requests })
			return out
		}
		return seenMsg{keys: list(config.DimAccessKey), buckets: list(config.DimBucket)}
	}
}

// keyForm routes keys while the form is open: tab moves between fields, the arrows act inside
// the focused one, enter accepts a suggestion or moves on, ctrl+s and ctrl+r save.
func (m Model) keyForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := &m.form
	if f.discard {
		switch msg.String() {
		case "y", "Y":
			m.form = formState{}
		case "n", "N", "esc":
			f.discard = false
		}
		return m, nil
	}
	sugs := m.suggestions()
	pick := m.highlight(sugs)
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		if f.dirty {
			f.discard = true
			return m, nil
		}
		m.form = formState{}
		return m, nil
	case "ctrl+s":
		return m.saveForm(false)
	case "ctrl+r":
		return m.saveForm(true)
	case "tab":
		f.move(1)
		return m, nil
	case "shift+tab":
		f.move(-1)
		return m, nil
	case "enter":
		if f.focus.isPicker() && pick >= 0 {
			head, _ := lastToken(f.inputs[f.focus].Value())
			f.inputs[f.focus].SetValue(head + sugs[pick].value)
			f.inputs[f.focus].CursorEnd()
			f.sug, f.dirty = -1, true
			f.check()
			return m, nil
		}
		f.move(1)
		return m, nil
	case "down":
		if f.focus.isPicker() && pick < len(sugs)-1 {
			f.sug = pick + 1
			return m, nil
		}
		if !f.focus.isPicker() {
			f.move(1)
		}
		return m, nil
	case "up":
		if f.focus.isPicker() && f.sug >= 0 {
			f.sug--
			return m, nil
		}
		f.move(-1)
		return m, nil
	}
	if !f.focus.isText() {
		switch msg.String() {
		case "right", " ", "l":
			f.cycle(1)
		case "left", "h":
			f.cycle(-1)
		}
		return m, nil
	}
	before := f.inputs[f.focus].Value()
	var cmd tea.Cmd
	f.inputs[f.focus], cmd = f.inputs[f.focus].Update(msg)
	if f.inputs[f.focus].Value() != before {
		f.dirty, f.sug = true, -1
		if f.focus == fName {
			f.nameEdited = true
		}
		f.check()
	}
	return m, cmd
}

// move shifts the focus by d fields, skipping the amount and burst of a blocked rule.
func (f *formState) move(d int) {
	f.inputs[f.focus].Blur()
	if f.focus == fOperation {
		// Leaving the field, the names that resolve are shown the way S3 spells them.
		parts := splitList(f.inputs[fOperation].Value())
		for i, p := range parts {
			if _, canonical, err := config.ResolveOperation(p); err == nil {
				parts[i] = canonical
			}
		}
		f.inputs[fOperation].SetValue(strings.Join(parts, ", "))
	}
	for {
		f.focus = formField((int(f.focus) + d + int(numFormFields)) % int(numFormFields))
		if (f.focus != fAmount && f.focus != fBurst) || f.unit != unitBlocked {
			break
		}
	}
	f.sug = -1
	if f.focus.isText() {
		f.inputs[f.focus].Focus()
		f.inputs[f.focus].CursorEnd()
	}
}

// cycle changes the focused choice.
func (f *formState) cycle(d int) {
	switch f.focus {
	case fUnit:
		f.unit = (f.unit + d + len(unitLabels)) % len(unitLabels)
		if f.unit == unitBlocked && f.per < 0 {
			f.per = 0
		}
	case fPer:
		n := len(f.perChoices())
		if f.per < 0 {
			f.per = 0
			if d < 0 {
				f.per = n - 1
			}
		} else {
			f.per = (f.per + d + n) % n
		}
	case fRespond:
		f.respond = (f.respond + d + len(respondLabels)) % len(respondLabels)
	default:
		return
	}
	f.dirty = true
	f.check()
}

// saveForm writes the rule into the file's text and saves it the way the editor does.
func (m Model) saveForm(reload bool) (tea.Model, tea.Cmd) {
	f := &m.form
	fail := func(msg string) (tea.Model, tea.Cmd) {
		m.notice = "not saved: " + msg
		return m, clearAfter(8 * time.Second)
	}
	f.check()
	if f.err != "" {
		return fail(f.err)
	}
	r := f.r
	var out []byte
	var err error
	if f.editing != "" {
		out, err = config.ReplaceRule([]byte(f.text), f.editing, &r)
	} else {
		out, err = config.AddRule([]byte(f.text), &r)
	}
	if err != nil {
		return fail(firstLine(err.Error()))
	}
	if _, perr := config.Parse(out); perr != nil {
		return fail("the file would not load: " + firstLine(perr.Error()))
	}
	path, mod, now, name := m.opts.ConfigPath, f.mod, m.opts.Now, r.Name
	return m, func() tea.Msg {
		_, backup, werr := writeConfig(path, string(out), mod, now())
		return formSavedMsg{name: name, backup: backup, reload: reload, err: werr}
	}
}

// keyConfirmDelete answers the delete prompt.
func (m Model) keyConfirmDelete(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	name := m.confirmDelete
	m.confirmDelete = ""
	if s := msg.String(); s != "y" && s != "Y" {
		return m, nil
	}
	path, now := m.opts.ConfigPath, m.opts.Now
	return m, func() tea.Msg {
		text, mod, err := readConfigText(path)
		if err != nil {
			return formSavedMsg{name: name, deleted: true, err: err}
		}
		out, err := config.DeleteRule([]byte(text), name)
		if err != nil {
			return formSavedMsg{name: name, deleted: true, err: err}
		}
		if _, perr := config.Parse(out); perr != nil {
			return formSavedMsg{name: name, deleted: true, err: fmt.Errorf("the file would not load: %s", firstLine(perr.Error()))}
		}
		_, backup, err := writeConfig(path, string(out), mod, now())
		return formSavedMsg{name: name, backup: backup, deleted: true, reload: true, err: err}
	}
}

// updateForm handles the form's own messages; ok is false when msg was not one of them.
func (m Model) updateForm(msg tea.Msg) (Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case seenMsg:
		m.form.keys, m.form.buckets = msg.keys, msg.buckets
		return m, nil, true
	case formSavedMsg:
		if msg.err != nil {
			m.notice = "not saved: " + firstLine(msg.err.Error())
			return m, clearAfter(8 * time.Second), true
		}
		verb := "saved"
		if msg.deleted {
			verb = "deleted"
		}
		m.form = formState{}
		m.notice = fmt.Sprintf("%s limit %s in %s; backup %s", verb, msg.name, filepath.Base(m.opts.ConfigPath), filepath.Base(msg.backup))
		if msg.reload {
			return m, tea.Batch(m.reload(), clearAfter(8*time.Second)), true
		}
		return m, clearAfter(8 * time.Second), true
	}
	return m, nil, false
}

// ---- rendering

const formLabelW = 15

// formLines renders the form as one pane: the fields, a context area for the focused field
// (suggestions, choices, or a hint), and the verdict (the sentence, or what is wrong, and what the
// traffic looks like right now).
func (m Model) formLines() []string {
	f := &m.form
	room := m.height - 2 // tabs and footer
	if m.stale {
		room--
	}
	w := m.width - 2
	if w < 40 {
		w = 40
	}
	cw := w - 2 // inside the padding
	title := "New limit"
	if f.editing != "" {
		title = "Change limit " + f.editing
	}
	border := colorBlue
	if f.err != "" {
		border = colorOrange
	}
	rows, verdict := m.formRows(cw), m.formVerdict(cw)
	body := make([]string, 0, room)
	body = append(body, styleTitle.Render(title))
	body = append(body, rows...)
	ctxRoom := room - 2 - len(body) - len(verdict)
	if ctxRoom < 2 {
		ctxRoom = 2
	}
	if ctxRoom > 10 {
		ctxRoom = 10
	}
	body = append(body, m.formContext(cw, ctxRoom)...)
	body = append(body, verdict...)
	for i := range body {
		body[i] = fit(body[i], cw)
	}
	pane := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).Padding(0, 1).Width(w)
	return strings.Split(pane.Render(strings.Join(body, "\n")), "\n")
}

// formRows are the field lines.
func (m Model) formRows(cw int) []string {
	f := &m.form
	label := func(field formField, text string) string {
		mark, st := "  ", lipgloss.NewStyle()
		if f.focus == field {
			mark, st = "▸ ", styleTitle
		}
		return mark + st.Render(fmt.Sprintf("%-*s", formLabelW, text))
	}
	text := func(field formField, width int, empty string) string {
		in := f.inputs[field]
		if f.focus == field {
			in.Width = max(8, width)
			return in.View()
		}
		if v := in.Value(); v != "" {
			return trunc(v, width)
		}
		return styleDim.Render(empty)
	}
	choice := func(field formField, value string) string {
		if f.focus == field {
			return styleSel.Render(" ‹ " + value + " › ")
		}
		return value
	}
	valW := cw - 2 - formLabelW
	rows := []string{label(fOperation, "Limit what") + text(fOperation, valW, "every operation")}

	lead := f.focus
	if lead != fAmount && lead != fUnit && lead != fBurst {
		lead = fAmount
	}
	if f.unit == unitBlocked {
		rows = append(rows, label(fUnit, "Limit")+choice(fUnit, unitLabels[f.unit])+styleDim.Render("  every matching request is refused"))
	} else {
		rows = append(rows, label(lead, "To at most")+text(fAmount, 10, "a number")+"  "+choice(fUnit, unitLabels[f.unit])+
			styleDim.Render("   at once: ")+text(fBurst, 10, "same as the limit"))
	}

	per, perLabel := styleWarn.Render("choose…"), "Counted"
	if f.unit == unitBlocked {
		perLabel = "List refusals"
	}
	if choices := f.perChoices(); f.per >= 0 && f.per < len(choices) {
		per = choices[f.per].label
	}
	rows = append(rows, label(fPer, perLabel)+choice(fPer, per))

	sub := func(field formField, first bool, name string) string {
		l := ""
		if first {
			l = "Only for"
		}
		return label(field, l) + fmt.Sprintf("%-11s", name) + text(field, valW-11, "any")
	}
	rows = append(rows,
		sub(fAccessKey, true, "access key"),
		sub(fBucket, false, "bucket"),
		sub(fPrefix, false, "prefix"),
		sub(fSource, false, "source IP"),
		label(fRespond, "Over the limit")+choice(fRespond, respondLabels[f.respond]),
		label(fName, "Name")+text(fName, valW, "a short name"),
		label(fNote, "Note")+text(fNote, valW, "why this limit exists (optional)"),
	)
	return rows
}

// formContext is the area under the fields for the focused one: its suggestions, its choices, or
// a hint, in exactly room lines.
func (m Model) formContext(cw, room int) []string {
	f := &m.form
	var lines []string
	head := func(text string) {
		lines = append(lines, styleDim.Render(fit("── "+text+" "+strings.Repeat("─", max(0, cw-4-lipgloss.Width(text))), cw)))
	}
	choices := func(labels, helps []string, cur int) {
		first := 0
		if show := room - len(lines); cur >= show {
			first = cur - show + 1 // keep the chosen one in view
		}
		for i := first; i < len(labels) && len(lines) < room; i++ {
			mark, st := "( ) ", lipgloss.NewStyle()
			if i == cur {
				mark, st = "(•) ", styleOK
			}
			lines = append(lines, "  "+st.Render(fmt.Sprintf("%s%-31s", mark, labels[i]))+" "+styleDim.Render(helpAt(helps, i)))
		}
	}
	switch f.focus {
	case fUnit:
		head("per what: ←/→ to choose")
		choices(unitLabels, []string{"", "", "", "refuse every matching request"}, f.unit)
	case fPer:
		if f.unit == unitBlocked {
			head("how `rules top` lists the refusals: ←/→ to choose")
		} else {
			head("who gets a budget of their own: ←/→ to choose")
		}
		cs := f.perChoices()
		labels, helps := make([]string, 0, len(cs)), make([]string, 0, len(cs))
		for _, c := range cs {
			labels, helps = append(labels, c.label), append(helps, c.help)
		}
		choices(labels, helps, f.per)
	case fRespond:
		head("what the client gets over the limit: ←/→ to choose")
		choices(respondLabels, nil, f.respond)
	case fAmount:
		head("how many")
		lines = append(lines, "  A number, for example 20.", "  That many may also arrive at once; the allowance then refills evenly.")
	case fBurst:
		head("how many at once")
		lines = append(lines, "  How many may arrive at once before the rate applies.", "  Empty: the same as the limit's own number.")
	case fPrefix:
		head("only for these key prefixes")
		lines = append(lines, "  Prefixes inside the bucket, for example logs/ (a comma between several).")
	case fSource:
		head("only for these clients")
		lines = append(lines, "  An address or a range, for example 10.20.0.0/16 (a comma between several).")
	case fName:
		head("name")
		lines = append(lines, "  What the limit is called in this console, the metrics, and the logs.")
	case fNote:
		head("note")
		lines = append(lines, "  Why the limit exists: a ticket, a date, who asked. Shown beside the rule.")
	default:
		none := "  (no S3 operation matches; names are as in the S3 API, such as ListObjectsV2)"
		if f.focus == fOperation {
			head("S3 operations, busiest first: ↓ pick · enter accept · comma for another")
		} else {
			head("seen by the proxy lately, busiest first: ↓ pick · enter accept")
			none = "  (nothing recorded lately that matches; type the value)"
		}
		sugs := m.suggestions()
		if len(sugs) == 0 {
			lines = append(lines, styleDim.Render(none))
		}
		pick, first := m.highlight(sugs), 0
		if pick >= room-1 {
			first = pick - (room - 2)
		}
		for i := first; i < len(sugs) && len(lines) < room; i++ {
			row := fmt.Sprintf("    %-36s %s", sugs[i].value, sugs[i].detail)
			if i == pick {
				row = styleSel.Render(fmt.Sprintf("  ▸ %-36s %s ", sugs[i].value, sugs[i].detail))
			}
			lines = append(lines, row)
		}
	}
	for len(lines) < room {
		lines = append(lines, "")
	}
	return lines[:room]
}

func helpAt(helps []string, i int) string {
	if i < len(helps) {
		return helps[i]
	}
	return ""
}

// formVerdict is the last three lines of the pane: the sentence (or the first problem) on two,
// and what the traffic looks like right now on one.
func (m Model) formVerdict(cw int) []string {
	f := &m.form
	var two []string
	if f.err != "" {
		two = wrap("✗ "+f.err, cw-8)
		for i := range two {
			two[i] = styleErr.Render(two[i])
		}
	} else {
		two = wrap(f.r.Sentence(), cw-8)
		if len(two) > 2 {
			two = append(two[:1], trunc(strings.Join(two[1:], " "), cw-8))
		}
		for i := range two {
			two[i] = styleOK.Render(two[i])
		}
	}
	for len(two) < 2 {
		two = append(two, "")
	}
	now, warn := m.formNow()
	st := styleDim
	if warn {
		st = styleWarn
	}
	return []string{styleTitle.Render("Means   ") + two[0], "        " + two[1], styleTitle.Render("Now     ") + st.Render(trunc(now, cw-8))}
}

// formNow says what the rule would meet: a filter value the proxy has not seen lately (the rule
// that names the wrong key), or the current rate of the operations it limits.
func (m Model) formNow() (text string, warn bool) {
	f := &m.form
	if f.discard {
		return "discard this limit? y discard · n keep editing", true
	}
	missing := func(vals []string, list []seen, what string) string {
		if len(list) == 0 {
			return ""
		}
		for _, v := range vals {
			found := false
			for _, s := range list {
				if s.value == v {
					found = true
				}
			}
			if !found {
				return fmt.Sprintf("⚠ no request %s %s seen in the last two hours", what, v)
			}
		}
		return ""
	}
	if w := missing(f.r.AccessKey, f.keys, "from access key"); w != "" {
		return w, true
	}
	if w := missing(f.r.Bucket, f.buckets, "for bucket"); w != "" {
		return w, true
	}
	if m.opRate == nil {
		return "rates appear after the next poll", false
	}
	ops := f.r.Operations()
	rate, what := 0.0, "all requests"
	switch {
	case len(f.r.Operation) > 0 && len(ops) == 0:
		return "", false // the operation does not resolve yet; the verdict says so
	case ops == nil:
		for _, r := range m.opRate {
			rate += r
		}
	case len(ops) == 1:
		rate, what = m.opRate[ops[0]], ops[0]
	default:
		for _, op := range ops {
			rate += m.opRate[op]
		}
		what = fmt.Sprintf("these %d operations", len(ops))
	}
	text = fmt.Sprintf("%s: %s/s right now", what, fmtRate(rate))
	if rate == 0 {
		return what + ": no requests right now", false
	}
	shared := len(f.r.Per) == 1 && f.r.Per[0] == config.PerTotal
	unfiltered := len(f.r.AccessKey)+len(f.r.Bucket)+len(f.r.Prefix)+len(f.r.SourceIP) == 0
	switch limit := f.r.Rate(); {
	case !unfiltered:
		text += ", before this rule's filters"
	case f.err == "" && shared && limit > 0 && rate > limit:
		text += fmt.Sprintf("; about %s/s of it would be over the limit", fmtRate(rate-limit))
	}
	return text, false
}
