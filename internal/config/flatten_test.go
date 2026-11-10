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
	"encoding/json"
	"reflect"
	"testing"
)

func TestFlatten(t *testing.T) {
	const doc = `{
	  "node_id": "n1",
	  "backend": {"endpoints": ["a:1", "b:2"], "tls": {"insecure_skip_verify": false}},
	  "base_domains": [],
	  "limits": [
	    {"name": "list-storm", "operation": "ListObjectsV2", "limit_per_second": 20, "per": "source_ip"},
	    {"name": "frozen", "bucket": ["a", "b"], "blocked": true}
	  ],
	  "unnamed": [{"x": 1}, {"x": 2}],
	  "twice": [{"name": "a"}, {"name": "a"}]
	}`
	var v any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatal(err)
	}
	want := [][2]string{
		{"backend.endpoints", `["a:1","b:2"]`},
		{"backend.tls.insecure_skip_verify", `false`},
		{"base_domains", `[]`},
		{"limits[list-storm].limit_per_second", `20`},
		{"limits[list-storm].name", `"list-storm"`},
		{"limits[list-storm].operation", `"ListObjectsV2"`},
		{"limits[list-storm].per", `"source_ip"`},
		{"limits[frozen].blocked", `true`},
		{"limits[frozen].bucket", `["a","b"]`},
		{"limits[frozen].name", `"frozen"`},
		{"node_id", `"n1"`},
		{"twice", `[{"name":"a"},{"name":"a"}]`},
		{"unnamed", `[{"x":1},{"x":2}]`},
	}
	if got := Flatten(v); !reflect.DeepEqual(got, want) {
		t.Errorf("Flatten:\n got %q\nwant %q", got, want)
	}
}

// The rules of a loaded config flatten by name, so `config diff` names the rule that changed.
func TestFlattenLoadedRules(t *testing.T) {
	c, err := Load("testdata/valid/rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(c.Redacted())
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	for _, kv := range Flatten(v) {
		rows[kv[0]] = kv[1]
	}
	if _, whole := rows["limits"]; whole {
		t.Fatalf("limits is one value: %s", rows["limits"])
	}
	for _, r := range c.Limits {
		if got := rows["limits["+r.Name+"].name"]; got != `"`+r.Name+`"` {
			t.Errorf("limits[%s].name = %s", r.Name, got)
		}
	}
}

func BenchmarkFlatten(b *testing.B) {
	c, err := Load("testdata/valid/rules.yaml")
	if err != nil {
		b.Fatal(err)
	}
	raw, err := json.Marshal(c.Redacted())
	if err != nil {
		b.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		Flatten(v)
	}
}
