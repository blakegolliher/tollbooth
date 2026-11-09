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

package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/blakegolliher/tollbooth/cmd/tollbooth/tui"
)

// runTUI is `tollbooth tui` (ADR-0003): the same admin client and flag resolution as the verbs.
func runTUI(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	fs.SetOutput(stderr)
	af := &adminFlags{}
	af.bind(fs)
	interval := fs.Duration("interval", 2*time.Second, "poll interval")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	af.format = "table"
	c, err := af.client()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "tollbooth tui: %v\n", err)
		return 1
	}
	cfgPath := af.config
	if _, err := os.Stat(cfgPath); err != nil {
		cfgPath = "" // the Config screen's diff and v need a file; without one they say so
	}
	if err := tui.Run(tui.Options{Client: c, Interval: *interval, ConfigPath: cfgPath}, os.Stdin, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "tollbooth tui: %v\n", err)
		return 1
	}
	return 0
}
