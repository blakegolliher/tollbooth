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
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

const ruleBase = "node_id: a\nbackend: {scheme: http, endpoints: [h:1]}\nshowback: {dir: /d}\nlimits:\n"

// parseRule loads one rule written as the body of a `limits:` item.
func parseRule(t *testing.T, body string) *Rule {
	t.Helper()
	c, err := Parse([]byte(ruleBase + "  - " + strings.ReplaceAll(strings.TrimSpace(body), "\n", "\n    ") + "\n"))
	if err != nil {
		t.Fatalf("rule rejected: %v\n%s", err, body)
	}
	return &c.Limits[0]
}

// The read-back (ADR-0007): each rule and the exact sentence and one-line summary an operator is
// shown for it. The first case is the sentence the design started from.
func TestSentences(t *testing.T) {
	cases := []struct{ rule, sentence, summary string }{
		{
			"name: list-storm\noperation: ListObjectsV2\nlimit_per_second: 20\nper: source_ip",
			"At most 20 ListObjectsV2 calls per second from each source IP. Over the limit, the client gets 503 SlowDown.",
			"ListObjectsV2: 20 per second from each source IP",
		},
		{
			"name: shared\noperation: ListObjectsV2\nlimit_per_second: 20\nper: total",
			"At most 20 ListObjectsV2 calls per second in total. Over the limit, the client gets 503 SlowDown.",
			"ListObjectsV2: 20 per second in total",
		},
		{
			"name: one\nlimit_per_second: 1\nper: access_key\nrespond: 429",
			"At most 1 request per second for each access key. Over the limit, the client gets 429 TooManyRequests.",
			"all requests: 1 per second for each access key",
		},
		{
			"name: uploads\noperation: [PutObject, UploadPart, CompleteMultipartUpload]\naccess_key: [AKIAEXAMPLE1, AKIAEXAMPLE2]\nbucket: [training-data, checkpoints]\nlimit_per_second: 2000\nburst: 4000\nper: [bucket, access_key]",
			"At most 2,000 PutObject, UploadPart, and CompleteMultipartUpload calls per second for each access key on each bucket, up to 4,000 at once. Only for requests by access keys AKIAEXAMPLE1 and AKIAEXAMPLE2 on buckets training-data and checkpoints. Over the limit, the client gets 503 SlowDown.",
			"PutObject +2: 2,000 per second for each access key on each bucket; 2 keys, 2 buckets",
		},
		{
			"name: quota\naccess_key: AKIAEXAMPLE1\nlimit_per_hour: 20\nper: access_key",
			"At most 20 requests per hour for each access key. Only for requests by access key AKIAEXAMPLE1. Over the limit, the client gets 503 SlowDown.",
			"all requests: 20 per hour for each access key; key AKIAEXAMPLE1",
		},
		{
			"name: gets\noperation: Get*\nlimit_per_minute: 600\nper: bucket",
			"At most 600 Get* calls (33 operations) per minute on each bucket. Over the limit, the client gets 503 SlowDown.",
			"Get*: 600 per minute on each bucket",
		},
		{
			"name: many\noperation: [GetObject, HeadObject, PutObject, DeleteObject, ListObjectsV2]\nlimit_per_second: 5\nper: total",
			"At most 5 GetObject, HeadObject, and 3 more calls (5 operations) per second in total. Over the limit, the client gets 503 SlowDown.",
			"GetObject +4: 5 per second in total",
		},
		{
			"name: cold\noperation: [GetObject, HeadObject]\nprefix: cold/\nlimit_per_second: 0.5\nper: prefix",
			"At most 0.5 GetObject and HeadObject calls per second under each listed prefix. Only for requests under prefix cold/. Over the limit, the client gets 503 SlowDown.",
			"GetObject, HeadObject: 0.5 per second under each listed prefix; prefix cold/",
		},
		{
			"name: net\nsource_ip: [10.0.0.0/8, 192.0.2.7]\nlimit_per_second: 50\nper: [source_ip, bucket]",
			"At most 50 requests per second from each source IP on each bucket. Only for requests from 10.0.0.0/8 and 192.0.2.7. Over the limit, the client gets 503 SlowDown.",
			"all requests: 50 per second from each source IP on each bucket; 2 sources",
		},
		{
			"name: revoked\naccess_key: AKIAREVOKED\nblocked: true",
			"Refuses every request by access key AKIAREVOKED. The client gets 503 SlowDown.",
			"all requests: blocked; key AKIAREVOKED",
		},
		{
			"name: frozen\noperation: [PutObject, DeleteObject]\nbucket: frozen\nblocked: true\nrespond: 429\nper: access_key",
			"Refuses every PutObject and DeleteObject call on bucket frozen. The client gets 429 TooManyRequests.",
			"PutObject, DeleteObject: blocked; bucket frozen",
		},
		{
			"name: anon\naccess_key: \"\"\nblocked: true",
			`Refuses every request by access key "". The client gets 503 SlowDown.`,
			`all requests: blocked; key ""`,
		},
		{
			"name: five\naccess_key: [A, B, C, D, E]\nlimit_per_second: 10\nper: total",
			"At most 10 requests per second in total. Only for requests by 5 access keys (A, B, …). Over the limit, the client gets 503 SlowDown.",
			"all requests: 10 per second in total; 5 keys",
		},
	}
	for _, c := range cases {
		r := parseRule(t, c.rule)
		if got := r.Sentence(); got != c.sentence {
			t.Errorf("%s sentence:\n got %s\nwant %s", r.Name, got, c.sentence)
		}
		if got := r.Summary(); got != c.summary {
			t.Errorf("%s summary:\n got %s\nwant %s", r.Name, got, c.summary)
		}
	}
}

