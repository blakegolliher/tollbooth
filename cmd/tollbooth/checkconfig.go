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
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/blakegolliher/tollbooth/internal/config"
)

// runCheckConfig validates a file without starting: every error on its own line, then warnings,
// exit 1 on any error (unknown keys included), 0 otherwise.
func runCheckConfig(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("check-config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	quiet := fs.Bool("q", false, "print nothing on success")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	path := "tollbooth.yaml"
	if fs.NArg() > 0 {
		path = fs.Arg(0)
	}
	c, err := config.Load(path)
	if err != nil {
		for _, line := range splitErrors(err) {
			_, _ = fmt.Fprintf(stderr, "%s: %s\n", path, line)
		}
		return 1
	}
	warnings := c.Warnings(goMemLimit())
	for _, w := range warnings {
		_, _ = fmt.Fprintf(stderr, "%s: warning: %s\n", path, w)
	}
	if !*quiet {
		_, _ = fmt.Fprintf(stdout, "%s: ok (%d rules, %d backend endpoints, %d warnings)\n", path, len(c.Limits), len(c.Backend.Endpoints), len(warnings))
		// The read-back (ADR-0007): each rule as tollbooth understood it.
		for i := range c.Limits {
			_, _ = fmt.Fprintf(stdout, "  %s: %s\n", c.Limits[i].Name, c.Limits[i].Sentence())
		}
	}
	return 0
}

// splitErrors flattens an errors.Join tree into one line per finding.
func splitErrors(err error) []string {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		errs := j.Unwrap()
		out := make([]string, 0, len(errs))
		for _, e := range errs {
			out = append(out, splitErrors(e)...)
		}
		return out
	}
	var ce *config.Error
	if errors.As(err, &ce) {
		return []string{ce.Error()}
	}
	return strings.Split(strings.TrimSpace(err.Error()), "\n")
}

// goMemLimit parses GOMEMLIMIT (bytes with an optional B, KiB, MiB, GiB, TiB suffix as the runtime
// accepts) from the checking environment; 0 when unset or unparseable.
func goMemLimit() int64 {
	v := strings.TrimSpace(os.Getenv("GOMEMLIMIT"))
	if v == "" || v == "off" {
		return 0
	}
	mult := int64(1)
	for _, s := range []struct {
		suffix string
		mult   int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(v, s.suffix) {
			v, mult = strings.TrimSuffix(v, s.suffix), s.mult
			break
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n * mult
}
