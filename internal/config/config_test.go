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
	"unknown-key":                 "listen.addres: unknown key",
	"missing-node-id":             "node_id: required",
	"missing-showback-dir":        "showback.dir: required",
	"backend-no-scheme":           "backend.scheme: required",
	"backend-endpoint-has-scheme": "backend.endpoints[0]: no scheme",
	"backend-endpoint-no-port":    "backend.endpoints[0]: ",
	"backend-http-with-tls":       "backend.tls: not allowed with scheme http",
	"tls-cert-only":               "listen.tls: cert and key must be set together",
	"tls-missing-files":           "listen.tls: ",
	"rule-dup-id":                 "rate_limits.rules[1].id: duplicate of rules[0]",
	"rule-bad-op":                 "rate_limits.rules[0].match.ops[0]: unknown operation or class \"GetObjekt\"",
	"rule-prefix-groupby":         "rate_limits.rules[0].group_by[0]: prefix needs match.prefixes",
	"rule-bad-cidr":               "rate_limits.rules[0].match.source_cidrs[0]: ",
	"rule-negative-rate":          "rate_limits.rules[0].limit.rate: must be >= 0",
	"rule-bad-id":                 "rate_limits.rules[0].id: required",
	"rule-bad-on-limit":           "rate_limits.rules[0].on_limit: must be slowdown or too_many_requests",
	"rule-bandwidth-reserved":     "rate_limits.rules[0].limit.bandwidth: unknown key",
	"dims-missing-operation":      "showback.dimensions: operation and status_class",
	"dims-unknown":                "showback.dimensions[0]: unknown dimension \"key\"",
	"admin-token-both":            "admin: token and token_file are mutually exclusive",
	"admin-token-file-missing":    "admin.token_file: ",
	"retention-negative":          "showback.retention_days: must be >= 0",
	"node-count-negative":         "cluster.node_count: must be positive",
	"type-error":                  "listen.max_inflight: ",
	"trusted-proxy-bad":           "listen.trusted_proxies[0]: ",
	"showback-prefix-bad-bucket":  "showback.prefixes.Bad_Bucket: not a valid bucket name",
	"copy-buffer-tiny":            "proxy.copy_buffer_bytes: must be at least 4096",
	"log-level":                   "logging.level: must be debug, info, warn, or error",
	"not-a-mapping":               "line 1: cannot construct",
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
		"rate_limits.default_on_limit":    {c.RateLimits.DefaultOnLimit, DefaultOnLimit},
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
		if len(o.RateLimits.Rules) == 0 {
			o.RateLimits.Rules = nil
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
	r := c.RateLimits.Rules
	if len(r) != 4 || r[0].ID != "tenant-a-writes" || r[0].Limit.Burst != 4000 || r[0].OnLimit != ModeSlowDown {
		t.Fatalf("rules: %+v", r)
	}
	if r[1].OnLimit != ModeTooManyRequests {
		t.Errorf("rule on_limit: %s", r[1].OnLimit)
	}
	if r[2].Limit.Rate != 0 || r[2].Limit.Burst != 0 {
		t.Errorf("rate 0 keeps burst 0: %+v", r[2].Limit)
	}
	if r[3].Limit.Rate != 0.5 || r[3].Limit.Burst != 1 {
		t.Errorf("burst default is max(1, ceil(rate)): %+v", r[3].Limit)
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
	for rate, want := range map[float64]int{2000: 2000, 0.5: 1, 2.5: 3, 1: 1, 0.001: 1} {
		c := Config{RateLimits: RateLimits{Rules: []Rule{{ID: "x", Limit: Limit{Rate: rate}}}}}
		c.applyDefaults()
		if got := c.RateLimits.Rules[0].Limit.Burst; got != want {
			t.Errorf("rate %v: burst %d, want %d", rate, got, want)
		}
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
	err = b.ReloadCompatible(a)
	if err == nil || !strings.Contains(err.Error(), "listen.address: cannot change on reload") || !strings.Contains(err.Error(), "node_id: cannot change") || !strings.Contains(err.Error(), "backend: cannot change") || strings.Contains(err.Error(), "drain_timeout") {
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
	f.Add([]byte("rate_limits: {rules: [1, 2]}"))
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
