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
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"

	"github.com/blakegolliher/tollbooth/internal/s3id"
)

// rowJSON is a data line as read. Optional dimensions are pointers so an absent one is told
// apart from an empty one (docs/SHOWBACK_SCHEMA.md: absent, not empty).
type rowJSON struct {
	Hour        int64   `json:"hour_utc"`
	NodeID      string  `json:"node_id"`
	AccessKey   *string `json:"access_key"`
	Bucket      *string `json:"bucket"`
	Prefix      *string `json:"prefix"`
	SourceIP    *string `json:"source_ip"`
	Operation   string  `json:"operation"`
	StatusClass string  `json:"status_class"`
	Requests    int64   `json:"requests"`
	BytesIn     int64   `json:"bytes_in"`
	BytesOut    int64   `json:"bytes_out"`
}

// ErrFormat is returned (wrapped) for any file that is not a tollbooth showback file.
var ErrFormat = errors.New("not a showback NDJSON file")

// maxLine bounds one line; a row is a few hundred bytes.
const maxLine = 1 << 20

// Read parses a showback file: the header line, then rows. It never panics on any input (the
// fuzz target FuzzRead pins that) and reports the first malformed line.
func Read(r io.Reader) (Header, []Row, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	var h Header
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return h, nil, err
		}
		return h, nil, fmt.Errorf("%w: empty", ErrFormat)
	}
	first := sc.Bytes()
	if !bytes.Contains(first, []byte(`"schema_version"`)) {
		return h, nil, fmt.Errorf("%w: first line is not a header", ErrFormat)
	}
	if err := json.Unmarshal(first, &h); err != nil {
		return h, nil, fmt.Errorf("%w: header: %v", ErrFormat, err)
	}
	if h.SchemaVersion != SchemaVersion {
		return h, nil, fmt.Errorf("%w: schema_version %d, this build reads %d", ErrFormat, h.SchemaVersion, SchemaVersion)
	}
	var rows []Row
	line := 1
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		var rj rowJSON
		if err := json.Unmarshal(b, &rj); err != nil {
			return h, rows, fmt.Errorf("%w: line %d: %v", ErrFormat, line, err)
		}
		row, err := rj.row()
		if err != nil {
			return h, rows, fmt.Errorf("%w: line %d: %v", ErrFormat, line, err)
		}
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		return h, rows, fmt.Errorf("%w: line %d: %v", ErrFormat, line+1, err)
	}
	return h, rows, nil
}

func (rj *rowJSON) row() (Row, error) {
	r := Row{Hour: rj.Hour, NodeID: rj.NodeID, requests: rj.Requests, bytesIn: rj.BytesIn, bytesOut: rj.BytesOut}
	op, ok := s3id.ParseOp(rj.Operation)
	if !ok {
		return r, fmt.Errorf("unknown operation %q", rj.Operation)
	}
	st, ok := ParseStatus(rj.StatusClass)
	if !ok {
		return r, fmt.Errorf("unknown status_class %q", rj.StatusClass)
	}
	r.op, r.status = op, st
	if rj.AccessKey != nil {
		r.ak, r.present = *rj.AccessKey, r.present|dimAccessKey
	}
	if rj.Bucket != nil {
		r.bucket, r.present = *rj.Bucket, r.present|dimBucket
	}
	if rj.Prefix != nil {
		r.prefix, r.present = *rj.Prefix, r.present|dimPrefix
	}
	if rj.SourceIP != nil {
		r.present |= dimSourceIP
		switch *rj.SourceIP {
		case "":
		case Overflow:
			r.ip = overflowIP
		default:
			a, err := netip.ParseAddr(*rj.SourceIP)
			if err != nil {
				return r, fmt.Errorf("source_ip: %v", err)
			}
			r.ip = a
		}
	}
	if r.requests < 0 || r.bytesIn < 0 || r.bytesOut < 0 {
		return r, errors.New("negative counter")
	}
	return r, nil
}

// ReadFile reads one file from disk.
func ReadFile(path string) (Header, []Row, error) {
	f, err := os.Open(path)
	if err != nil {
		return Header{}, nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	h, rows, err := Read(f)
	if err != nil {
		return h, rows, fmt.Errorf("%s: %w", path, err)
	}
	return h, rows, nil
}

// Dimension renders one of a row's dimensions by name for queries. ok is false when the file did
// not record it.
func (r *Row) Dimension(name string) (value string, ok bool) {
	switch name {
	case "access_key":
		return r.ak, r.present&dimAccessKey != 0
	case "bucket":
		return r.bucket, r.present&dimBucket != 0
	case "prefix":
		return r.prefix, r.present&dimPrefix != 0
	case "source_ip":
		if r.present&dimSourceIP == 0 {
			return "", false
		}
		switch {
		case r.ip == overflowIP:
			return Overflow, true
		case r.ip.IsValid():
			return r.ip.String(), true
		}
		return "", true
	case "operation":
		return r.op.String(), true
	case "status_class":
		return r.status.String(), true
	case "node_id":
		return r.NodeID, true
	case "hour_utc":
		return HourString(r.Hour), true
	}
	return "", false
}

// Dimensions a query may group or filter by.
var Dimensions = []string{"access_key", "bucket", "prefix", "source_ip", "operation", "status_class", "node_id", "hour_utc"}
