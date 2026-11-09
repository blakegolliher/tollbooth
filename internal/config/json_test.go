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
	"strings"
	"testing"
)

// The JSON view (GET /admin/config, `config show --format json`) uses the file's key names,
// renders durations the way the file does, and never carries the token.
func TestMarshalJSONUsesFileKeys(t *testing.T) {
	c, err := Parse([]byte("node_id: a\nbackend: {scheme: http, endpoints: [h:1]}\nshowback: {dir: /d, retention_days: 0}\nadmin: {token: hunter2}\nrate_limits:\n  rules:\n    - id: r\n      match: {ops: [READ]}\n      limit: {rate: 2.5}\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(c.Redacted())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "NodeID") {
		t.Fatalf("%s", b)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	listen := m["listen"].(map[string]any)
	admin := m["admin"].(map[string]any)
	showback := m["showback"].(map[string]any)
	rules := m["rate_limits"].(map[string]any)["rules"].([]any)
	rule := rules[0].(map[string]any)
	if m["node_id"] != "a" || listen["read_header_timeout"] != "10s" || listen["max_inflight"] != float64(4096) || admin["token"] != "<redacted>" ||
		showback["retention_days"] != float64(0) || rule["id"] != "r" || rule["limit"].(map[string]any)["rate"] != 2.5 || rule["limit"].(map[string]any)["burst"] != float64(3) {
		t.Fatalf("%s", b)
	}
	// The original is untouched by Redacted.
	if c.AdminToken() != "hunter2" || c.Admin.Token != "hunter2" {
		t.Fatal("Redacted must copy")
	}
}
