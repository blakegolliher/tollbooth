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

package tui

import (
	"encoding/json"
	"sort"

	"github.com/blakegolliher/tollbooth/internal/config"
)

// flatten renders a JSON object as sorted dotted key paths; lists become one value (the same
// rendering as `tollbooth config show`).
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
	default:
		b, _ := json.Marshal(v)
		out = append(out, [2]string{prefix, string(b)})
	}
	return out
}

// loadFlat loads and redacts the config file on disk and flattens it like the running config.
func loadFlat(path string) (map[string]string, error) {
	c, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(c.Redacted())
	if err != nil {
		return nil, err
	}
	var generic map[string]any
	if err := json.Unmarshal(b, &generic); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, kv := range flatten("", generic) {
		out[kv[0]] = kv[1]
	}
	return out, nil
}
