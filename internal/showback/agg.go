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
	"hash/maphash"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/blakegolliher/tollbooth/internal/config"
	"github.com/blakegolliher/tollbooth/internal/s3id"
)

// Status is the showback status partition (docs/SHOWBACK_SCHEMA.md). It mirrors
// internal/admin's StatusClass value for value (D-048; admin imports this package for the
// metrics, so this package cannot import admin); internal/proxy asserts the two agree.
type Status uint8

// Status classes, in admin.StatusClass order.
const (
	Status2xx Status = iota
	Status3xx
	Status4xx
	Status5xx
	StatusThrottled
	StatusProxyError
	numStatus
)

var statusNames = [...]string{"2xx", "3xx", "4xx", "5xx", "throttled", "proxy_error"}

func (s Status) String() string {
	if int(s) < len(statusNames) {
		return statusNames[s]
	}
	return "proxy_error"
}

// ParseStatus maps a status_class string back to its Status (file reading, queries).
func ParseStatus(s string) (Status, bool) {
	for i, n := range statusNames {
		if n == s {
			return Status(i), true
		}
	}
	return 0, false
}

// Overflow is the literal every configured dimension carries when the tuple cap folds a request
// (docs/SHOWBACK_SCHEMA.md, The overflow row).
const Overflow = "__overflow__"

// numShards is the hour table's shard count (docs/DESIGN.md §6.1).
const numShards = 256

// Metrics are the catalog rows this package observes (docs/telemetry-catalog.md, Phase 3).
// internal/admin registers them. Any field may be nil in tests.
type Metrics struct {
	Tuples        prometheus.Gauge    // tollbooth_showback_tuples
	FlushDuration prometheus.Observer // tollbooth_showback_flush_duration_seconds
	FlushErrors   prometheus.Counter  // tollbooth_showback_flush_errors_total
	Collapsed     prometheus.Counter  // tollbooth_showback_collapsed_total
	ClockSkew     prometheus.Counter  // tollbooth_showback_clock_skew_total
}

// tuple is one row's identity: struct values of the identity's existing substrings for the
// configured dimensions ("" when a dimension is not configured), so a lookup allocates nothing.
type tuple struct {
	ak, bucket, prefix string
	ip                 netip.Addr
	op                 s3id.Op
	status             Status
}

// clone detaches the request-owned strings before the tuple is stored. prefix is the configured
// string already.
func (t tuple) clone() tuple {
	t.ak = strings.Clone(t.ak)
	t.bucket = strings.Clone(t.bucket)
	return t
}

// row is the three additive counters, updated under the shard lock.
type row struct{ requests, bytesIn, bytesOut int64 }

type shard struct {
	mu     sync.Mutex
	sealed bool
	m      map[tuple]*row
	_      [32]byte
}

// hourTable is one hour's rows. Once the sealer has set every shard's sealed flag, no Record can
// land in it.
type hourTable struct {
	hour   int64
	tuples atomic.Int64
	// overflowLogged is set by the first fold; overflowWritten by the writer when it has logged
	// it, so the line appears once per hour.
	overflowLogged  atomic.Bool
	overflowWritten atomic.Bool
	shards          [numShards]shard
}

// dimension bits
const (
	dimAccessKey uint8 = 1 << iota
	dimBucket
	dimPrefix
	dimSourceIP
)

// Aggregator counts completed requests into the open hour (docs/DESIGN.md §6.1). One per
// process; the Store (store.go) snapshots and seals its tables.
type Aggregator struct {
	cur atomic.Pointer[hourTable]
	// closed holds tables that were open and have been replaced; the writer seals each exactly
	// once, whether or not it ever snapshotted it.
	closedMu  sync.Mutex
	closed    []*hourTable
	seed      maphash.Seed
	dims      uint8
	maxTuples int64
	prefixes  map[string][]string // bucket → configured showback prefixes, longest first
	m         *Metrics
	// now is the clock; tests inject one. The hour is read inside Record (D-025).
	now func() time.Time
}

