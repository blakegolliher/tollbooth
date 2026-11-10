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
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/blakegolliher/tollbooth/internal/admin"
	"github.com/blakegolliher/tollbooth/internal/config"
)

const configUsage = `usage: tollbooth config <verb> [flags]

verbs:
  show     the running config, redacted, with the generation and last reload result (GET /admin/config)
  reload   reload the config file on the proxy (POST /admin/reload); exit 1 when it was rejected
  diff     the running config against --config on disk: one line per key that differs

Nothing here writes the config file: edit it, then reload.
shared flags: --admin, --token, --config, --format table|json|csv
`

// runConfig dispatches the config verbs. Neither the CLI nor the TUI writes the file.
func runConfig(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, configUsage)
		return 2
	}
	verb := args[0]
	var err error
	switch verb {
	case "show":
		err = configShow(args[1:], stdout, stderr)
	case "reload":
		err = configReload(args[1:], stdout, stderr)
	case "diff":
		err = configDiff(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(stdout, configUsage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "tollbooth config: unknown verb %q\n%s", verb, configUsage)
		return 2
	}
	if err != nil {
		if strings.HasPrefix(err.Error(), "flag") {
			return 2
		}
		_, _ = fmt.Fprintf(stderr, "tollbooth config %s: %v\n", verb, err)
		return 1
	}
	return 0
}

func configFlags(verb string, stderr *os.File) (*flag.FlagSet, *adminFlags) {
	fs := flag.NewFlagSet("config "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	af := &adminFlags{}
	af.bind(fs)
	return fs, af
}

// fetchView gets GET /admin/config.
func fetchView(af *adminFlags) (*admin.Client, admin.ConfigView, []byte, error) {
	c, err := af.client()
	if err != nil {
		return nil, admin.ConfigView{}, nil, err
	}
	cx, cancel := ctx()
	defer cancel()
	var v struct {
		Config     map[string]any      `json:"config"`
		Generation int                 `json:"generation"`
		LoadedAt   time.Time           `json:"loaded_at"`
		LastReload *admin.ReloadResult `json:"last_reload"`
	}
	raw, err := c.Do(cx, http.MethodGet, "/admin/config", nil, &v)
	if err != nil {
		return nil, admin.ConfigView{}, nil, err
	}
	view := admin.ConfigView{Generation: v.Generation, LoadedAt: v.LoadedAt, LastReload: v.LastReload}
	return c, view, raw, nil
}

func configShow(args []string, stdout, stderr *os.File) error {
	fs, af := configFlags("show", stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, view, raw, err := fetchView(af)
	if err != nil {
		return err
	}
	var top struct {
		Config map[string]any `json:"config"`
	}
	if uErr := json.Unmarshal(raw, &top); uErr != nil {
		return uErr
	}
	g := &grid{header: []string{"KEY", "VALUE"}}
	for _, kv := range config.Flatten(top.Config) {
		g.add(kv[0], kv[1])
	}
	if af.format != "table" {
		return render(stdout, af.format, g, raw)
	}
	if rErr := render(stdout, "table", g, nil); rErr != nil {
		return rErr
	}
	last := "none"
	if view.LastReload != nil {
		if view.LastReload.OK {
			last = fmt.Sprintf("ok at %s", view.LastReload.At.UTC().Format(time.RFC3339))
		} else {
			last = fmt.Sprintf("FAILED at %s: %s", view.LastReload.At.UTC().Format(time.RFC3339), view.LastReload.Error)
		}
	}
	_, err = fmt.Fprintf(stdout, "\ngeneration %d, loaded %s; last reload: %s\n", view.Generation, view.LoadedAt.UTC().Format(time.RFC3339), last)
	return err
}

func configReload(args []string, stdout, stderr *os.File) error {
	fs, af := configFlags("reload", stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := af.client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	var res admin.ReloadResult
	raw, doErr := c.Do(cx, http.MethodPost, "/admin/reload", nil, &res)
	var ae *admin.Error
	rejected := isAdminError(doErr, &ae) && ae.Status == http.StatusUnprocessableEntity
	if doErr != nil && !rejected {
		return doErr
	}
	if doErr != nil {
		_ = json.Unmarshal(raw, &res) // the 422 body is the result too
	}
	g := &grid{header: []string{"FIELD", "VALUE"}}
	g.add("ok", fmt.Sprint(res.OK))
	g.add("generation", fmt.Sprint(res.Generation))
	g.add("rules", fmt.Sprint(res.Rules))
	g.add("duration_ms", fmt.Sprintf("%.2f", res.DurationMS))
	g.add("at", res.At.UTC().Format(time.RFC3339))
	if res.Error != "" {
		g.add("error", res.Error)
	}
	if err := render(stdout, af.format, g, raw); err != nil {
		return err
	}
	if !res.OK {
		return fmt.Errorf("rejected: %s", res.Error)
	}
	return nil
}

func isAdminError(err error, target **admin.Error) bool {
	e, ok := err.(*admin.Error)
	if ok {
		*target = e
	}
	return ok
}

// configDiff compares the running config (redacted) with the file on disk (loaded and redacted
// the same way), key by key. Exit 0 and "no differences" when they match.
func configDiff(args []string, stdout, stderr *os.File) error {
	fs, af := configFlags("diff", stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, _, raw, err := fetchView(af)
	if err != nil {
		return err
	}
	var top struct {
		Config map[string]any `json:"config"`
	}
	if uErr := json.Unmarshal(raw, &top); uErr != nil {
		return uErr
	}
	onDisk, err := config.Load(af.config)
	if err != nil {
		return fmt.Errorf("%s: %w", af.config, err)
	}
	b, err := json.Marshal(onDisk.Redacted())
	if err != nil {
		return err
	}
	var disk map[string]any
	if uErr := json.Unmarshal(b, &disk); uErr != nil {
		return uErr
	}
	running := map[string]string{}
	for _, kv := range config.Flatten(top.Config) {
		running[kv[0]] = kv[1]
	}
	file := map[string]string{}
	for _, kv := range config.Flatten(disk) {
		file[kv[0]] = kv[1]
	}
	keys := map[string]bool{}
	for k := range running {
		keys[k] = true
	}
	for k := range file {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	g := &grid{header: []string{"KEY", "RUNNING", "FILE"}}
	for _, k := range sorted {
		r, inR := running[k]
		f, inF := file[k]
		if inR && inF && r == f {
			continue
		}
		if !inR {
			r = "(absent)"
		}
		if !inF {
			f = "(absent)"
		}
		g.add(k, r, f)
	}
	type diffJSON struct {
		Key, Running, File string
	}
	out := make([]diffJSON, 0, len(g.rows))
	for _, r := range g.rows {
		out = append(out, diffJSON{r[0], r[1], r[2]})
	}
	j, _ := json.Marshal(map[string]any{"config_file": af.config, "differences": out})
	if len(g.rows) == 0 && af.format == "table" {
		_, err = fmt.Fprintf(stdout, "no differences between the running config and %s\n", af.config)
		return err
	}
	if rErr := render(stdout, af.format, g, j); rErr != nil {
		return rErr
	}
	if af.format == "table" {
		_, err = fmt.Fprintf(stdout, "\n%d keys differ; run `tollbooth config reload` to apply %s\n", len(g.rows), af.config)
	}
	return err
}
