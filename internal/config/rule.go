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
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/blakegolliher/tollbooth/internal/s3id"
)

// Rule is one limit, written in S3's own words (ADR-0007): which requests (operation, access_key,
// bucket, prefix, source_ip), how many (exactly one of limit_per_second, limit_per_minute,
// limit_per_hour, or blocked), counted how (per), and what the client gets over the limit
// (respond). The fields are the file's keys; the methods below turn them into what the limiter
// enforces. Validation is in validate.go; compilation into matchers and GCRA state is
// internal/limiter's.
type Rule struct {
	Name           string     `yaml:"name"`
	Operation      StringList `yaml:"operation,omitempty"`
	AccessKey      StringList `yaml:"access_key,omitempty"`
	Bucket         StringList `yaml:"bucket,omitempty"`
	Prefix         StringList `yaml:"prefix,omitempty"`
	SourceIP       StringList `yaml:"source_ip,omitempty"`
	LimitPerSecond *Number    `yaml:"limit_per_second,omitempty"`
	LimitPerMinute *Number    `yaml:"limit_per_minute,omitempty"`
	LimitPerHour   *Number    `yaml:"limit_per_hour,omitempty"`
	Blocked        bool       `yaml:"blocked,omitempty"`
	Burst          int        `yaml:"burst,omitempty"`
	Per            StringList `yaml:"per,omitempty"`
	Respond        int        `yaml:"respond,omitempty"`
	Note           string     `yaml:"note,omitempty"`
}

// PerTotal is the `per` value for one budget shared by every request the rule matches.
const PerTotal = "total"

// The statuses `respond` accepts: what S3 itself answers (SlowDown) and the alternative.
const (
	RespondSlowDown        = 503
	RespondTooManyRequests = 429
)

// Units a limit is written in, as the sentence prints them.
const (
	UnitSecond = "second"
	UnitMinute = "minute"
	UnitHour   = "hour"
)

// StringList is a key that takes one value or a list: `bucket: logs` and `bucket: [logs, tmp]`.
type StringList []string

// UnmarshalYAML accepts a scalar or a sequence of scalars. Values are taken as written, so an
// access key of digits or a bucket named `no` stays a string.
func (s *StringList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
			*s = nil
			return nil
		}
		*s = StringList{n.Value}
	case yaml.SequenceNode:
		out := make(StringList, 0, len(n.Content))
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return errors.New("every item must be a plain value")
			}
			out = append(out, c.Value)
		}
		*s = out
	default:
		return errors.New("must be one value or a list of values")
	}
	return nil
}

// MarshalYAML writes one value as a scalar and several as an inline list.
func (s StringList) MarshalYAML() (any, error) {
	if len(s) == 1 {
		return s[0], nil
	}
	n := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
	for _, v := range s {
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v})
	}
	return n, nil
}

// Number is a limit's count. It reads any YAML number and writes whole numbers without an
// exponent (the encoder would print one million as 1e+06).
type Number float64

// UnmarshalYAML reads an integer or a decimal.
func (x *Number) UnmarshalYAML(n *yaml.Node) error {
	var f float64
	if err := n.Decode(&f); err != nil {
		return errors.New("must be a number")
	}
	*x = Number(f)
	return nil
}

// MarshalYAML writes 20 as 20 and 0.5 as 0.5.
func (x Number) MarshalYAML() (any, error) {
	f := float64(x)
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return int64(f), nil
	}
	return f, nil
}

// Count and Unit return the limit as written: (20, "second"). ok is false for a blocked rule and
// for a rule that names no limit or more than one (validation rejects both).
func (r *Rule) Count() (n float64, unit string, ok bool) {
	set := 0
	if r.LimitPerSecond != nil {
		n, unit, set = float64(*r.LimitPerSecond), UnitSecond, set+1
	}
	if r.LimitPerMinute != nil {
		n, unit, set = float64(*r.LimitPerMinute), UnitMinute, set+1
	}
	if r.LimitPerHour != nil {
		n, unit, set = float64(*r.LimitPerHour), UnitHour, set+1
	}
	if set != 1 || r.Blocked {
		return 0, "", false
	}
	return n, unit, true
}

// unitSeconds is the length of a unit.
func unitSeconds(unit string) float64 {
	switch unit {
	case UnitMinute:
		return 60
	case UnitHour:
		return 3600
	}
	return 1
}

// Rate is the limit in requests per second, 0 for a blocked rule.
func (r *Rule) Rate() float64 {
	n, unit, ok := r.Count()
	if !ok {
		return 0
	}
	return n / unitSeconds(unit)
}

// BurstSize is how many requests may arrive at once: `burst` when set, otherwise the limit's own
// number, so "20 per hour" lets 20 through at once and refills evenly over the hour.
func (r *Rule) BurstSize() int {
	if r.Burst > 0 {
		return r.Burst
	}
	n, _, ok := r.Count()
	if !ok {
		return 0
	}
	b := int(math.Ceil(n))
	if b < 1 {
		b = 1
	}
	return b
}

// GroupBy is `per` as the limiter's key dimensions: empty for `total`.
func (r *Rule) GroupBy() []string {
	out := make([]string, 0, len(r.Per))
	for _, p := range r.Per {
		if p != PerTotal {
			out = append(out, p)
		}
	}
	return out
}

// Mode is `respond` as the limiter's response mode.
func (r *Rule) Mode() string {
	if r.Respond == RespondTooManyRequests {
		return ModeTooManyRequests
	}
	return ModeSlowDown
}

