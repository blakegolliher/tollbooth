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

// Command tollbooth is the proxy and its operator CLI: serve, status, rules, config, backends,
// usage, check-config, doctor, tui, version. One binary (CLAUDE.md). Subcommands are dispatched
// with the standard library's flag package; each verb's flags are its own.
package main

import (
	"fmt"
	"os"
	"runtime"
)

// Set by the Makefile through -ldflags -X.
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

const usage = `usage: tollbooth <command> [flags]

commands:
  serve         run the proxy (--config FILE)
  check-config  validate a config file without starting (exit 1 on any error)
  rules         list | show <id> | test | top: the rate-limit rules in force (via the admin API)
  version       print version, commit, build date, and Go version

  status, config, backends, usage, doctor, tui: arrive in Phases 3 and 4

every verb that talks to the running proxy takes --admin, --token, --config, and
--format table|json|csv; the endpoint and token fall back to TOLLBOOTH_ADMIN and
TOLLBOOTH_TOKEN, then to the config file.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches a verb and returns the process exit code. Verbs print one line on stderr and
// return non-zero on failure (docs/SPEC.md, Operator surface).
func run(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:], stdout, stderr)
	case "check-config":
		return runCheckConfig(args[1:], stdout, stderr)
	case "rules":
		return runRules(args[1:], stdout, stderr)
	case "version":
		_, _ = fmt.Fprintf(stdout, "tollbooth %s (commit %s, built %s, %s)\n", version, commit, date, runtime.Version())
		return 0
	case "status", "config", "backends", "usage", "doctor", "tui":
		_, _ = fmt.Fprintf(stderr, "tollbooth %s: not available yet (see docs/STATUS.md)\n", args[0])
		return 2
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "tollbooth: unknown command %q\n%s", args[0], usage)
	return 2
}
