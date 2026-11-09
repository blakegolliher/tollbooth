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
//
// Adapted from github.com/blakegolliher/shunt internal/config/config.go (Load, Parse,
// KeyedDecodeError, keyPath, before) at commit 9b36d09ed109add4a2bedfe1dbfae6108038ee5b
// (Apache-2.0, Copyright 2026 Blake Golliher).
// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, 2026-11-09: tollbooth's
// schema and defaults; the admin token is resolved from token_file at load; the identity
// configuration is derived once here.

package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/blakegolliher/tollbooth/internal/s3id"
)

// Error is one validation or decode finding, keyed by the dotted path of the offending key so
// check-config output points at the key, not a line number.
type Error struct {
	Key string
	Msg string
}

func (e *Error) Error() string { return e.Key + ": " + e.Msg }

// errEmpty is returned when the input has no YAML document.
var errEmpty = errors.New("config: empty document")

// Load reads, parses, applies defaults, validates, and derives a config from path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only file
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse parses, applies defaults, validates, and derives a config from bytes.
func Parse(data []byte) (*Config, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errEmpty
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errEmpty
		}
		return nil, keyedDecodeError(data, err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if err := c.derive(); err != nil {
		return nil, err
	}
	return &c, nil
}

// derive builds the values the data path uses that are not in the file: the identity
// configuration and the admin token.
func (c *Config) derive() error {
	trusted, err := trustedPrefixes(c.Listen.TrustedProxies)
	if err != nil {
		return &Error{Key: "listen.trusted_proxies", Msg: err.Error()}
	}
	c.identity = s3id.Config{Domains: s3id.NewDomains(c.BaseDomains), TrustedProxies: trusted}
	switch {
	case c.Admin.TokenFile != "":
		b, err := os.ReadFile(c.Admin.TokenFile)
		if err != nil {
			return &Error{Key: "admin.token_file", Msg: err.Error()}
		}
		c.token = strings.TrimSpace(string(b))
		if c.token == "" {
			return &Error{Key: "admin.token_file", Msg: "file is empty"}
		}
	case c.Admin.Token != "":
		c.token = c.Admin.Token
	}
	return nil
}

// keyedDecodeError turns the YAML library's positional errors into errors that name the key path
// (e.g. `proxy.drain_timeout`). Errors that carry no position keep the library's text.
func keyedDecodeError(data []byte, err error) error {
	var le *yaml.LoadErrors
	if !errors.As(err, &le) || len(le.Errors) == 0 {
		return fmt.Errorf("config: %w", err)
	}
	var root yaml.Node
	if yaml.Unmarshal(data, &root) != nil {
		return fmt.Errorf("config: %w", err)
	}
	out := make([]error, 0, len(le.Errors))
	for _, e := range le.Errors {
		key := keyPath(&root, e.Mark.Line, e.Mark.Column)
		if key == "" {
			key = fmt.Sprintf("line %d", e.Mark.Line)
		}
		msg := e.Message
		if strings.HasPrefix(msg, "field ") && strings.Contains(msg, " not found in type ") {
			msg = "unknown key"
		}
		out = append(out, &Error{Key: key, Msg: msg})
	}
	return errors.Join(out...)
}

// keyPath returns the dotted path of the deepest mapping key at or before (line, col).
// Positions are 1-indexed on both the error marks and the nodes.
func keyPath(root *yaml.Node, line, col int) string {
	n := root
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	var parts []string
	for {
		switch n.Kind {
		case yaml.MappingNode:
			var key, val *yaml.Node
			for i := 0; i+1 < len(n.Content); i += 2 {
				k := n.Content[i]
				if before(k.Line, k.Column, line, col) {
					key, val = k, n.Content[i+1]
				}
			}
			if key == nil {
				return strings.Join(parts, ".")
			}
			parts = append(parts, key.Value)
			if val.Kind == yaml.ScalarNode || val.Kind == yaml.AliasNode || !before(val.Line, val.Column, line, col) {
				return strings.Join(parts, ".")
			}
			n = val
		case yaml.SequenceNode:
			idx := -1
			for i, item := range n.Content {
				if before(item.Line, item.Column, line, col) {
					idx = i
				}
			}
			if idx < 0 {
				return strings.Join(parts, ".")
			}
			parts[len(parts)-1] += fmt.Sprintf("[%d]", idx)
			n = n.Content[idx]
			if n.Kind == yaml.ScalarNode {
				return strings.Join(parts, ".")
			}
		default:
			return strings.Join(parts, ".")
		}
	}
}

// before reports whether (l1, c1) is at or before (l2, c2) in document order.
func before(l1, c1, l2, c2 int) bool {
	return l1 < l2 || (l1 == l2 && c1 <= c2)
}
