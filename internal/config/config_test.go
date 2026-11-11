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
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// invalidWant maps each testdata/invalid file to the substring its error must carry: the key path
// first, so check-config output points at the key.
var invalidWant = map[string]string{
	"unknown-key":                    "listen.addres: unknown key",
	"missing-node-id":                "node_id: required",
	"missing-showback-dir":           "showback.dir: required",
	"backend-no-scheme":              "backend.scheme: required",
	"backend-endpoint-has-scheme":    "backend.endpoints[0]: no scheme",
	"backend-endpoint-no-port":       "backend.endpoints[0]: ",
	"backend-http-with-tls":          "backend.tls: not allowed with scheme http",
	"tls-cert-only":                  "listen.tls: cert and key must be set together",
	"tls-missing-files":              "listen.tls: ",
	"rule-dup-name":                  "limits[1].name: duplicate of limits[0]",
	"rule-bad-name":                  "limits[0].name: required",
	"rule-op-typo":                   "limits[0].operation: unknown S3 operation \"GetObjekt\"; did you mean GetObject?",
	"rule-op-typo-v2":                "limits[0].operation: unknown S3 operation \"ListObjectV2\"; did you mean ListObjectsV2?",
	"rule-op-class-word":             "limits[0].operation: \"WRITE\" is not an S3 operation; name the operations themselves (it used to mean PostObject, PutObject, CopyObject",
	"rule-op-star":                   "limits[0].operation: omit `operation` to match every operation",
	"rule-op-inner-star":             "limits[0].operation: \"Get*Object\": only a trailing * is a wildcard",
	"rule-op-family-empty":           "limits[0].operation: \"Zz*\" matches no S3 operation",
	"rule-op-in-list":                "limits[0].operation[1]: unknown S3 operation \"Nope\"",
	"rule-op-not-scalar":             "limits[0].operation: every item must be a plain value",
	"rule-per-missing":               "limits[0].per: required: who gets their own budget?",
	"rule-per-unknown":               "limits[0].per: must be total, access_key, bucket, prefix, or source_ip, got \"key\"",
	"rule-per-total-combined":        "limits[0].per[0]: total means one shared budget",
	"rule-per-prefix-without-prefix": "limits[0].per: per: prefix counts each listed prefix separately",
	"rule-per-repeated":              "limits[0].per[1]: repeated",
	"rule-bad-source":                "limits[0].source_ip: not an address or a CIDR: \"10.0.0.0/33\"",
	"rule-empty-prefix":              "limits[0].prefix: empty prefix",
	"rule-no-limit":                  "limits[0].limit: none set: write exactly one of limit_per_second, limit_per_minute, limit_per_hour, or blocked: true",
	"rule-two-limits":                "limits[0].limit: more than one set",
	"rule-blocked-and-limit":         "limits[0].limit: set either a limit or blocked: true, not both",
	"rule-blocked-burst":             "limits[0].burst: a blocked rule has no burst",
	"rule-negative-limit":            "limits[0].limit_per_second: must be a number above 0, got -1 (to refuse every request write blocked: true)",
	"rule-zero-limit":                "limits[0].limit_per_second: must be a number above 0, got 0",
	"rule-limit-not-a-number":        "limits[0].limit_per_second: must be a number",
	"rule-bad-respond":               "limits[0].respond: must be 503 (SlowDown) or 429 (TooManyRequests), got 500",
	"rule-unknown-key":               "limits[0].limit_bytes_per_second: unknown key",
	"rule-rate-tiny":                 "limits[0].limit_per_hour: 0.001 per hour is outside what the limiter can enforce",
	"rule-rate-huge":                 "limits[0].limit_per_second: 2,000,000,000 per second is outside what the limiter can enforce",
	"rule-burst-refill":              "limits[0].burst: 2000000000 at 0.001 per second takes 2000000000000 s to refill",
	"rule-note-long":                 "limits[0].note: at most 200 characters, got 201",
	"old-schema-top-level":           "rate_limits: unknown key; rules are written under `limits:`",
	"old-schema-rule-keys":           "limits[0].group_by: unknown key; write `per:`",
	"dims-missing-operation":         "showback.dimensions: operation and status_class",
	"dims-unknown":                   "showback.dimensions[0]: unknown dimension \"key\"",
	"admin-token-both":               "admin: token and token_file are mutually exclusive",
	"admin-token-file-missing":       "admin.token_file: ",
	"retention-negative":             "showback.retention_days: must be >= 0",
	"node-count-negative":            "cluster.node_count: must be positive",
	"node-count-huge":                "cluster.node_count: must be at most 1024",
	"type-error":                     "listen.max_inflight: ",
	"trusted-proxy-bad":              "listen.trusted_proxies[0]: ",
	"showback-prefix-bad-bucket":     "showback.prefixes.Bad_Bucket: not a valid bucket name",
	"copy-buffer-tiny":               "proxy.copy_buffer_bytes: must be at least 4096",
	"log-level":                      "logging.level: must be debug, info, warn, or error",
	"not-a-mapping":                  "line 1: cannot construct",
}