// NewAggregator builds an aggregator for cfg's showback dimensions, prefixes, and tuple cap. m
// may be nil.
func NewAggregator(cfg *config.Config, m *Metrics) *Aggregator {
	a := &Aggregator{seed: maphash.MakeSeed(), maxTuples: int64(cfg.Showback.MaxTuplesPerHour), m: m, now: time.Now}
	if m == nil {
		a.m = &Metrics{}
	}
	for _, d := range cfg.Showback.Dimensions {
		switch d {
		case config.DimAccessKey:
			a.dims |= dimAccessKey
		case config.DimBucket:
			a.dims |= dimBucket
		case config.DimPrefix:
			a.dims |= dimPrefix
		case config.DimSourceIP:
			a.dims |= dimSourceIP
		}
	}
	if len(cfg.Showback.Prefixes) > 0 {
		a.prefixes = make(map[string][]string, len(cfg.Showback.Prefixes))
		for b, ps := range cfg.Showback.Prefixes {
			list := make([]string, 0, len(ps))
			for _, p := range ps {
				if p != "" {
					list = append(list, strings.Clone(p))
				}
			}
			sort.SliceStable(list, func(i, j int) bool { return len(list[i]) > len(list[j]) })
			a.prefixes[strings.Clone(b)] = list
		}
	}
	a.cur.Store(newTable(a.now().Unix() / 3600))
	return a
}

func newTable(hour int64) *hourTable {
	t := &hourTable{hour: hour}
	for i := range t.shards {
		t.shards[i].m = make(map[tuple]*row)
	}
	return t
}

// Dimensions reports which of access_key, bucket, prefix, source_ip are recorded.
func (a *Aggregator) Dimensions() (accessKey, bucket, prefix, sourceIP bool) {
	return a.dims&dimAccessKey != 0, a.dims&dimBucket != 0, a.dims&dimPrefix != 0, a.dims&dimSourceIP != 0
}

// Prefix returns the longest configured showback prefix of the bucket that the identity's key
// (or a listing's prefix parameter, ADR-0001) starts with, or "" for the bucket root.
func (a *Aggregator) Prefix(id *s3id.RequestIdentity) string {
	if a.prefixes == nil {
		return ""
	}
	ps := a.prefixes[id.Bucket]
	if ps == nil {
		return ""
	}
	var in string
	switch {
	case id.Level == s3id.LevelObject:
		in = id.Key
	case id.Op.IsBucketList():
		in = id.ListPrefix
	default:
		return ""
	}
	for _, p := range ps {
		if strings.HasPrefix(in, p) {
			return p
		}
	}
	return ""
}

func (a *Aggregator) tupleFor(id *s3id.RequestIdentity, st Status) tuple {
	t := tuple{op: id.Op, status: st}
	if int(id.Op) >= s3id.NumOps {
		t.op = s3id.OpUnknown
	}
	if st >= numStatus {
		t.status = StatusProxyError
	}
	if a.dims&dimAccessKey != 0 {
		t.ak = id.AccessKey
	}
	if a.dims&dimBucket != 0 {
		t.bucket = id.Bucket
	}
	if a.dims&dimPrefix != 0 {
		t.prefix = a.Prefix(id)
	}
	if a.dims&dimSourceIP != 0 {
		t.ip = id.SourceIP
	}
	return t
}

// overflowIP stands for the source_ip dimension in the overflow row (the text form is written as
// "__overflow__"); it is an address no client has.
var overflowIP = netip.MustParseAddr("ff02::7466:6c6f:7700") // "tbflow"

func (a *Aggregator) overflow(t tuple) tuple {
	o := tuple{op: t.op, status: t.status}
	if a.dims&dimAccessKey != 0 {
		o.ak = Overflow
	}
	if a.dims&dimBucket != 0 {
		o.bucket = Overflow
	}
	if a.dims&dimPrefix != 0 {
		o.prefix = Overflow
	}
	if a.dims&dimSourceIP != 0 {
		o.ip = overflowIP
	}
	return o
}

