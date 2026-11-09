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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SchemaVersion is the header's schema_version (docs/SHOWBACK_SCHEMA.md).
const SchemaVersion = 1

// Header is line 1 of every file.
type Header struct {
	SchemaVersion int    `json:"schema_version"`
	NodeID        string `json:"node_id"`
	Hour          int64  `json:"hour_utc"`
	Rows          int64  `json:"rows"`
	Requests      int64  `json:"requests"`
	BytesIn       int64  `json:"bytes_in"`
	BytesOut      int64  `json:"bytes_out"`
	Partial       bool   `json:"partial"`
	Recovered     bool   `json:"recovered"`
	WrittenAt     int64  `json:"written_at"`
}

// Row is one data line: the hour, the node, the tuple, and the three counters. Dimensions not
// configured are absent from the file; Row carries which are present through the reader.
type Row struct {
	Hour   int64
	NodeID string
	tuple
	requests, bytesIn, bytesOut int64
	// present says which optional dimensions the file carried (reader side).
	present uint8
}

// Requests is the request count.
func (r *Row) Requests() int64 { return r.requests }

// BytesIn is request body bytes read from the client.
func (r *Row) BytesIn() int64 { return r.bytesIn }

// BytesOut is response body bytes written to the client.
func (r *Row) BytesOut() int64 { return r.bytesOut }

func (r *Row) less(o *Row) bool {
	if r.ak != o.ak {
		return r.ak < o.ak
	}
	if r.bucket != o.bucket {
		return r.bucket < o.bucket
	}
	if r.prefix != o.prefix {
		return r.prefix < o.prefix
	}
	if r.ip != o.ip {
		return r.ip.Less(o.ip)
	}
	if r.op != o.op {
		return r.op < o.op
	}
	return r.status < o.status
}

// appendRow encodes one row as a JSON line with exactly the configured dimensions.
func appendRow(b []byte, r *Row, dims uint8) []byte {
	b = append(b, `{"hour_utc":`...)
	b = strconv.AppendInt(b, r.Hour, 10)
	b = append(b, `,"node_id":`...)
	b = strconv.AppendQuote(b, r.NodeID)
	if dims&dimAccessKey != 0 {
		b = append(b, `,"access_key":`...)
		b = strconv.AppendQuote(b, r.ak)
	}
	if dims&dimBucket != 0 {
		b = append(b, `,"bucket":`...)
		b = strconv.AppendQuote(b, r.bucket)
	}
	if dims&dimPrefix != 0 {
		b = append(b, `,"prefix":`...)
		b = strconv.AppendQuote(b, r.prefix)
	}
	if dims&dimSourceIP != 0 {
		b = append(b, `,"source_ip":`...)
		switch {
		case r.ip == overflowIP:
			b = strconv.AppendQuote(b, Overflow)
		case r.ip.IsValid():
			b = strconv.AppendQuote(b, r.ip.String())
		default:
			b = append(b, `""`...)
		}
	}
	b = append(b, `,"operation":`...)
	b = strconv.AppendQuote(b, r.op.String())
	b = append(b, `,"status_class":`...)
	b = strconv.AppendQuote(b, r.status.String())
	b = append(b, `,"requests":`...)
	b = strconv.AppendInt(b, r.requests, 10)
	b = append(b, `,"bytes_in":`...)
	b = strconv.AppendInt(b, r.bytesIn, 10)
	b = append(b, `,"bytes_out":`...)
	b = strconv.AppendInt(b, r.bytesOut, 10)
	return append(b, "}\n"...)
}

func appendHeader(b []byte, h *Header) []byte {
	b = append(b, `{"schema_version":`...)
	b = strconv.AppendInt(b, int64(h.SchemaVersion), 10)
	b = append(b, `,"node_id":`...)
	b = strconv.AppendQuote(b, h.NodeID)
	b = append(b, `,"hour_utc":`...)
	b = strconv.AppendInt(b, h.Hour, 10)
	b = append(b, `,"rows":`...)
	b = strconv.AppendInt(b, h.Rows, 10)
	b = append(b, `,"requests":`...)
	b = strconv.AppendInt(b, h.Requests, 10)
	b = append(b, `,"bytes_in":`...)
	b = strconv.AppendInt(b, h.BytesIn, 10)
	b = append(b, `,"bytes_out":`...)
	b = strconv.AppendInt(b, h.BytesOut, 10)
	b = append(b, `,"partial":`...)
	b = strconv.AppendBool(b, h.Partial)
	b = append(b, `,"recovered":`...)
	b = strconv.AppendBool(b, h.Recovered)
	b = append(b, `,"written_at":`...)
	b = strconv.AppendInt(b, h.WrittenAt, 10)
	return append(b, "}\n"...)
}

