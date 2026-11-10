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
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/blakegolliher/tollbooth/internal/admin"
	"github.com/blakegolliher/tollbooth/internal/limiter"
)

const rulesUsage = `usage: tollbooth rules <verb> [flags]

verbs:
  list                         rules in force, each read back as a sentence, with live key counts
                               and denials (GET /admin/limits)
  show <name>                  one rule in full
  test --op OP [--access-key K] [--bucket B] [--key P] [--prefix Q] [--ip A]
                               dry run: which rules match, the key each resolves to, the decision now;
                               no token is consumed (POST /admin/limits/test)
  top [--n 20] [--window 60s]  most-denied keys right now (GET /admin/limits/top)

shared flags: --admin, --token, --config, --format table|json|csv
`

// runRules dispatches the rules verbs (docs/SPEC.md, Operator surface). Each talks to the admin
// API through the shared client and renders in the chosen format.
func runRules(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, rulesUsage)
		return 2
	}
	verb := args[0]
	var err error
	switch verb {
	case "list":
		err = rulesList(args[1:], stdout, stderr)
	case "show":
		err = rulesShow(args[1:], stdout, stderr)
	case "test":
		err = rulesTest(args[1:], stdout, stderr)
	case "top":
		err = rulesTop(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(stdout, rulesUsage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "tollbooth rules: unknown verb %q\n%s", args[0], rulesUsage)
		return 2
	}
	if err != nil {
		if err == flag.ErrHelp || strings.HasPrefix(err.Error(), "flag") {
			return 2
		}
		_, _ = fmt.Fprintf(stderr, "tollbooth rules %s: %v\n", verb, err)
		return 1
	}
	return 0
}

