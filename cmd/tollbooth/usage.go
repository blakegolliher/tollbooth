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
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/showback"
)

const usageUsage = `usage: tollbooth usage <verb> [flags] [path ...]

verbs:
  query  --from T --to T [--group-by d,d] [--where d=v,d=v]   filter, group, and sum over showback files
  top    --from T --to T --by access_key|bucket|source_ip|... [--n 20]   most requests per value of one dimension
  verify [path ...]                                            recompute every file header; report missing, partial,
                                                               recovered, and mismatched hours per node

paths are directories (their usage/ and current/ trees) or globs of .ndjson files from one or many
nodes; with none, showback.dir from --config. Times are RFC 3339 (2026-11-09T00:00:00Z); --from is
inclusive, --to exclusive. Dimensions: access_key, bucket, prefix, source_ip, operation,
status_class, node_id, hour_utc. The open hour is included and marked partial. Anything beyond
filter, group, and sum is duckdb: select ... from read_ndjson('usage/**/*.ndjson', union_by_name=true).

shared flags: --config, --format table|json|csv
`

// runUsage dispatches the usage verbs (docs/SPEC.md, Operator surface). They read files; nothing
// here talks to the running proxy (GET /admin/usage shares the query code).
func runUsage(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usageUsage)
		return 2
	}
	verb := args[0]
	var err error
	switch verb {
	case "query":
		err = usageQuery(args[1:], stdout, stderr, false)
	case "top":
		err = usageQuery(args[1:], stdout, stderr, true)
	case "verify":
		err = usageVerify(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(stdout, usageUsage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "tollbooth usage: unknown verb %q\n%s", verb, usageUsage)
		return 2
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) || strings.HasPrefix(err.Error(), "flag") {
			return 2
		}
		_, _ = fmt.Fprintf(stderr, "tollbooth usage %s: %v\n", verb, err)
		return 1
	}
	return 0
}

// usagePaths resolves the positional paths, falling back to showback.dir from the config file.
func usagePaths(pos []string, cfgPath string) ([]string, error) {
	if len(pos) > 0 {
		return pos, nil
	}
	c, err := config.Load(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("no path given and %s: %w", cfgPath, err)
	}
	return []string{c.Showback.Dir}, nil
}

func parseTime(name, v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return t, fmt.Errorf("--%s: %w (want RFC 3339, like 2026-11-09T00:00:00Z)", name, err)
	}
	return t, nil
}

