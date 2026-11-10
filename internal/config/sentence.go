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
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The read-back (ADR-0007): every rule is shown to the operator as English generated from the
// parsed rule, so they check what tollbooth understood rather than what they typed. Sentence is
// the full form; Summary is one clause for a table cell. Both are output only; nothing parses them.

// Sentence describes a valid rule in full:
//
//	At most 20 ListObjectsV2 calls per second from each source IP. Over the limit, the client gets 503 SlowDown.
func (r *Rule) Sentence() string {
	status := statusText(r.RespondStatus())
	if r.Blocked {
		return fmt.Sprintf("Refuses every %s%s. The client gets %s.", r.callsPhrase(false), r.scopeInline(), status)
	}
	n, unit, ok := r.Count()
	if !ok {
		return "No limit is set."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "At most %s %s per %s %s", FormatCount(n), r.callsPhrase(n != 1), unit, r.perPhrase())
	switch {
	case r.Strict():
		fmt.Fprintf(&b, ", evenly spaced (one every %s)", r.Spacing())
	case r.Burst > 0 && r.Burst != int(math.Max(1, math.Ceil(n))):
		fmt.Fprintf(&b, ", up to %s at once", FormatCount(float64(r.Burst)))
	}
	b.WriteString(".")
	if scope := r.scopeInline(); scope != "" {
		fmt.Fprintf(&b, " Only for requests%s.", scope)
	}
	fmt.Fprintf(&b, " Over the limit, the client gets %s.", status)
	return b.String()
}

// Summary is the rule in one clause for a table:
//
//	ListObjectsV2: 20 per second from each source IP
func (r *Rule) Summary() string {
	what := "all requests"
	switch len(r.Operation) {
	case 0:
	case 1, 2:
		what = strings.Join(r.Operation, ", ")
	default:
		what = fmt.Sprintf("%s +%d", r.Operation[0], len(r.Operation)-1)
	}
	var limit string
	if n, unit, ok := r.Count(); ok {
		limit = fmt.Sprintf("%s per %s %s", FormatCount(n), unit, r.perPhrase())
		if r.Strict() {
			limit += ", evenly spaced"
		}
	} else {
		limit = "blocked"
	}
	var scope []string
	add := func(vals []string, one, many string) {
		switch len(vals) {
		case 0:
		case 1:
			scope = append(scope, one+" "+showValue(vals[0]))
		default:
			scope = append(scope, fmt.Sprintf("%d %s", len(vals), many))
		}
	}
	add(r.AccessKey, "key", "keys")
	add(r.SourceIP, "from", "sources")
	add(r.Bucket, "bucket", "buckets")
	add(r.Prefix, "prefix", "prefixes")
	out := what + ": " + limit
	if len(scope) > 0 {
		out += "; " + strings.Join(scope, ", ")
	}
	return out
}

// Strict reports a limit that is paced rather than budgeted: `burst: 1` on a limit above one per
// unit. One request passes at a time and the next after an even interval (20 per hour is one every
// three minutes), so the count is never exceeded in any period; without it the limit's own number
// may arrive at once and the allowance then refills at that pace.
func (r *Rule) Strict() bool {
	n, _, ok := r.Count()
	return ok && n > 1 && r.Burst == 1
}

// Spacing is the interval at which a limit's allowance refills, and so the gap between requests
// of a strict one: "3 minutes" for 20 per hour. Empty for a rule without a limit.
func (r *Rule) Spacing() string {
	n, unit, ok := r.Count()
	if !ok || n <= 0 {
		return ""
	}
	return spacing(unitSeconds(unit) / n)
}

// spacing prints an interval between requests: "3 minutes", "1 second", "50 ms".
func spacing(seconds float64) string {
	round := func(f float64) float64 { // three significant digits are plenty for a sentence
		if f == 0 {
			return 0
		}
		scale := math.Pow(10, 2-math.Floor(math.Log10(math.Abs(f))))
		return math.Round(f*scale) / scale
	}
	unit := func(f float64, one, many string) string {
		f = round(f)
		if f == 1 {
			return "1 " + one
		}
		return FormatCount(f) + " " + many
	}
	switch {
	case seconds >= 3600:
		return unit(seconds/3600, "hour", "hours")
	case seconds >= 60:
		return unit(seconds/60, "minute", "minutes")
	case seconds >= 1:
		return unit(seconds, "second", "seconds")
	case seconds >= 1e-3:
		return unit(seconds*1e3, "ms", "ms")
	case seconds >= 1e-6:
		return unit(seconds*1e6, "µs", "µs")
	}
	return "under 1 µs"
}