func rulesFlags(verb string, stderr *os.File) (*flag.FlagSet, *adminFlags) {
	fs := flag.NewFlagSet("rules "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	af := &adminFlags{}
	af.bind(fs)
	return fs, af
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

func rulesList(args []string, stdout, stderr *os.File) error {
	fs, af := rulesFlags("list", stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := af.client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	var lim admin.Limits
	raw, err := c.Do(cx, http.MethodGet, "/admin/limits", nil, &lim)
	if err != nil {
		return err
	}
	g := &grid{header: []string{"NAME", "LIMIT", "PER", "OPERATION", "ONLY_FOR", "RESPOND", "KEYS", "DENIED"}}
	for i := range lim.Rules {
		r := &lim.Rules[i]
		g.add(r.Name, limitString(r), perString(r), listOrAny(r.Operation), onlyFor(r), strconv.Itoa(r.Respond), strconv.FormatInt(r.LiveKeys, 10), strconv.FormatInt(r.Denied, 10))
	}
	if af.format != "table" {
		return render(stdout, af.format, g, raw)
	}
	if rErr := render(stdout, af.format, g, raw); rErr != nil {
		return rErr
	}
	// The read-back (ADR-0007): what each rule means, in a sentence.
	if len(lim.Rules) > 0 {
		_, _ = fmt.Fprintln(stdout)
	}
	width := 0
	for i := range lim.Rules {
		width = max(width, len(lim.Rules[i].Name))
	}
	for i := range lim.Rules {
		_, _ = fmt.Fprintf(stdout, "%-*s  %s\n", width, lim.Rules[i].Name, lim.Rules[i].Sentence)
	}
	_, err = fmt.Fprintf(stdout, "\n%d rules, %d live keys\n", len(lim.Rules), lim.LiveKeys)
	return err
}

// onlyFor is the one-cell description of a rule's filters: each present one with its value or,
// for several, their count; "any" when the rule has none.
func onlyFor(r *limiter.RuleStatus) string {
	var parts []string
	add := func(name string, vs []string) {
		switch len(vs) {
		case 0:
		case 1:
			parts = append(parts, name+"="+quoteEmpty(vs[0]))
		default:
			parts = append(parts, fmt.Sprintf("%s(%d)", name, len(vs)))
		}
	}
	add("access_key", r.AccessKey)
	add("bucket", r.Bucket)
	add("prefix", r.Prefix)
	add("source_ip", r.SourceIP)
	if len(parts) == 0 {
		return "any"
	}
	return strings.Join(parts, " ")
}

func quoteEmpty(s string) string {
	if s == "" {
		return `""`
	}
	return s
}

// limitString shows the limit as written and, when node_count divides it, what this node enforces.
func limitString(r *limiter.RuleStatus) string {
	if r.Blocked {
		return "blocked"
	}
	s := r.Limit
	if r.EffectiveRate != r.RatePerSecond {
		s += " (" + strconv.FormatFloat(r.EffectiveRate, 'f', -1, 64) + "/s on this node)"
	}
	return s
}

func burstString(r *limiter.RuleStatus) string {
	if r.Blocked {
		return "-"
	}
	s := strconv.Itoa(r.Burst)
	if r.EffectiveBurst != r.Burst {
		s += " (" + strconv.Itoa(r.EffectiveBurst) + " on this node)"
	}
	return s
}

func perString(r *limiter.RuleStatus) string {
	if len(r.Per) == 0 {
		return "-"
	}
	return strings.Join(r.Per, ",")
}

// parseMixed parses flags that may appear before or after positional arguments (the flag package
// stops at the first positional), returning the positionals in order.
func parseMixed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func rulesShow(args []string, stdout, stderr *os.File) error {
	fs, af := rulesFlags("show", stderr)
	pos, err := parseMixed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: tollbooth rules show <name>")
	}
	id := pos[0]
	c, err := af.client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	var lim admin.Limits
	if _, err := c.Do(cx, http.MethodGet, "/admin/limits", nil, &lim); err != nil {
		return err
	}
	for i := range lim.Rules {
		r := &lim.Rules[i]
		if r.Name != id {
			continue
		}
		raw, _ := json.Marshal(r) //nolint:gosec // access key ids are identifiers, not secrets (G117 matches the field name)
		g := &grid{header: []string{"FIELD", "VALUE"}}
		g.add("name", r.Name)
		g.add("means", r.Sentence)
		g.add("operation", listOrAny(r.Operation))
		g.add("access_key", listOrAny(r.AccessKey))
		g.add("bucket", listOrAny(r.Bucket))
		g.add("prefix", listOrAny(r.Prefix))
		g.add("source_ip", listOrAny(r.SourceIP))
		g.add("limit", limitString(r))
		g.add("per", perString(r))
		g.add("burst", burstString(r))
		g.add("respond", strconv.Itoa(r.Respond))
		if r.Note != "" {
			g.add("note", r.Note)
		}
		g.add("live_keys", strconv.FormatInt(r.LiveKeys, 10))
		g.add("denied", strconv.FormatInt(r.Denied, 10))
		return render(stdout, af.format, g, raw)
	}
	return fmt.Errorf("no rule %q in force (%d rules loaded)", id, len(lim.Rules))
}

func listOrAny(vs []string) string {
	if len(vs) == 0 {
		return "any"
	}
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = quoteEmpty(v)
	}
	return strings.Join(out, ",")
}

func rulesTest(args []string, stdout, stderr *os.File) error {
	fs, af := rulesFlags("test", stderr)
	var req admin.TestRequest
	fs.StringVar(&req.AccessKey, "access-key", "", "access key id (empty = anonymous)")
	fs.StringVar(&req.Bucket, "bucket", "", "bucket (empty = service level)")
	fs.StringVar(&req.Key, "key", "", "object key")
	fs.StringVar(&req.Prefix, "prefix", "", "a listing's prefix parameter (ADR-0001)")
	fs.StringVar(&req.IP, "ip", "", "client source address")
	fs.StringVar(&req.Op, "op", "", "operation name from docs/OPERATIONS_TABLE.md, e.g. GetObject (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if req.Op == "" {
		return fmt.Errorf("--op is required (an operation name from docs/OPERATIONS_TABLE.md)")
	}
	c, err := af.client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	var res admin.TestResponse
	raw, err := c.Do(cx, http.MethodPost, "/admin/limits/test", &req, &res)
	if err != nil {
		return err
	}
	g := &grid{header: []string{"RULE", "KEY", "LIVE_KEYS", "VERDICT", "RETRY_AFTER", "MODE"}}
	for _, m := range res.Result.Matches {
		verdict, ra := "allow", "-"
		if !m.Allowed {
			verdict, ra = "deny", strconv.Itoa(m.RetryAfter)+"s"
		}
		g.add(m.Rule, keyDims(m.AccessKey, m.Bucket, m.Prefix, m.SourceIP), strconv.FormatInt(m.LiveKeys, 10), verdict, ra, m.Mode)
	}
	if af.format != "table" {
		return render(stdout, af.format, g, raw)
	}
	decision := "allowed"
	if !res.Result.Allowed {
		decision = fmt.Sprintf("denied by %s (%s, Retry-After %ds)", res.Result.DeniedBy, res.Result.Mode, res.Result.RetryAfter)
	}
	_, _ = fmt.Fprintf(stdout, "%s %s (%s level): %s; %d rules match\n", res.Operation, res.Class, res.Level, decision, len(res.Result.Matches))
	if len(res.Result.Matches) == 0 {
		return nil
	}
	return render(stdout, af.format, g, raw)
}

// keyDims renders the `per` dimensions a rule resolved; "(shared)" for per: total.
func keyDims(ak, bucket, prefix, ip string) string {
	var parts []string
	if ak != "" {
		parts = append(parts, "key="+ak)
	}
	if bucket != "" {
		parts = append(parts, "bucket="+bucket)
	}
	if prefix != "" {
		parts = append(parts, "prefix="+prefix)
	}
	if ip != "" {
		parts = append(parts, "ip="+ip)
	}
	if len(parts) == 0 {
		return "(shared)"
	}
	return strings.Join(parts, " ")
}

func rulesTop(args []string, stdout, stderr *os.File) error {
	fs, af := rulesFlags("top", stderr)
	n := fs.Int("n", 20, "how many keys")
	window := fs.Duration("window", 60*time.Second, "look back this far")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := af.client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	q := url.Values{"n": {strconv.Itoa(*n)}, "window": {window.String()}}
	var top admin.Top
	raw, err := c.Do(cx, http.MethodGet, "/admin/limits/top?"+q.Encode(), nil, &top)
	if err != nil {
		return err
	}
	g := &grid{header: []string{"RULE", "ACCESS_KEY", "BUCKET", "PREFIX", "SOURCE_IP", "DENIED", "LAST"}}
	for _, e := range top.Top {
		g.add(e.Rule, e.AccessKey, e.Bucket, e.Prefix, e.SourceIP, strconv.Itoa(e.Denied), fmt.Sprintf("%.0fs ago", e.LastSeconds))
	}
	if af.format == "table" && len(top.Top) == 0 {
		_, err := fmt.Fprintf(stdout, "no denials in the last %s\n", window)
		return err
	}
	return render(stdout, af.format, g, raw)
}
