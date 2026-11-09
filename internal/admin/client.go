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

package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is the one HTTP client the CLI and the TUI use for the admin API (docs/SPEC.md,
// Operator surface), so parity is structural. It never touches the data listener.
type Client struct {
	// Base is the admin listener, "http://host:port".
	Base string
	// Token is sent as a bearer token when non-empty.
	Token string
	// HTTP is the transport; nil means a client with a 10 s timeout.
	HTTP *http.Client
}

// Error is a non-2xx answer from the admin API, with the server's message when it sent JSON.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("admin API: %d %s: %s", e.Status, http.StatusText(e.Status), e.Message)
	}
	return fmt.Sprintf("admin API: %d %s", e.Status, http.StatusText(e.Status))
}

// Do sends one request and returns the raw body. in, when non-nil, is sent as JSON; out, when
// non-nil, receives the decoded body. A non-2xx status is an *Error carrying the JSON "error"
// field when present.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) ([]byte, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Base, "/")+path, body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("admin API at %s: %w", c.Base, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read fully below
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &Error{Status: resp.StatusCode}
		var msg struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &msg) == nil && msg.Error != "" {
			e.Message = msg.Error
		} else if s := strings.TrimSpace(string(raw)); s != "" && len(s) < 200 {
			e.Message = s
		}
		return raw, e
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return raw, fmt.Errorf("admin API %s: decoding response: %w", path, err)
		}
	}
	return raw, nil
}