func usageQuery(args []string, stdout, stderr *os.File, top bool) error {
	name := "query"
	if top {
		name = "top"
	}
	fs := flag.NewFlagSet("usage "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "tollbooth.yaml", "config file, read only for showback.dir when no path is given")
	format := fs.String("format", "table", "output format: table, json, or csv")
	from := fs.String("from", "", "start, inclusive (RFC 3339)")
	to := fs.String("to", "", "end, exclusive (RFC 3339)")
	var groupBy, where, by string
	var n int
	if top {
		fs.StringVar(&by, "by", "access_key", "dimension to rank")
		fs.IntVar(&n, "n", 20, "how many values")
	} else {
		fs.StringVar(&groupBy, "group-by", "", "comma-separated dimensions")
		fs.StringVar(&where, "where", "", "comma-separated dimension=value filters (all must match)")
	}
	pos, err := parseMixed(fs, args)
	if err != nil {
		return err
	}
	if *format != "table" && *format != "json" && *format != "csv" {
		return fmt.Errorf("--format must be table, json, or csv, got %q", *format)
	}
	q := showback.Query{}
	if q.From, err = parseTime("from", *from); err != nil {
		return err
	}
	if q.To, err = parseTime("to", *to); err != nil {
		return err
	}
	if top {
		q.GroupBy = []string{by}
	} else if groupBy != "" {
		q.GroupBy = strings.Split(groupBy, ",")
	}
	if q.Where, err = showback.ParseWhere(where); err != nil {
		return err
	}
	paths, err := usagePaths(pos, *cfgPath)
	if err != nil {
		return err
	}
	files, err := showback.Files(paths...)
	if err != nil {
		return err
	}
	res, err := q.Run(files)
	if err != nil {
		return err
	}
	if top {
		sort.SliceStable(res.Groups, func(i, j int) bool { return res.Groups[i].Requests > res.Groups[j].Requests })
		if n > 0 && len(res.Groups) > n {
			res.Groups = res.Groups[:n]
		}
	}
	switch *format {
	case "json":
		b, _ := json.MarshalIndent(res, "", "  ")
		_, err = fmt.Fprintf(stdout, "%s\n", b)
		return err
	case "csv":
		_, err = stdout.Write(showback.CSV(res))
		return err
	}
	g := &grid{header: append(append([]string{}, upper(res.GroupBy)...), "REQUESTS", "BYTES_IN", "BYTES_OUT", "PARTIAL")}
	for _, gr := range res.Groups {
		row := make([]string, 0, len(res.GroupBy)+4)
		for _, d := range res.GroupBy {
			row = append(row, quoteEmpty(gr.Keys[d]))
		}
		g.add(append(row, strconv.FormatInt(gr.Requests, 10), strconv.FormatInt(gr.BytesIn, 10), strconv.FormatInt(gr.BytesOut, 10), strconv.FormatBool(gr.Partial))...)
	}
	if len(res.GroupBy) == 0 {
		g.add(strconv.FormatInt(res.Requests, 10), strconv.FormatInt(res.BytesIn, 10), strconv.FormatInt(res.BytesOut, 10), strconv.FormatBool(res.Partial))
	}
	if rErr := render(stdout, "table", g, nil); rErr != nil {
		return rErr
	}
	partial := ""
	if res.Partial {
		partial = "; includes the open hour (partial)"
	}
	_, err = fmt.Fprintf(stdout, "\n%d files, %d groups, %d requests, %d bytes in, %d bytes out%s\n", res.Files, len(res.Groups), res.Requests, res.BytesIn, res.BytesOut, partial)
	return err
}

func upper(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.ToUpper(s)
	}
	return out
}

func usageVerify(args []string, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("usage verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "tollbooth.yaml", "config file, read only for showback.dir when no path is given")
	format := fs.String("format", "table", "output format: table, json, or csv")
	pos, err := parseMixed(fs, args)
	if err != nil {
		return err
	}
	paths, err := usagePaths(pos, *cfgPath)
	if err != nil {
		return err
	}
	files, err := showback.Files(paths...)
	if err != nil {
		return err
	}
	rep := showback.Verify(files)
	if *format == "json" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		_, err = fmt.Fprintf(stdout, "%s\n", b)
	} else {
		g := &grid{header: []string{"NODE", "FIRST_HOUR", "LAST_HOUR", "SEALED", "MISSING", "PARTIAL", "RECOVERED", "MISMATCH", "UNREADABLE"}}
		for i := range rep.Nodes {
			n := &rep.Nodes[i]
			g.add(n.NodeID, n.FirstHour, n.LastHour, strconv.Itoa(n.Sealed), joinOrDash(n.Missing), joinOrDash(n.Partial), joinOrDash(n.Recovered), strconv.Itoa(len(n.Mismatch)), strconv.Itoa(len(n.Unreadable)))
		}
		if rErr := render(stdout, *format, g, nil); rErr != nil {
			return rErr
		}
		if *format == "table" {
			for i := range rep.Nodes {
				n := &rep.Nodes[i]
				for _, m := range append(append([]string{}, n.Mismatch...), n.Unreadable...) {
					_, _ = fmt.Fprintf(stdout, "  %s: %s\n", n.NodeID, m)
				}
			}
			_, err = fmt.Fprintf(stdout, "\n%d files, %d nodes, %d problems\n", rep.Files, len(rep.Nodes), rep.Problems)
		}
	}
	if err != nil {
		return err
	}
	if rep.Problems > 0 {
		return fmt.Errorf("%d problems", rep.Problems)
	}
	return nil
}

func joinOrDash(ss []string) string {
	if len(ss) == 0 {
		return "-"
	}
	if len(ss) > 3 {
		return fmt.Sprintf("%s … (%d)", strings.Join(ss[:3], " "), len(ss))
	}
	return strings.Join(ss, " ")
}
