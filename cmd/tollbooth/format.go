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

package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// grid is one tabular result: a header and rows of strings. Every verb renders through it in
// table and csv; json is the admin response passed through unwrapped (docs/SPEC.md, Operator
// surface), so scripts never screen-scrape.
type grid struct {
	header []string
	rows   [][]string
}

func (g *grid) add(cells ...string) { g.rows = append(g.rows, cells) }

// render writes the result in the chosen format. raw is the admin API's JSON body.
func render(w io.Writer, format string, g *grid, raw []byte) error {
	switch format {
	case "json":
		var buf bytes.Buffer
		if err := json.Indent(&buf, raw, "", "  "); err != nil {
			_, err = w.Write(raw)
			return err
		}
		buf.WriteByte('\n')
		_, err := w.Write(buf.Bytes())
		return err
	case "csv":
		cw := csv.NewWriter(w)
		if err := cw.Write(g.header); err != nil {
			return err
		}
		for _, r := range g.rows {
			if err := cw.Write(r); err != nil {
				return err
			}
		}
		cw.Flush()
		return cw.Error()
	default:
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		if _, err := fmt.Fprintln(tw, strings.Join(g.header, "\t")); err != nil {
			return err
		}
		for _, r := range g.rows {
			if _, err := fmt.Fprintln(tw, strings.Join(r, "\t")); err != nil {
				return err
			}
		}
		return tw.Flush()
	}
}