// encode renders header plus rows; the header's sums are computed from the rows.
func encode(buf []byte, nodeID string, hour int64, rows []Row, dims uint8, partial, recovered bool, writtenAt time.Time) []byte {
	h := Header{SchemaVersion: SchemaVersion, NodeID: nodeID, Hour: hour, Rows: int64(len(rows)), Partial: partial, Recovered: recovered, WrittenAt: writtenAt.Unix()}
	for i := range rows {
		h.Requests += rows[i].requests
		h.BytesIn += rows[i].bytesIn
		h.BytesOut += rows[i].bytesOut
	}
	buf = appendHeader(buf[:0], &h)
	for i := range rows {
		buf = appendRow(buf, &rows[i], dims)
	}
	return buf
}

// CurrentPath is current/<hour>-<node>.ndjson under dir.
func CurrentPath(dir, nodeID string, hour int64) string {
	return filepath.Join(dir, "current", strconv.FormatInt(hour, 10)+"-"+nodeID+".ndjson")
}

// SealedPath is usage/YYYY/MM/DD/HH-<node>.ndjson under dir, the hour rendered in UTC.
func SealedPath(dir, nodeID string, hour int64) string {
	t := time.Unix(hour*3600, 0).UTC()
	return filepath.Join(dir, "usage", t.Format("2006"), t.Format("01"), t.Format("02"), t.Format("15")+"-"+nodeID+".ndjson")
}

// writeAtomic writes b to path through a temp file, fsyncs, and renames.
func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// Store is the writer: snapshots of the open hour every flush interval, the seal at the hour
// boundary, restart recovery, and retention (docs/DESIGN.md §6.2–6.3). One goroutine; it never
// blocks a request.
type Store struct {
	Agg      *Aggregator
	Dir      string
	NodeID   string
	Interval time.Duration
	// Retention in days; 0 never deletes.
	Retention int
	Log       *slog.Logger
	m         *Metrics

	mu       sync.Mutex // serializes snapshot, seal, and Flush
	buf      []byte
	rows     []Row
	lastErr  time.Time
	lastSwep time.Time
	now      func() time.Time
}

// NewStore wires the writer. m may be nil.
func NewStore(agg *Aggregator, dir, nodeID string, interval time.Duration, retentionDays int, m *Metrics, log *slog.Logger) *Store {
	s := &Store{Agg: agg, Dir: dir, NodeID: nodeID, Interval: interval, Retention: retentionDays, Log: log, m: m, now: agg.now}
	if m == nil {
		s.m = &Metrics{}
	}
	if log == nil {
		s.Log = slog.New(slog.DiscardHandler)
	}
	return s
}

// Recover reads this node's current/ snapshot at startup (docs/DESIGN.md §6.3): the current hour
// is loaded into the open table and counting continues; an earlier hour is sealed as recovered.
// Other nodes' files are left alone. It also seals any stray .tmp files away.
func (s *Store) Recover() error {
	matches, _ := filepath.Glob(filepath.Join(s.Dir, "current", "*-"+s.NodeID+".ndjson"))
	for _, p := range matches {
		hdr, rows, err := ReadFile(p)
		if err != nil {
			s.Log.Error("showback: unreadable snapshot left in place", "path", p, "err", err.Error())
			continue
		}
		if hdr.NodeID != s.NodeID {
			continue
		}
		cur := s.Agg.cur.Load()
		switch {
		case hdr.Hour == cur.hour:
			s.Agg.load(cur, rows)
			s.Log.Info("showback: snapshot recovered into the open hour", "path", p, "rows", len(rows), "requests", hdr.Requests)
		case hdr.Hour < cur.hour:
			if err := s.sealRows(hdr.Hour, rows, true); err != nil {
				return err
			}
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			s.Log.Warn("showback: earlier hour's snapshot sealed as recovered (up to one flush interval of it is missing)", "path", p, "hour", hdr.Hour, "rows", len(rows))
		default:
			s.Log.Warn("showback: snapshot from a future hour left in place (clock stepped back?)", "path", p, "hour", hdr.Hour)
		}
	}
	_ = os.MkdirAll(filepath.Join(s.Dir, "current"), 0o750)
	return nil
}

// Run is the writer loop: a snapshot every Interval, and a wake-up just past each hour boundary
// so the old hour is sealed within a second of closing even with no traffic. ctx ending flushes.
func (s *Store) Run(ctx context.Context) {
	for {
		now := s.now()
		next := time.Unix((now.Unix()/3600+1)*3600, 0).Add(500 * time.Millisecond)
		boundary := time.NewTimer(next.Sub(now))
		tick := time.NewTimer(s.Interval)
		select {
		case <-ctx.Done():
			boundary.Stop()
			tick.Stop()
			s.Flush()
			return
		case <-tick.C:
			boundary.Stop()
		case <-boundary.C:
			tick.Stop()
		}
		s.Flush()
	}
}