func TestInvalidFiles(t *testing.T) {
	files, err := filepath.Glob("testdata/invalid/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no invalid fixtures: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".yaml")
		seen[name] = true
		t.Run(name, func(t *testing.T) {
			want, ok := invalidWant[name]
			if !ok {
				t.Fatalf("fixture %s has no expected error in invalidWant", name)
			}
			_, err := Load(f)
			if err == nil {
				t.Fatalf("%s was accepted", f)
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error does not name the key:\n  got:  %v\n  want: …%s…", err, want)
			}
		})
	}
	for name := range invalidWant {
		if !seen[name] {
			t.Errorf("invalidWant names %s but no fixture exists", name)
		}
	}
}

func TestValidFiles(t *testing.T) {
	files, err := filepath.Glob("testdata/valid/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no valid fixtures: %v", err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			c, err := Load(f)
			if err != nil {
				t.Fatalf("%s rejected: %v", f, err)
			}
			if err := c.Validate(); err != nil {
				t.Fatalf("re-validation failed: %v", err)
			}
		})
	}
}

func TestMinimalGetsEveryDefault(t *testing.T) {
	c, err := Load("testdata/valid/minimal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string][2]any{
		"listen.address":                  {c.Listen.Address, DefaultListenAddress},
		"listen.tls.min_version":          {c.Listen.TLS.MinVersion, DefaultMinTLSVersion},
		"listen.read_header_timeout":      {c.Listen.ReadHeaderTimeout, DefaultReadHeaderTimeout},
		"listen.idle_timeout":             {c.Listen.IdleTimeout, DefaultIdleTimeout},
		"listen.max_header_bytes":         {c.Listen.MaxHeaderBytes, DefaultMaxHeaderBytes},
		"listen.max_inflight":             {c.Listen.MaxInflight, DefaultMaxInflight},
		"backend.dial_timeout":            {c.Backend.DialTimeout, DefaultDialTimeout},
		"backend.response_header_timeout": {c.Backend.ResponseHeaderTimeout, DefaultResponseHeaderTimeout},
		"backend.idle_body_timeout":       {c.Backend.IdleBodyTimeout, DefaultIdleBodyTimeout},
		"backend.max_idle_conns_per_host": {c.Backend.MaxIdleConnsPerHost, DefaultMaxIdleConnsPerHost},
		"backend.health_check.interval":   {c.Backend.HealthCheck.Interval, DefaultHealthInterval},
		"backend.health_check.timeout":    {c.Backend.HealthCheck.Timeout, DefaultHealthTimeout},
		"proxy.copy_buffer_bytes":         {c.Proxy.CopyBufferBytes, DefaultCopyBufferBytes},
		"proxy.drain_timeout":             {c.Proxy.DrainTimeout, DefaultDrainTimeout},
		"cluster.node_count":              {c.Cluster.NodeCount, DefaultNodeCount},
		"limiter.max_keys":                {c.Limiter.MaxKeys, DefaultMaxKeys},
		"limiter.sweep_interval":          {c.Limiter.SweepInterval, DefaultSweepInterval},
		"showback.flush_interval":         {c.Showback.FlushInterval, DefaultFlushInterval},
		"showback.retention_days":         {c.Showback.Retention(), DefaultRetentionDays},
		"showback.dimensions":             {c.Showback.Dimensions, DefaultDimensions},
		"showback.max_tuples_per_hour":    {c.Showback.MaxTuplesPerHour, DefaultMaxTuplesPerHour},
		"admin.address":                   {c.Admin.Address, DefaultAdminAddress},
		"logging.level":                   {c.Logging.Level, DefaultLogLevel},
		"logging.throttle_sample":         {c.Logging.ThrottleSample, DefaultThrottleSample},
	}
	for key, got := range checks {
		if !reflect.DeepEqual(got[0], got[1]) {
			t.Errorf("%s default: got %v want %v", key, got[0], got[1])
		}
	}
	// The budget decided at Gate 0 (D-024): 4,096 × 128 KiB = 512 MiB.
	if int64(c.Listen.MaxInflight)*int64(c.Proxy.CopyBufferBytes) != 512<<20 {
		t.Errorf("copy-buffer worst case is %d, want 512 MiB", int64(c.Listen.MaxInflight)*int64(c.Proxy.CopyBufferBytes))
	}
}