func TestResolveOperation(t *testing.T) {
	ok := map[string]struct {
		canonical string
		n         int
	}{
		"ListObjectsV2": {"ListObjectsV2", 1},
		"listobjectsv2": {"ListObjectsV2", 1},
		" GETOBJECT ":   {"GetObject", 1},
		"List*":         {"List*", 11},
		"list*":         {"List*", 11},
		"Get*":          {"Get*", 33},
		"Unknown":       {"Unknown", 1},
	}
	for in, want := range ok {
		ops, canonical, err := ResolveOperation(in)
		if err != nil || canonical != want.canonical || len(ops) != want.n {
			t.Errorf("ResolveOperation(%q) = %d ops, %q, %v; want %d, %q", in, len(ops), canonical, err, want.n, want.canonical)
		}
	}
	bad := map[string]string{
		"":             "omit `operation`",
		"*":            "omit `operation`",
		"Get*Object":   "only a trailing *",
		"Zz*":          "matches no S3 operation",
		"ListObjectV2": "did you mean ListObjectsV2?",
		"PutObjcet":    "did you mean PutObject?",
		"headobject2":  "did you mean HeadObject?",
		"READ":         "is not an S3 operation; name the operations themselves (it used to mean GetObject, HeadObject",
		"list":         "is not an S3 operation",
		"Frobnicate":   "docs/OPERATIONS_TABLE.md",
	}
	for in, want := range bad {
		if _, _, err := ResolveOperation(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ResolveOperation(%q) error %v, want …%s…", in, err, want)
		}
	}
	if n := len(OperationNames()); n != 113 {
		t.Errorf("OperationNames has %d entries, want the 113 operations of docs/OPERATIONS_TABLE.md", n)
	}
}

// A rule written by tollbooth (the TUI's form, GET /admin/config) reads back as the same rule: one
// value as a scalar, several as an inline list, whole numbers without an exponent, unset keys absent.
func TestRuleYAMLRoundTrip(t *testing.T) {
	src := parseRule(t, "name: r\noperation: [GetObject, HeadObject]\nbucket: \"no\"\naccess_key: 1234567890\nprefix: \"a, b/\"\nlimit_per_minute: 1000000\nburst: 7\nper: [access_key, bucket]\nrespond: 429\nnote: \"why: INC-1\"")
	out, err := yaml.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{"name: r\n", "operation: [GetObject, HeadObject]\n", "limit_per_minute: 1000000\n", "per: [access_key, bucket]\n", "respond: 429\n", "burst: 7\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("marshaled rule lacks %q:\n%s", want, text)
		}
	}
	for _, absent := range []string{"limit_per_second", "limit_per_hour", "blocked", "source_ip", "1e+06"} {
		if strings.Contains(text, absent) {
			t.Errorf("marshaled rule has %q:\n%s", absent, text)
		}
	}
	var back Rule
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("%v\n%s", err, text)
	}
	if back.Sentence() != src.Sentence() || back.Bucket[0] != "no" || back.AccessKey[0] != "1234567890" || back.Prefix[0] != "a, b/" || back.Note != "why: INC-1" {
		t.Fatalf("round trip changed the rule:\n%s\n%+v", text, back)
	}
	half := parseRule(t, "name: h\nlimit_per_second: 0.5\nper: total")
	if out, _ := yaml.Marshal(half); !strings.Contains(string(out), "limit_per_second: 0.5\n") || !strings.Contains(string(out), "per: total\n") {
		t.Errorf("%s", out)
	}
}

func TestCountRateAndUnits(t *testing.T) {
	for body, want := range map[string]struct {
		rate float64
		text string
	}{
		"limit_per_second: 20\nper: total":    {20, "20 per second"},
		"limit_per_minute: 600\nper: total":   {10, "600 per minute"},
		"limit_per_hour: 7200\nper: total":    {2, "7,200 per hour"},
		"limit_per_second: 2.5\nper: total":   {2.5, "2.5 per second"},
		"blocked: true":                       {0, "blocked"},
		"limit_per_minute: 0.6\nper: total":   {0.01, "0.6 per minute"},
		"limit_per_hour: 1000000\nper: total": {1000000.0 / 3600, "1,000,000 per hour"},
	} {
		r := parseRule(t, "name: r\n"+body)
		if got := r.Rate(); got < want.rate*0.999999 || got > want.rate*1.000001 {
			t.Errorf("%q: rate %v, want %v", body, got, want.rate)
		}
		if got := r.LimitText(); got != want.text {
			t.Errorf("%q: %q, want %q", body, got, want.text)
		}
	}
	if FormatCount(1234567.5) != "1234567.5" || FormatCount(-1500) != "-1,500" || FormatCount(999) != "999" || FormatCount(1000) != "1,000" {
		t.Errorf("FormatCount: %s %s %s %s", FormatCount(1234567.5), FormatCount(-1500), FormatCount(999), FormatCount(1000))
	}
}

func TestEditDistance(t *testing.T) {
	for _, c := range []struct {
		a, b string
		d    int
	}{{"", "", 0}, {"abc", "abc", 0}, {"abc", "", 3}, {"listobjectv2", "listobjectsv2", 1}, {"kitten", "sitting", 3}} {
		if got := editDistance(c.a, c.b); got != c.d {
			t.Errorf("editDistance(%q, %q) = %d, want %d", c.a, c.b, got, c.d)
		}
	}
}

func BenchmarkSentence(b *testing.B) {
	c, err := Load("testdata/valid/rules.yaml")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		for i := range c.Limits {
			_ = c.Limits[i].Sentence()
		}
	}
}
