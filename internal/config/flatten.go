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
	"sort"
)

// Flatten renders a decoded JSON object (the config as GET /admin/config returns it, or Redacted
// marshaled) as dotted key paths in sorted order, each with its value as JSON. `config show`,
// `config diff`, and the TUI's Config screen all print these rows.
//
// A list is one value, except a list of named objects (the rules under `limits`): each of those is
// flattened under `limits[<name>]`, so a diff says which rule changed and which of its keys
// (`limits[list-storm].limit_per_second`), not that the whole list did.
func Flatten(v any) [][2]string { return flatten("", v) }

func flatten(prefix string, v any) [][2]string {
	var out [][2]string
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			out = append(out, flatten(p, x[k])...)
		}
		return out
	case []any:
		if names := itemNames(x); names != nil {
			for i, item := range x {
				out = append(out, flatten(prefix+"["+names[i]+"]", item)...)
			}
			return out
		}
	}
	b, _ := json.Marshal(v)
	return append(out, [2]string{prefix, string(b)})
}

// itemNames returns the `name` of every item when the list is non-empty, every item is an object
// with a non-empty string name, and no name repeats; nil otherwise.
func itemNames(list []any) []string {
	if len(list) == 0 {
		return nil
	}
	names := make([]string, len(list))
	seen := make(map[string]bool, len(list))
	for i, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil
		}
		name, ok := obj["name"].(string)
		if !ok || name == "" || seen[name] {
			return nil
		}
		names[i], seen[name] = name, true
	}
	return names
}
