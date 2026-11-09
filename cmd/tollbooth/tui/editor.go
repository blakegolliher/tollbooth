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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/blakegolliher/tollbooth/internal/config"
)

// The Config screen's editor (ADR-0006): the file's raw text in a textarea, NORMAL and INSERT
// modes, validation on every change with the same code as check-config, and a save that is the
// only write the TUI ever makes to the config file: explicit, validated, atomic (temp and rename),
// with a timestamped backup beside the file, refused when the file changed on disk since it was
// loaded.

type editorState struct {
	open     bool
	insert   bool
	prompt   bool // save/discard prompt after esc on a dirty buffer
	ta       textarea.Model
	loaded   string    // the text as loaded, for dirty detection
	loadedAt time.Time // the file's modification time when loaded
	findings []string  // validation result, first line is the verdict
	rules    int
}

// Messages.
type (
	editorSavedMsg struct {
		mod    time.Time
		backup string
		reload bool
		err    error
	}
	editorExternalMsg struct {
		text string
		err  error
	}
)

// openEditor loads the file on disk into the editor.
func (m Model) openEditor() (tea.Model, tea.Cmd) {
	if m.opts.ConfigPath == "" {
		m.notice = "no --config given; nothing to edit"
		return m, clearAfter(8 * time.Second)
	}
	text, mod, err := readConfigText(m.opts.ConfigPath)
	if err != nil {
		m.notice = "edit: " + err.Error()
		return m, clearAfter(8 * time.Second)
	}
	ta := textarea.New()
	ta.ShowLineNumbers = true
	ta.Cursor.SetMode(cursor.CursorStatic) // a steady cursor in a config file; also keeps the tests from waiting on blinks
	ta.Prompt = ""
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.MaxWidth = 0
	ta.SetValue(text)
	ta.CursorStart()
	for ta.Line() > 0 {
		ta.CursorUp()
	}
	m.ed = editorState{open: true, ta: ta, loaded: text, loadedAt: mod}
	m.ed.ta.Blur()
	m.sizeEditor()
	m.ed.validate()
	return m, nil
}

func (m *Model) sizeEditor() {
	w := m.width - 2
	if w < 20 {
		w = 20
	}
	// tabs, header, findings (up to 3), footer, blank
	h := m.height - 8
	if m.stale {
		h--
	}
	if h < 3 {
		h = 3
	}
	m.ed.ta.SetWidth(w)
	m.ed.ta.SetHeight(h)
}

func readConfigText(path string) (string, time.Time, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", time.Time{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", time.Time{}, err
	}
	return string(b), st.ModTime(), nil
}

// dirty reports whether the buffer differs from what was loaded.
func (e *editorState) dirty() bool { return e.ta.Value() != e.loaded }

// validate runs the buffer through config.Parse (YAML with line and column, unknown keys, bounds,
// defaults) and records the verdict. Restart-only changes are reported by the reload itself (D-046).
func (e *editorState) validate() {
	c, err := config.Parse([]byte(e.ta.Value()))
	if err != nil {
		e.findings = []string{"INVALID: " + firstLine(err.Error())}
		e.rules = 0
		return
	}
	e.rules = len(c.RateLimits.Rules)
	e.findings = []string{fmt.Sprintf("valid: %d rules", e.rules)}
	for _, w := range c.Warnings(0) {
		e.findings = append(e.findings, "warning: "+firstLine(w))
	}
}

func (e *editorState) valid() bool {
	return len(e.findings) > 0 && !strings.HasPrefix(e.findings[0], "INVALID")
}

