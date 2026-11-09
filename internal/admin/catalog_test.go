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
// Adapted from github.com/blakegolliher/shunt internal/telemetry/catalog_test.go at commit
// 9b36d09ed109add4a2bedfe1dbfae6108038ee5b (Apache-2.0, Copyright 2026 Blake Golliher).
// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, 2026-11-09: the catalog's
// label column is parsed too, and every registered metric's label set must equal it exactly
// (the cardinality test CLAUDE.md requires).

package admin

import (
	"bufio"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/blakegolliher/tollbooth/internal/s3id"
)

// catalog parses docs/telemetry-catalog.md: metric name → sorted label names.
func catalog(t *testing.T) map[string][]string {
	t.Helper()
	f, err := os.Open("../../docs/telemetry-catalog.md")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only
	row := regexp.MustCompile("^\\| `(tollbooth_[a-z0-9_]+)` \\| [a-z]+ \\| ([^|]*)\\|")
	label := regexp.MustCompile("`([a-z_]+)`")
	values := regexp.MustCompile(`\{[^}]*\}`) // the ∈ {…} value enumerations are not label names
	out := map[string][]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		m := row.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		labels := make([]string, 0, 4)
		for _, l := range label.FindAllStringSubmatch(values.ReplaceAllString(m[2], ""), -1) {
			labels = append(labels, l[1])
		}
		sort.Strings(labels)
		out[m[1]] = labels
	}
	if len(out) == 0 {
		t.Fatal("no metric rows found in docs/telemetry-catalog.md")
	}
	return out
}

func touchAll(m *Metrics) {
	m.Request(s3id.OpGetObject, Status2xx, ResultForwarded).Inc()
	m.Observe(s3id.ClassRead, 0.01, 0.0001, 0.005)
	m.AddBytes(1, 1)
	m.Inflight.Set(1)
	m.Connections(SideClient).Set(1)
	m.BackendUp.WithLabelValues("127.0.0.1:9000").Set(1)
	m.BackendErrors.WithLabelValues("127.0.0.1:9000", "dial").Inc()
	m.BackendRetries.Inc()
	m.ConfigReload.WithLabelValues("ok").Inc()
}

// TestEveryMetricIsInTheCatalog: a registered tollbooth_* metric without a catalog row fails.
// Catalog rows without a metric are logged, not fatal, because rows are written before the code.
func TestEveryMetricIsInTheCatalog(t *testing.T) {
	cat := catalog(t)
	m := NewMetrics()
	touchAll(m)
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, f := range families {
		name := f.GetName()
		if !strings.HasPrefix(name, "tollbooth_") {
			continue
		}
		registered[name] = true
		if _, ok := cat[name]; !ok {
			t.Errorf("metric %s is registered but not in docs/telemetry-catalog.md", name)
		}
	}
	for name := range cat {
		if !registered[name] {
			t.Logf("catalog row %s has no registered metric yet", name)
		}
	}
	// Phase 1 registers exactly the eleven request-path, backend, and reload metrics.
	if len(registered) != 11 {
		t.Errorf("Phase 1 registers exactly eleven tollbooth_ metrics, got %d: %v", len(registered), registered)
	}
}

// TestLabelSetsMatchTheCatalog is the cardinality guard: every registered metric's label names
// equal the catalog's, so a label can be neither added nor dropped without editing the catalog.
func TestLabelSetsMatchTheCatalog(t *testing.T) {
	cat := catalog(t)
	m := NewMetrics()
	touchAll(m)
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		name := f.GetName()
		want, ok := cat[name]
		if !ok || len(f.GetMetric()) == 0 {
			continue
		}
		var got []string
		for _, lp := range f.GetMetric()[0].GetLabel() {
			got = append(got, lp.GetName())
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s labels: registered %v, catalog %v", name, got, want)
		}
	}
}

// The pre-resolved handles are fixed in number: every op × status × result child exists from
// construction, so the series count never grows with traffic.
func TestPreResolvedCardinality(t *testing.T) {
	m := NewMetrics()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		switch f.GetName() {
		case "tollbooth_requests_total":
			if n := len(f.GetMetric()); n != s3id.NumOps*int(numStatus)*int(numResult) {
				t.Errorf("requests_total has %d series, want %d", n, s3id.NumOps*int(numStatus)*int(numResult))
			}
		case "tollbooth_request_duration_seconds", "tollbooth_proxy_overhead_seconds", "tollbooth_backend_ttfb_seconds":
			if n := len(f.GetMetric()); n != s3id.NumClasses {
				t.Errorf("%s has %d series, want %d", f.GetName(), n, s3id.NumClasses)
			}
		case "tollbooth_bytes_total", "tollbooth_connections_open", "tollbooth_config_reload_total":
			if n := len(f.GetMetric()); n != 2 {
				t.Errorf("%s has %d series, want 2", f.GetName(), n)
			}
		}
	}
	// Out-of-range enums clamp instead of indexing past the arrays.
	m.Request(s3id.Op(250), StatusClass(99), Result(99)).Inc()
	m.Observe(s3id.Class(99), 1, 1, 1)
	m.Connections(Side(9)).Set(1)
}

func TestClassifyStatus(t *testing.T) {
	for status, want := range map[int]StatusClass{200: Status2xx, 206: Status2xx, 301: Status3xx, 404: Status4xx, 503: Status5xx, 0: StatusProxyError, 999: StatusProxyError} {
		if got := ClassifyStatus(status); got != want {
			t.Errorf("ClassifyStatus(%d) = %s, want %s", status, got, want)
		}
	}
	if StatusClass(42).String() != "proxy_error" || Result(42).String() != "error" {
		t.Error("out-of-range enums must stringify to the error values")
	}
}

// A request observation allocates nothing (docs/DESIGN.md §9, stage 9).
func TestObservationAllocatesNothing(t *testing.T) {
	m := NewMetrics()
	allocs := testing.AllocsPerRun(1000, func() {
		m.Request(s3id.OpPutObject, Status2xx, ResultForwarded).Inc()
		m.Observe(s3id.ClassWrite, 0.002, 0.00005, 0.001)
		m.AddBytes(4096, 0)
	})
	if allocs != 0 {
		t.Fatalf("metrics observation allocates %.1f per request; the budget is 0", allocs)
	}
}

func BenchmarkObserve(b *testing.B) {
	m := NewMetrics()
	b.ReportAllocs()
	for b.Loop() {
		m.Request(s3id.OpGetObject, Status2xx, ResultForwarded).Inc()
		m.Observe(s3id.ClassRead, 0.002, 0.00005, 0.001)
		m.AddBytes(0, 4096)
	}
}