// Record counts one completed request (docs/DESIGN.md §6.1). The hour is read from the clock
// here (D-025). Zero allocations for an existing tuple; a new tuple allocates its row and clones
// its strings. The request goroutine never waits on storage.
func (a *Aggregator) Record(id *s3id.RequestIdentity, st Status, bytesIn, bytesOut int64) {
	h := a.now().Unix() / 3600
	t := a.tupleFor(id, st)
	folded := false
	for {
		cur := a.cur.Load()
		switch {
		case h > cur.hour:
			nt := newTable(h)
			if !a.cur.CompareAndSwap(cur, nt) {
				continue // another request opened the hour first
			}
			a.pushClosed(cur)
			cur = nt
		case h < cur.hour:
			if a.m.ClockSkew != nil {
				a.m.ClockSkew.Inc()
			}
		}
		sh := &cur.shards[maphash.Comparable(a.seed, t)&(numShards-1)]
		sh.mu.Lock()
		if sh.sealed {
			sh.mu.Unlock()
			continue // the sealer got here first; the reload finds the new table
		}
		r := sh.m[t]
		if r == nil {
			if !folded && cur.tuples.Load() >= a.maxTuples {
				sh.mu.Unlock()
				t = a.overflow(t)
				folded = true
				if a.m.Collapsed != nil {
					a.m.Collapsed.Inc()
				}
				cur.overflowLogged.Store(true)
				continue // the overflow tuple lives in its own shard
			}
			r = &row{}
			sh.m[t.clone()] = r
			n := cur.tuples.Add(1)
			if a.m.Tuples != nil {
				a.m.Tuples.Set(float64(n))
			}
		}
		r.requests++
		r.bytesIn += bytesIn
		r.bytesOut += bytesOut
		sh.mu.Unlock()
		return
	}
}

// pushClosed hands a replaced table to the writer.
func (a *Aggregator) pushClosed(t *hourTable) {
	a.closedMu.Lock()
	a.closed = append(a.closed, t)
	a.closedMu.Unlock()
}

// takeClosed returns the tables awaiting their seal, oldest first.
func (a *Aggregator) takeClosed() []*hourTable {
	a.closedMu.Lock()
	out := a.closed
	a.closed = nil
	a.closedMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].hour < out[j].hour })
	return out
}

// Tuples is the distinct tuple count of the open hour.
func (a *Aggregator) Tuples() int64 { return a.cur.Load().tuples.Load() }

// Hour is the open hour.
func (a *Aggregator) Hour() int64 { return a.cur.Load().hour }

// snapshot copies the table's rows, shard by shard under each lock, into dst (reused across
// calls) in a deterministic order.
func (t *hourTable) snapshot(dst []Row, nodeID string) []Row {
	dst = dst[:0]
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.Lock()
		for k, r := range sh.m {
			dst = append(dst, Row{Hour: t.hour, NodeID: nodeID, tuple: k, requests: r.requests, bytesIn: r.bytesIn, bytesOut: r.bytesOut})
		}
		sh.mu.Unlock()
	}
	sort.Slice(dst, func(i, j int) bool { return dst[i].less(&dst[j]) })
	return dst
}

// seal sets every shard's flag so no Record can land in the table afterwards.
func (t *hourTable) seal() {
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.Lock()
		sh.sealed = true
		sh.mu.Unlock()
	}
}

// load inserts rows read back from a snapshot (restart, docs/DESIGN.md §6.3).
func (a *Aggregator) load(t *hourTable, rows []Row) {
	for i := range rows {
		r := &rows[i]
		sh := &t.shards[maphash.Comparable(a.seed, r.tuple)&(numShards-1)]
		sh.mu.Lock()
		if cur := sh.m[r.tuple]; cur != nil {
			cur.requests += r.requests
			cur.bytesIn += r.bytesIn
			cur.bytesOut += r.bytesOut
		} else {
			sh.m[r.clone()] = &row{requests: r.requests, bytesIn: r.bytesIn, bytesOut: r.bytesOut}
			t.tuples.Add(1)
		}
		sh.mu.Unlock()
	}
	if a.m.Tuples != nil {
		a.m.Tuples.Set(float64(t.tuples.Load()))
	}
}