// LimitText is the limit alone: "20 per second", or "blocked".
func (r *Rule) LimitText() string {
	if n, unit, ok := r.Count(); ok {
		return fmt.Sprintf("%s per %s", FormatCount(n), unit)
	}
	return "blocked"
}

// callsPhrase names what is limited: "ListObjectsV2 calls", "GetObject and HeadObject calls",
// "Get* calls (33 operations)", or "requests" when the rule names no operation.
func (r *Rule) callsPhrase(plural bool) string {
	noun, req := "call", "request"
	if plural {
		noun, req = "calls", "requests"
	}
	if len(r.Operation) == 0 {
		return req
	}
	names := []string(r.Operation)
	shown := names
	more := 0
	if len(names) > 3 {
		shown, more = names[:2], len(names)-2
	}
	list := joinAnd(shown)
	if more > 0 {
		list = fmt.Sprintf("%s, and %d more", strings.Join(shown, ", "), more)
	}
	out := list + " " + noun
	family := false
	for _, n := range names {
		if strings.HasSuffix(n, "*") {
			family = true
		}
	}
	if family || more > 0 {
		out += fmt.Sprintf(" (%d operations)", len(r.Operations()))
	}
	return out
}

// perPhrase says who gets their own budget.
func (r *Rule) perPhrase() string {
	dims := sortDims(r.GroupBy())
	if len(dims) == 0 {
		return "in total"
	}
	parts := make([]string, 0, len(dims))
	for _, d := range dims {
		switch d {
		case DimAccessKey:
			parts = append(parts, "for each access key")
		case DimSourceIP:
			parts = append(parts, "from each source IP")
		case DimBucket:
			parts = append(parts, "on each bucket")
		case DimPrefix:
			parts = append(parts, "under each listed prefix")
		}
	}
	return strings.Join(parts, " ")
}

// scopeInline names the filters as a clause that follows "requests" or a call: " by access key A
// from 10.0.0.0/8 on bucket b under prefix p". Empty when the rule has no filter.
func (r *Rule) scopeInline() string {
	var b strings.Builder
	add := func(vals []string, lead, one, many string) {
		if len(vals) == 0 {
			return
		}
		shown := make([]string, len(vals))
		for i, v := range vals {
			shown[i] = showValue(v)
		}
		switch {
		case len(shown) > 3:
			fmt.Fprintf(&b, " %s %d %s (%s, …)", lead, len(shown), many, strings.Join(shown[:2], ", "))
		case one == "": // an address reads without a noun: "from 10.0.0.0/8 and 192.0.2.7"
			fmt.Fprintf(&b, " %s %s", lead, joinAnd(shown))
		case len(shown) > 1:
			fmt.Fprintf(&b, " %s %s %s", lead, many, joinAnd(shown))
		default:
			fmt.Fprintf(&b, " %s %s %s", lead, one, shown[0])
		}
	}
	add(r.AccessKey, "by", "access key", "access keys")
	add(r.SourceIP, "from", "", "sources")
	add(r.Bucket, "on", "bucket", "buckets")
	add(r.Prefix, "under", "prefix", "prefixes")
	return b.String()
}

// showValue prints a filter value; the empty string is a legal access key (anonymous requests) and
// bucket (service-level operations) and is shown as such.
func showValue(v string) string {
	if v == "" {
		return `""`
	}
	return v
}

// joinAnd joins names the way a sentence does: "A", "A and B", "A, B, and C".
func joinAnd(v []string) string {
	switch len(v) {
	case 0:
		return ""
	case 1:
		return v[0]
	case 2:
		return v[0] + " and " + v[1]
	}
	return strings.Join(v[:len(v)-1], ", ") + ", and " + v[len(v)-1]
}

// ModeStatus is the HTTP status a response mode answers with, the number `respond:` takes. The dry
// run reports the mode; operators are shown the status.
func ModeStatus(mode string) int {
	if mode == ModeTooManyRequests {
		return RespondTooManyRequests
	}
	return RespondSlowDown
}

// ModeText is ModeStatus with S3's error code beside it: "503 SlowDown" or "429 TooManyRequests".
func ModeText(mode string) string { return statusText(ModeStatus(mode)) }

func statusText(code int) string {
	if code == RespondTooManyRequests {
		return "429 TooManyRequests"
	}
	return "503 SlowDown"
}

// FormatCount prints a count the way a person writes it: 2,000 and 0.5.
func FormatCount(f float64) string {
	if f != math.Trunc(f) || math.Abs(f) >= 1e15 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	s := strconv.FormatInt(int64(f), 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