// deploy/tollbooth.example.yaml sets every key to its default, so it must parse to exactly what the
// minimal file parses to once the three required values match.
func TestExampleConfigIsTheDefaults(t *testing.T) {
	ex, err := Load("../../deploy/tollbooth.example.yaml")
	if err != nil {
		t.Fatalf("example config rejected: %v", err)
	}
	min, err := Load("testdata/valid/minimal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Normalize the shapes YAML cannot distinguish: an empty list vs nil, an empty map vs nil.
	norm := func(c *Config) Config {
		o := *c
		if len(o.BaseDomains) == 0 {
			o.BaseDomains = nil
		}
		if len(o.Listen.TrustedProxies) == 0 {
			o.Listen.TrustedProxies = nil
		}
		if len(o.Limits) == 0 {
			o.Limits = nil
		}
		if len(o.Showback.Prefixes) == 0 {
			o.Showback.Prefixes = nil
		}
		return o
	}
	if a, b := norm(ex), norm(min); !reflect.DeepEqual(a, b) {
		t.Fatalf("example config differs from the defaults:\n example: %+v\n minimal: %+v", a, b)
	}
}

func TestRulesParse(t *testing.T) {
	c, err := Load("testdata/valid/rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r := c.Limits
	if len(r) != 6 || r[0].Name != "tenant-a-uploads" || r[0].BurstSize() != 4000 || r[0].Rate() != 2000 || r[0].Mode() != ModeSlowDown ||
		!reflect.DeepEqual(r[0].GroupBy(), []string{"access_key", "bucket"}) || len(r[0].Operations()) != 3 {
		t.Fatalf("rules: %+v", r)
	}
	if r[1].Mode() != ModeTooManyRequests || r[1].RespondStatus() != 429 || r[1].Operation[0] != "ListObjectsV2" {
		t.Errorf("respond: 429 and the operation in S3's spelling: %+v", r[1])
	}
	if !r[2].Blocked || r[2].Rate() != 0 || r[2].BurstSize() != 0 || len(r[2].GroupBy()) != 0 || r[2].Note == "" {
		t.Errorf("a blocked rule has no rate, burst, or budget: %+v", r[2])
	}
	if r[3].Rate() != 0.5 || r[3].BurstSize() != 1 {
		t.Errorf("burst default is max(1, ceil(count)): rate %v burst %d", r[3].Rate(), r[3].BurstSize())
	}
	if got := r[4].Operations(); len(got) != 11 || r[4].Rate() != 10 || r[4].BurstSize() != 600 || len(r[4].GroupBy()) != 0 {
		t.Errorf("List* is 11 operations and 600 a minute is 10 a second with 600 at once: %d ops, rate %v, burst %d", len(got), r[4].Rate(), r[4].BurstSize())
	}
	if r[5].AccessKey[0] != "1234567890" || r[5].Bucket[0] != "no" || math.Abs(r[5].Rate()-20.0/3600) > 1e-12 || r[5].BurstSize() != 20 {
		t.Errorf("digits and `no` stay strings; 20 an hour allows 20 at once: %+v rate %v burst %d", r[5], r[5].Rate(), r[5].BurstSize())
	}
	if c.Showback.Retention() != 0 {
		t.Errorf("retention_days: 0 must stay 0, got %d", c.Showback.Retention())
	}
	id := c.Identity()
	if len(id.TrustedProxies) != 3 || id.TrustedProxies[1] != netip.MustParsePrefix("192.0.2.1/32") {
		t.Errorf("trusted proxies: %v", id.TrustedProxies)
	}
	if got := id.Domains.Bare(); len(got) != 1 || got[0] != "s3.lab.example.com" {
		t.Errorf("domains: %v", got)
	}
}

func TestBurstDefault(t *testing.T) {
	for count, want := range map[float64]int{2000: 2000, 0.5: 1, 2.5: 3, 1: 1, 0.001: 1} {
		n := Number(count)
		for _, r := range []Rule{{LimitPerSecond: &n}, {LimitPerMinute: &n}, {LimitPerHour: &n}} {
			if got := r.BurstSize(); got != want {
				t.Errorf("%s: burst %d, want %d", r.LimitText(), got, want)
			}
		}
	}
	n := Number(20)
	if r := (Rule{LimitPerHour: &n, Burst: 5}); r.BurstSize() != 5 {
		t.Errorf("an explicit burst wins: %d", r.BurstSize())
	}
}

func TestAdminToken(t *testing.T) {
	dir := t.TempDir()
	tf := filepath.Join(dir, "token")
	if err := os.WriteFile(tf, []byte("  s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := "node_id: a\nbackend: {scheme: http, endpoints: [h:1]}\nshowback: {dir: /d}\n"
	c, err := Parse([]byte(base + "admin: {token_file: " + tf + "}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminToken() != "s3cr3t" {
		t.Fatalf("token from file: %q", c.AdminToken())
	}
	if r := c.Redacted(); r.Admin.TokenFile != tf || r.Admin.Token != "" || r.AdminToken() != "" {
		t.Fatalf("redacted file form: %+v", r.Admin)
	}
	c, err = Parse([]byte(base + "admin: {token: inline}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminToken() != "inline" || c.Redacted().Admin.Token != "<redacted>" || c.Redacted().AdminToken() != "" {
		t.Fatalf("inline token: %q redacted %+v", c.AdminToken(), c.Redacted().Admin)
	}
	if err := os.WriteFile(tf, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse([]byte(base + "admin: {token_file: " + tf + "}\n")); err == nil || !strings.Contains(err.Error(), "admin.token_file: file is empty") {
		t.Fatalf("empty token file accepted: %v", err)
	}
}

func TestWarnings(t *testing.T) {
	c, err := Load("testdata/valid/rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(c.Warnings(1<<30), "\n")
	for _, want := range []string{
		"insecure_skip_verify is true",
		"retention_days is 0",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("missing warning %q in:\n%s", want, w)
		}
	}
	if strings.Contains(w, "s3.lab.example.com:443 is not among") {
		t.Errorf("endpoint in base_domains must not warn:\n%s", w)
	}
	if strings.Contains(w, "10.0.0.2:443 is not among") {
		t.Errorf("an IP endpoint must not warn:\n%s", w)
	}
	// 4,096 × 128 KiB = 512 MiB is exactly half of 1 GiB: no warning; above it, warn.
	if strings.Contains(w, "GOMEMLIMIT") {
		t.Errorf("default budget must fit half of 1 GiB:\n%s", w)
	}
	if w := strings.Join(c.Warnings(1<<29), "\n"); !strings.Contains(w, "exceeds half of GOMEMLIMIT") {
		t.Errorf("budget warning missing at 512 MiB limit:\n%s", w)
	}
	m, err := Load("testdata/valid/minimal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(m.Warnings(0), "\n"); !strings.Contains(w, "base_domains is empty") {
		t.Errorf("empty base_domains must warn:\n%s", w)
	}
	// A named endpoint outside base_domains warns about the vhost constraint.
	c2, err := Parse([]byte("node_id: a\nbase_domains: [s3.example.com]\nbackend: {scheme: http, endpoints: [vast-vip:80]}\nshowback: {dir: /d}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(c2.Warnings(0), "\n"); !strings.Contains(w, "vast-vip:80 is not among base_domains") {
		t.Errorf("vhost constraint warning missing:\n%s", w)
	}
}

func TestReloadCompatible(t *testing.T) {
	a, err := Load("testdata/valid/minimal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	b := *a
	if err := b.ReloadCompatible(a); err != nil {
		t.Fatalf("identical config must reload: %v", err)
	}
	b.Listen.Address = ":9999"
	b.NodeID = "other"
	b.Proxy.DrainTimeout = time.Minute // allowed to change
	b.Backend.Endpoints = []string{"other:1"}
	b.Showback.Dimensions = []string{"operation", "status_class"}
	b.Showback.Prefixes = map[string][]string{"bkt": {"a/"}}
	b.Showback.MaxTuplesPerHour = 10 // allowed to change
	err = b.ReloadCompatible(a)
	if err == nil || !strings.Contains(err.Error(), "listen.address: cannot change on reload") || !strings.Contains(err.Error(), "node_id: cannot change") || !strings.Contains(err.Error(), "backend: cannot change") || strings.Contains(err.Error(), "drain_timeout") ||
		!strings.Contains(err.Error(), "showback.dimensions: cannot change") || !strings.Contains(err.Error(), "showback.prefixes: cannot change") || strings.Contains(err.Error(), "max_tuples") {
		t.Fatalf("reload compatibility: %v", err)
	}
}

func TestValidBucketName(t *testing.T) {
	valid := []string{"abc", "my-bucket", "my.bucket", "a1b2c3", strings.Repeat("a", 63), "1bucket", "a-.b"}
	invalid := []string{"", "ab", strings.Repeat("a", 64), "My-Bucket", "-bucket", "bucket-", "bucket.", ".bucket", "my..bucket",
		"my_bucket", "192.168.5.4", "xn--bucket", "sthree-b", "amzn-s3-demo-x", "b-s3alias", "b--ol-s3", "b.mrap", "b--x-s3", "b--table-s3", "bu cket", "bücket"}
	for _, n := range valid {
		if !ValidBucketName(n) {
			t.Errorf("ValidBucketName(%q) = false, want true", n)
		}
	}
	for _, n := range invalid {
		if ValidBucketName(n) {
			t.Errorf("ValidBucketName(%q) = true, want false", n)
		}
	}
}

func TestEmptyAndMalformed(t *testing.T) {
	for _, in := range []string{"", "   \n", "# only a comment\n"} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
	if _, err := Parse([]byte("node_id: [unterminated")); err == nil {
		t.Error("accepted malformed YAML")
	}
}

// FuzzParse: Parse never panics, and any config it accepts re-validates and re-derives.
func FuzzParse(f *testing.F) {
	for _, glob := range []string{"testdata/valid/*.yaml", "testdata/invalid/*.yaml"} {
		files, _ := filepath.Glob(glob)
		for _, p := range files {
			b, err := os.ReadFile(p)
			if err != nil {
				f.Fatal(err)
			}
			f.Add(b)
		}
	}
	f.Add([]byte(""))
	f.Add([]byte("limits: [1, 2]"))
	f.Add([]byte("limits: [{name: a, operation: [Get*, x], limit_per_minute: 1e3, per: [total]}]"))
	f.Add([]byte("showback: {prefixes: {b: x}}"))
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := Parse(data)
		if err != nil {
			return
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("accepted config fails re-validation: %v", err)
		}
		if err := c.derive(); err != nil {
			t.Fatalf("accepted config fails re-derivation: %v", err)
		}
	})
}

func BenchmarkParse(b *testing.B) {
	data, err := os.ReadFile("testdata/valid/rules.yaml")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Parse(data); err != nil {
			b.Fatal(err)
		}
	}
}

// deploy/tollbooth.quickstart.yaml is the first thing a newcomer runs, and README.md prints it. The
// file must load, say what the README says it says, and be the config the README shows, so the
// two cannot drift apart.
func TestQuickstartConfigIsWhatTheReadmeShows(t *testing.T) {
	qs, err := Load("../../deploy/tollbooth.quickstart.yaml")
	if err != nil {
		t.Fatalf("quick-start config rejected: %v", err)
	}
	const sentence = "At most 50 GetObject calls per second for each access key. Only for requests by access key AKIAEXAMPLEKEY000001. Over the limit, the client gets 503 SlowDown."
	if len(qs.Limits) != 1 || qs.Limits[0].Sentence() != sentence {
		t.Fatalf("quick-start limits: %+v", qs.Limits)
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "  one-key-getobject: "+sentence) {
		t.Error("README.md does not print the sentence check-config gives for the quick-start config")
	}
	// The first yaml block of the README is the same configuration.
	_, rest, ok := strings.Cut(string(readme), "```yaml\n")
	block, _, ok2 := strings.Cut(rest, "```")
	if !ok || !ok2 {
		t.Fatal("README.md has no yaml block")
	}
	shown, err := Parse([]byte(block))
	if err != nil {
		t.Fatalf("the config README.md shows does not load: %v", err)
	}
	a, _ := json.Marshal(qs.Redacted())
	b, _ := json.Marshal(shown.Redacted())
	if string(a) != string(b) {
		t.Errorf("README.md shows a different config than deploy/tollbooth.quickstart.yaml:\nfile   %s\nreadme %s", a, b)
	}
}