// keyEditor routes keys while the editor is open.
func (m Model) keyEditor(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.ed.prompt {
		switch msg.String() {
		case "y", "Y":
			m.ed.prompt = false
			return m, m.saveConfig(false)
		case "n", "N":
			m.ed = editorState{}
			return m, nil
		case "esc":
			m.ed.prompt = false
			return m, nil
		}
		return m, nil
	}
	if m.ed.insert {
		if msg.Type == tea.KeyEsc {
			m.ed.insert = false
			m.ed.ta.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		m.ed.ta, cmd = m.ed.ta.Update(msg)
		m.ed.validate()
		return m, cmd
	}
	switch msg.String() {
	case "i":
		m.ed.insert = true
		return m, m.ed.ta.Focus()
	case "esc", "q":
		if m.ed.dirty() {
			m.ed.prompt = true
			return m, nil
		}
		m.ed = editorState{}
		return m, nil
	case "ctrl+s":
		return m, m.saveConfig(false)
	case "ctrl+r":
		return m, m.saveConfig(true)
	case "E":
		return m, m.externalEditor()
	case "L":
		text, mod, err := readConfigText(m.opts.ConfigPath)
		if err != nil {
			m.notice = "reload buffer: " + err.Error()
			return m, clearAfter(8 * time.Second)
		}
		m.ed.ta.SetValue(text)
		m.ed.loaded, m.ed.loadedAt = text, mod
		m.ed.validate()
		m.notice = "buffer reloaded from disk"
		return m, clearAfter(4 * time.Second)
	case "up", "k":
		m.ed.ta.CursorUp()
	case "down", "j":
		m.ed.ta.CursorDown()
	case "left", "h":
		m.ed.ta.SetCursor(m.ed.ta.LineInfo().ColumnOffset - 1)
	case "right", "l":
		m.ed.ta.SetCursor(m.ed.ta.LineInfo().ColumnOffset + 1)
	case "pgup":
		for i := 0; i < m.ed.ta.Height(); i++ {
			m.ed.ta.CursorUp()
		}
	case "pgdown":
		for i := 0; i < m.ed.ta.Height(); i++ {
			m.ed.ta.CursorDown()
		}
	case "home", "g":
		for m.ed.ta.Line() > 0 {
			m.ed.ta.CursorUp()
		}
		m.ed.ta.CursorStart()
	case "end", "G":
		for m.ed.ta.Line() < m.ed.ta.LineCount()-1 {
			m.ed.ta.CursorDown()
		}
		m.ed.ta.CursorEnd()
	case "?":
		m.help = !m.help
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	}
	return m, nil
}

// saveConfig validates the buffer and writes it. The write is the only one the TUI makes.
func (m Model) saveConfig(reload bool) tea.Cmd {
	path, text, loadedAt := m.opts.ConfigPath, m.ed.ta.Value(), m.ed.loadedAt
	if !m.ed.valid() {
		return func() tea.Msg { return editorSavedMsg{err: errors.New("not saved: the buffer does not validate")} }
	}
	now := m.opts.Now
	return func() tea.Msg {
		mod, backup, err := writeConfig(path, text, loadedAt, now())
		return editorSavedMsg{mod: mod, backup: backup, reload: reload, err: err}
	}
}

// writeConfig writes text to path atomically after backing up the current content to
// path.<timestamp>.bak, refusing when the file's modification time is not loadedAt.
func writeConfig(path, text string, loadedAt, now time.Time) (time.Time, string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}, "", err
	}
	if !st.ModTime().Equal(loadedAt) {
		return time.Time{}, "", fmt.Errorf("%s changed on disk since it was loaded (press L to reload the buffer)", filepath.Base(path))
	}
	old, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, "", err
	}
	backup := fmt.Sprintf("%s.%s.bak", path, now.UTC().Format("20060102T150405Z"))
	if werr := os.WriteFile(backup, old, st.Mode().Perm()); werr != nil { //nolint:gosec // beside the operator's own config file, by design (ADR-0006)
		return time.Time{}, "", fmt.Errorf("backup: %w", werr)
	}
	tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	if werr := os.WriteFile(tmp, []byte(text), st.Mode().Perm()); werr != nil { //nolint:gosec // same directory as the config file
		return time.Time{}, "", werr
	}
	if rerr := os.Rename(tmp, path); rerr != nil {
		_ = os.Remove(tmp)
		return time.Time{}, "", rerr
	}
	st, err = os.Stat(path)
	if err != nil {
		return time.Time{}, backup, err
	}
	return st.ModTime(), backup, nil
}

// externalEditor runs $EDITOR on a copy of the buffer and reads it back.
func (m Model) externalEditor() tea.Cmd {
	editor := m.opts.Editor
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	tmp, err := os.CreateTemp("", "tollbooth-edit-*.yaml")
	if err != nil {
		return func() tea.Msg { return editorExternalMsg{err: err} }
	}
	if _, err := tmp.WriteString(m.ed.ta.Value()); err != nil {
		_ = tmp.Close()
		return func() tea.Msg { return editorExternalMsg{err: err} }
	}
	_ = tmp.Close()
	name := tmp.Name()
	cmd := exec.CommandContext(context.Background(), editor, name) //nolint:gosec // the operator's own $EDITOR, by design (ADR-0006)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		defer os.Remove(name) //nolint:errcheck // temp file
		if err != nil {
			return editorExternalMsg{err: err}
		}
		b, rerr := os.ReadFile(name)
		if rerr != nil {
			return editorExternalMsg{err: rerr}
		}
		return editorExternalMsg{text: string(b)}
	})
}

// updateEditor handles the editor's own messages; ok is false when msg was not one of them.
func (m Model) updateEditor(msg tea.Msg) (Model, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case editorSavedMsg:
		if msg.err != nil {
			m.notice = "save: " + msg.err.Error()
			return m, clearAfter(8 * time.Second), true
		}
		m.ed.loaded, m.ed.loadedAt = m.ed.ta.Value(), msg.mod
		m.notice = "saved " + m.opts.ConfigPath + "; backup " + filepath.Base(msg.backup)
		if msg.reload {
			return m, tea.Batch(m.reload(), clearAfter(8*time.Second)), true
		}
		return m, clearAfter(8 * time.Second), true
	case editorExternalMsg:
		if msg.err != nil {
			m.notice = "$EDITOR: " + msg.err.Error()
			return m, clearAfter(8 * time.Second), true
		}
		m.ed.ta.SetValue(msg.text)
		m.ed.validate()
		m.notice = "buffer read back from $EDITOR"
		return m, clearAfter(4 * time.Second), true
	}
	return m, nil, false
}

// editorLines renders the editor: a header, the textarea, and the findings.
func (m Model) editorLines() []string {
	mode := "NORMAL"
	if m.ed.insert {
		mode = "INSERT"
	}
	dirty := ""
	if m.ed.dirty() {
		dirty = " · modified"
	}
	li := m.ed.ta.LineInfo()
	hdr := fmt.Sprintf("  %s  -- %s --%s  line %d col %d", filepath.Base(m.opts.ConfigPath), mode, dirty, m.ed.ta.Line()+1, li.ColumnOffset+1)
	lines := []string{styleTitle.Render(hdr)}
	lines = append(lines, strings.Split(m.ed.ta.View(), "\n")...)
	for i, f := range m.ed.findings {
		if i >= 3 {
			break
		}
		if strings.HasPrefix(f, "INVALID") {
			f = styleStale.Render(" " + f + " ")
		}
		lines = append(lines, "  "+f)
	}
	if m.ed.prompt {
		lines = append(lines, "", styleStale.Render("  Unsaved changes. y save · n discard · esc keep editing  "))
	}
	return lines
}