// RespondStatus is `respond` with its default applied.
func (r *Rule) RespondStatus() int {
	if r.Respond == RespondTooManyRequests {
		return RespondTooManyRequests
	}
	return RespondSlowDown
}

// Operations expands `operation` into S3 operation names: nil when the key is absent (every
// operation), otherwise each name or family resolved, in table order, without repeats. Entries
// that resolve to nothing are skipped; validation reports them.
func (r *Rule) Operations() []string {
	if len(r.Operation) == 0 {
		return nil
	}
	seen := make(map[s3id.Op]bool)
	for _, entry := range r.Operation {
		ops, _, _ := ResolveOperation(entry)
		for _, op := range ops {
			seen[op] = true
		}
	}
	out := make([]string, 0, len(seen))
	for _, op := range s3id.Ops() {
		if seen[op] {
			out = append(out, op.String())
		}
	}
	return out
}

// ResolveOperation resolves one `operation` entry: an S3 operation name in any letter case, or a
// family written with a trailing star (`Get*`). canonical is the entry in S3's spelling. The
// error says what to write instead: the closest name for a typo, the member operations for one
// of the old class words.
func ResolveOperation(entry string) (ops []s3id.Op, canonical string, err error) {
	e := strings.TrimSpace(entry)
	switch {
	case e == "":
		return nil, "", errors.New("empty; omit `operation` to match every operation")
	case e == "*":
		return nil, "", errors.New("omit `operation` to match every operation")
	case strings.Contains(strings.TrimSuffix(e, "*"), "*"):
		return nil, "", fmt.Errorf("%q: only a trailing * is a wildcard, as in Get*", e)
	}
	if strings.HasSuffix(e, "*") {
		prefix := strings.TrimSuffix(e, "*")
		for _, op := range s3id.Ops() {
			name := op.String()
			if len(name) >= len(prefix) && strings.EqualFold(name[:len(prefix)], prefix) {
				if ops == nil {
					canonical = name[:len(prefix)] + "*"
				}
				ops = append(ops, op)
			}
		}
		if ops == nil {
			return nil, "", fmt.Errorf("%q matches no S3 operation", e)
		}
		return ops, canonical, nil
	}
	for _, op := range s3id.Ops() {
		if strings.EqualFold(op.String(), e) {
			return []s3id.Op{op}, op.String(), nil
		}
	}
	// One of the class words tollbooth's rules used before ADR-0007, written the way they were.
	if c, ok := s3id.ParseClass(e); ok {
		var members []string
		for _, op := range s3id.Ops() {
			if op.Class() == c {
				members = append(members, op.String())
			}
		}
		return nil, "", fmt.Errorf("%q is not an S3 operation; name the operations themselves (it used to mean %s)", e, joinSome(members, 6))
	}
	// The start of several names: the operator is partway to one of them, or wants the family.
	var starts []string
	for _, op := range s3id.Ops() {
		if name := op.String(); len(name) > len(e) && strings.EqualFold(name[:len(e)], e) {
			starts = append(starts, name)
		}
	}
	if len(starts) > 1 {
		family := starts[0][:len(e)] + "*"
		return nil, "", fmt.Errorf("%q is the start of %d S3 operations (%s); name one, or write %s for all of them", e, len(starts), joinSome(starts, 3), family)
	}
	if s := closestOperation(e); s != "" {
		return nil, "", fmt.Errorf("unknown S3 operation %q; did you mean %s?", e, s)
	}
	return nil, "", fmt.Errorf("unknown S3 operation %q (docs/OPERATIONS_TABLE.md lists them)", e)
}

// OperationNames lists every S3 operation tollbooth classifies, in table order.
func OperationNames() []string {
	all := s3id.Ops()
	out := make([]string, 0, len(all))
	for _, op := range all {
		out = append(out, op.String())
	}
	return out
}

// closestOperation returns the operation name nearest to s by edit distance, or "" when nothing
// is close enough to be a typo of it.
func closestOperation(s string) string {
	low := strings.ToLower(s)
	best, bestD := "", len(low)/3+2
	for _, op := range s3id.Ops() {
		name := op.String()
		if d := editDistance(low, strings.ToLower(name)); d < bestD {
			best, bestD = name, d
		}
	}
	return best
}

// editDistance is the Levenshtein distance between two short ASCII strings.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// joinSome joins up to n names and says how many more there are.
func joinSome(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(names[:n], ", "), len(names)-n)
}

// canonicalize rewrites `operation` entries into S3's spelling and sorts nothing: the order is the
// operator's. Entries that do not resolve are left for validation to report.
func (r *Rule) canonicalize() {
	if len(r.Operation) == 0 {
		return
	}
	out := make(StringList, len(r.Operation)) // a fresh list: the caller's is not written through
	for i, entry := range r.Operation {
		out[i] = entry
		if _, canonical, err := ResolveOperation(entry); err == nil {
			out[i] = canonical
		}
	}
	r.Operation = out
}

// validPer reports whether p is a `per` value, and sortDims orders dimensions the way the sentence
// names them.
func validPer(p string) bool {
	switch p {
	case PerTotal, DimAccessKey, DimBucket, DimPrefix, DimSourceIP:
		return true
	}
	return false
}

var perOrder = map[string]int{DimAccessKey: 0, DimSourceIP: 1, DimBucket: 2, DimPrefix: 3}

func sortDims(dims []string) []string {
	out := append([]string(nil), dims...)
	sort.SliceStable(out, func(i, j int) bool { return perOrder[out[i]] < perOrder[out[j]] })
	return out
}