// Flush seals every hour that has closed and snapshots the open one. Safe to call from a drain.
func (s *Store) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.now().Unix() / 3600
	cur := s.Agg.cur.Load()
	if h > cur.hour {
		// No request has opened the new hour: open it, so the closed hour still gets its file.
		nt := newTable(h)
		if s.Agg.cur.CompareAndSwap(cur, nt) {
			s.Agg.pushClosed(cur)
			cur = nt
		} else {
			cur = s.Agg.cur.Load()
		}
	}
	for _, old := range s.Agg.takeClosed() {
		s.sealTable(old)
	}
	if cur.overflowLogged.Load() && !cur.overflowLoggedByWriter() {
		s.Log.Warn("showback: tuple cap reached; new tuples fold into the __overflow__ row", "hour", cur.hour, "max_tuples_per_hour", s.Agg.maxTuples)
	}
	s.snapshot(cur)
	if s.Retention > 0 && s.now().Sub(s.lastSwep) > 24*time.Hour {
		s.lastSwep = s.now()
		s.sweep()
	}
}

// overflowLoggedByWriter flips the once-per-hour flag for the log line.
func (t *hourTable) overflowLoggedByWriter() bool { return t.overflowWritten.Swap(true) }

func (s *Store) snapshot(t *hourTable) {
	start := s.now()
	s.rows = t.snapshot(s.rows, s.NodeID)
	s.buf = encode(s.buf, s.NodeID, t.hour, s.rows, s.Agg.dims, true, false, start)
	s.observe(writeAtomic(CurrentPath(s.Dir, s.NodeID, t.hour), s.buf), start, "snapshot", t.hour)
}

func (s *Store) sealTable(t *hourTable) {
	t.seal()
	start := s.now()
	s.rows = t.snapshot(s.rows, s.NodeID)
	if err := s.sealRows(t.hour, s.rows, false); err != nil {
		s.observe(err, start, "seal", t.hour)
		return
	}
	s.observe(nil, start, "seal", t.hour)
	if err := os.Remove(CurrentPath(s.Dir, s.NodeID, t.hour)); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.Log.Warn("showback: sealed hour's snapshot not removed", "err", err.Error())
	}
}

// sealRows writes the final file for hour. A sealed file is never overwritten (D-049): if one
// exists, the rows are written beside it with a .dup suffix and the event is logged.
func (s *Store) sealRows(hour int64, rows []Row, recovered bool) error {
	path := SealedPath(s.Dir, s.NodeID, hour)
	if _, err := os.Stat(path); err == nil {
		s.Log.Error("showback: sealed file already exists; writing the duplicate beside it", "path", path)
		path += ".dup"
	}
	s.buf = encode(s.buf, s.NodeID, hour, rows, s.Agg.dims, false, recovered, s.now())
	return writeAtomic(path, s.buf)
}

func (s *Store) observe(err error, start time.Time, what string, hour int64) {
	if s.m.FlushDuration != nil {
		s.m.FlushDuration.Observe(s.now().Sub(start).Seconds())
	}
	if err == nil {
		return
	}
	if s.m.FlushErrors != nil {
		s.m.FlushErrors.Inc()
	}
	if s.now().Sub(s.lastErr) >= time.Minute {
		s.lastErr = s.now()
		s.Log.Error("showback: "+what+" failed; counting continues in memory", "hour", hour, "dir", s.Dir, "err", err.Error())
	}
}

// sweep deletes sealed files older than Retention days.
func (s *Store) sweep() {
	cutoff := s.now().Unix()/3600 - int64(s.Retention)*24
	root := filepath.Join(s.Dir, "usage")
	var old []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".ndjson") {
			return nil
		}
		if hour, _, ok := parseSealedName(root, path); ok && hour < cutoff {
			old = append(old, path)
		}
		return nil
	})
	for _, path := range old {
		if err := os.Remove(path); err != nil {
			s.Log.Warn("showback: retention delete failed", "path", path, "err", err.Error())
		}
	}
}

// parseSealedName recovers (hour, node) from usage/YYYY/MM/DD/HH-<node>.ndjson.
func parseSealedName(root, path string) (hour int64, node string, ok bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return 0, "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 4 {
		return 0, "", false
	}
	base := strings.TrimSuffix(parts[3], ".ndjson")
	hh, node, found := strings.Cut(base, "-")
	if !found {
		return 0, "", false
	}
	t, err := time.Parse("2006/01/02/15", parts[0]+"/"+parts[1]+"/"+parts[2]+"/"+hh)
	if err != nil {
		return 0, "", false
	}
	return t.Unix() / 3600, node, true
}

// parseCurrentName recovers (hour, node) from current/<hour>-<node>.ndjson.
func parseCurrentName(path string) (hour int64, node string, ok bool) {
	base := strings.TrimSuffix(filepath.Base(path), ".ndjson")
	hs, node, found := strings.Cut(base, "-")
	if !found {
		return 0, "", false
	}
	h, err := strconv.ParseInt(hs, 10, 64)
	if err != nil {
		return 0, "", false
	}
	return h, node, true
}

// HourString renders an hour bucket as RFC 3339 in UTC.
func HourString(hour int64) string { return time.Unix(hour*3600, 0).UTC().Format(time.RFC3339) }

func (s *Store) String() string { return fmt.Sprintf("showback store %s (node %s)", s.Dir, s.NodeID) }
