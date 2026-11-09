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

package showback

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Query is filter, group, and sum over a set of files, and nothing else (docs/SPEC.md,
// Showback, Query interface). From is inclusive and To exclusive, both truncated to the hour.
type Query struct {
	From, To time.Time
	GroupBy  []string          // dimensions from Dimensions; empty = one total row
	Where    map[string]string // dimension → exact value, all must match
}

// Group is one result row.
type Group struct {
	Keys     map[string]string `json:"keys"`
	Requests int64             `json:"requests"`
	BytesIn  int64             `json:"bytes_in"`
	BytesOut int64             `json:"bytes_out"`
	// Partial is true when an open (current/) hour contributed.
	Partial bool `json:"partial"`
}

// Result is a query's answer.
type Result struct {
	From, To time.Time `json:"-"`
	FromHour int64     `json:"from_hour_utc"`
	ToHour   int64     `json:"to_hour_utc"`
	GroupBy  []string  `json:"group_by"`
	Files    int       `json:"files"`
	Groups   []Group   `json:"groups"`
	Requests int64     `json:"requests"`
	BytesIn  int64     `json:"bytes_in"`
	BytesOut int64     `json:"bytes_out"`
	Partial  bool      `json:"partial"`
}

// Files lists every showback file under each path: a directory (its usage/ and current/ trees,
// or the directory itself when it holds .ndjson files) or a glob. Files are returned sorted.
func Files(paths ...string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if strings.HasSuffix(p, ".ndjson") && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			err := filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() {
					add(path)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			continue
		}
		matches, err := filepath.Glob(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("%s: no such directory or file", p)
		}
		for _, m := range matches {
			add(m)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Run executes the query over the files. A group_by or where dimension a file did not record is
// an error naming the file, not an empty group.
func (q Query) Run(files []string) (*Result, error) {
	for _, d := range q.GroupBy {
		if !validDimension(d) {
			return nil, fmt.Errorf("group_by: unknown dimension %q (one of %s)", d, strings.Join(Dimensions, ", "))
		}
	}
	for d := range q.Where {
		if !validDimension(d) {
			return nil, fmt.Errorf("where: unknown dimension %q (one of %s)", d, strings.Join(Dimensions, ", "))
		}
	}
	res := &Result{From: q.From, To: q.To, GroupBy: q.GroupBy, Groups: []Group{}}
	if !q.From.IsZero() {
		res.FromHour = q.From.Unix() / 3600
	}
	if q.To.IsZero() {
		res.ToHour = 1<<62 - 1
	} else {
		res.ToHour = q.To.Unix() / 3600
		if q.To.Unix()%3600 != 0 {
			res.ToHour++ // a To inside an hour includes that hour
		}
	}
	groups := map[string]*Group{}
	keyBuf := make([]string, 0, len(q.GroupBy))
	for _, f := range files {
		hour, ok := hourOfFile(f)
		if ok && (hour < res.FromHour || hour >= res.ToHour) {
			continue
		}
		h, rows, err := ReadFile(f)
		if err != nil {
			return nil, err
		}
		if h.Hour < res.FromHour || h.Hour >= res.ToHour {
			continue
		}
		res.Files++
		for i := range rows {
			r := &rows[i]
			match := true
			for d, want := range q.Where {
				v, ok := r.Dimension(d)
				if !ok {
					return nil, fmt.Errorf("%s: dimension %q was not recorded in this file", f, d)
				}
				if v != want {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			keyBuf = keyBuf[:0]
			for _, d := range q.GroupBy {
				v, ok := r.Dimension(d)
				if !ok {
					return nil, fmt.Errorf("%s: dimension %q was not recorded in this file", f, d)
				}
				keyBuf = append(keyBuf, v)
			}
			k := strings.Join(keyBuf, "\x00")
			g := groups[k]
			if g == nil {
				g = &Group{Keys: map[string]string{}}
				for i, d := range q.GroupBy {
					g.Keys[d] = keyBuf[i]
				}
				groups[k] = g
			}
			g.Requests += r.requests
			g.BytesIn += r.bytesIn
			g.BytesOut += r.bytesOut
			g.Partial = g.Partial || h.Partial
			res.Requests += r.requests
			res.BytesIn += r.bytesIn
			res.BytesOut += r.bytesOut
			res.Partial = res.Partial || h.Partial
		}
	}
	for _, g := range groups {
		res.Groups = append(res.Groups, *g)
	}
	sort.Slice(res.Groups, func(i, j int) bool {
		for _, d := range q.GroupBy {
			if res.Groups[i].Keys[d] != res.Groups[j].Keys[d] {
				return res.Groups[i].Keys[d] < res.Groups[j].Keys[d]
			}
		}
		return false
	})
	return res, nil
}

// CSV renders a result as rows of the group keys, requests, bytes_in, bytes_out, partial.
func CSV(res *Result) []byte {
	var b strings.Builder
	for _, d := range res.GroupBy {
		b.WriteString(csvField(d))
		b.WriteByte(',')
	}
	b.WriteString("requests,bytes_in,bytes_out,partial\n")
	for _, g := range res.Groups {
		for _, d := range res.GroupBy {
			b.WriteString(csvField(g.Keys[d]))
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d,%d,%d,%t\n", g.Requests, g.BytesIn, g.BytesOut, g.Partial)
	}
	if len(res.GroupBy) == 0 {
		fmt.Fprintf(&b, "%d,%d,%d,%t\n", res.Requests, res.BytesIn, res.BytesOut, res.Partial)
	}
	return []byte(b.String())
}

func csvField(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

func validDimension(d string) bool {
	for _, x := range Dimensions {
		if x == d {
			return true
		}
	}
	return false
}

// hourOfFile reads the hour from a file's name so out-of-range files are skipped unopened; ok is
// false for a name that is not one of ours, in which case the header decides.
func hourOfFile(path string) (int64, bool) {
	dir := filepath.Base(filepath.Dir(path))
	if dir == "current" {
		h, _, ok := parseCurrentName(path)
		return h, ok
	}
	// usage/YYYY/MM/DD/HH-node.ndjson: the root is four levels up.
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(path))))
	h, _, ok := parseSealedName(root, path)
	return h, ok
}

// ParseWhere parses "dim=value,dim=value".
func ParseWhere(s string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("where: %q is not dimension=value", part)
		}
		out[k] = v
	}
	return out, nil
}

// VerifyReport is the result of Verify: per node, the hours covered and every finding.
type VerifyReport struct {
	Nodes    []NodeReport `json:"nodes"`
	Files    int          `json:"files"`
	Problems int          `json:"problems"`
}

// NodeReport is one node's verification.
type NodeReport struct {
	NodeID     string   `json:"node_id"`
	FirstHour  string   `json:"first_hour,omitempty"`
	LastHour   string   `json:"last_hour,omitempty"`
	Sealed     int      `json:"sealed_hours"`
	Missing    []string `json:"missing_hours"`   // downtime: no file between first and last
	Partial    []string `json:"partial_hours"`   // current/ snapshots (the open hour)
	Recovered  []string `json:"recovered_hours"` // sealed at startup from an earlier hour's snapshot
	Mismatch   []string `json:"header_mismatch"` // header sums differ from the rows
	Unreadable []string `json:"unreadable"`
}

// Verify recomputes every header and reports, per node, missing hours (downtime), partial and
// recovered hours, and header mismatches (docs/SPEC.md, Showback, Query interface).
func Verify(files []string) *VerifyReport {
	rep := &VerifyReport{}
	type nodeState struct {
		rep   *NodeReport
		hours map[int64]bool
	}
	nodes := map[string]*nodeState{}
	get := func(id string) *nodeState {
		n := nodes[id]
		if n == nil {
			n = &nodeState{rep: &NodeReport{NodeID: id, Missing: []string{}, Partial: []string{}, Recovered: []string{}, Mismatch: []string{}, Unreadable: []string{}}, hours: map[int64]bool{}}
			nodes[id] = n
		}
		return n
	}
	for _, f := range files {
		rep.Files++
		h, rows, err := ReadFile(f)
		if err != nil {
			id := h.NodeID
			if id == "" {
				if _, n, ok := parseCurrentName(f); ok {
					id = n
				} else {
					id = "?"
				}
			}
			get(id).rep.Unreadable = append(get(id).rep.Unreadable, f+": "+errors.Unwrap(err).Error())
			rep.Problems++
			continue
		}
		n := get(h.NodeID)
		var reqs, in, out int64
		for i := range rows {
			reqs += rows[i].requests
			in += rows[i].bytesIn
			out += rows[i].bytesOut
		}
		if int64(len(rows)) != h.Rows || reqs != h.Requests || in != h.BytesIn || out != h.BytesOut {
			n.rep.Mismatch = append(n.rep.Mismatch, fmt.Sprintf("%s: header rows=%d requests=%d bytes_in=%d bytes_out=%d, rows say %d/%d/%d/%d", f, h.Rows, h.Requests, h.BytesIn, h.BytesOut, len(rows), reqs, in, out))
			rep.Problems++
		}
		switch {
		case h.Partial:
			n.rep.Partial = append(n.rep.Partial, HourString(h.Hour))
		default:
			n.rep.Sealed++
			n.hours[h.Hour] = true
			if h.Recovered {
				n.rep.Recovered = append(n.rep.Recovered, HourString(h.Hour))
			}
		}
	}
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		n := nodes[id]
		if len(n.hours) > 0 {
			first, last := int64(1<<62), int64(-1)
			for h := range n.hours {
				first, last = min(first, h), max(last, h)
			}
			n.rep.FirstHour, n.rep.LastHour = HourString(first), HourString(last)
			for h := first; h <= last; h++ {
				if !n.hours[h] {
					n.rep.Missing = append(n.rep.Missing, HourString(h))
					rep.Problems++
				}
			}
		}
		rep.Nodes = append(rep.Nodes, *n.rep)
	}
	return rep
}
