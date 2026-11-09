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
	"io"

	tea "github.com/charmbracelet/bubbletea"
)

// Run starts the program on the terminal and blocks until the operator quits. Bubble Tea reads
// NO_COLOR and TERM itself (TERM=dumb and NO_COLOR give a plain ASCII rendering).
func Run(opts Options, in io.Reader, out io.Writer) error {
	p := tea.NewProgram(New(opts), tea.WithAltScreen(), tea.WithInput(in), tea.WithOutput(out))
	_, err := p.Run()
	return err
}
