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
	"time"

	"github.com/blakegolliher/tollbooth/internal/s3id"
)

// Config is the root of the tollbooth configuration file (docs/CONFIG.md). Every key is documented
// there with its type, default, and one example; unknown keys are errors. A Config is immutable
// once Load returns it and is published behind one atomic.Pointer; nothing ever writes it back.
type Config struct {
	NodeID      string   `yaml:"node_id"`
	BaseDomains []string `yaml:"base_domains"`

	Listen     Listen       `yaml:"listen"`
	Backend    Backend      `yaml:"backend"`
	Proxy      Proxy        `yaml:"proxy"`
	Cluster    Cluster      `yaml:"cluster"`
	RateLimits RateLimits   `yaml:"rate_limits"`
	Limiter    Limiter      `yaml:"limiter"`
	Showback   Showback     `yaml:"showback"`
	Admin      Admin        `yaml:"admin"`
	Logging    Logging      `yaml:"logging"`
	Features   Features     `yaml:"features"`
	Kill       KillSwitches `yaml:"kill_switches"`

	// Derived at load, never from the file.
	identity s3id.Config
	token    string // the admin bearer token, from token_file or inline
}

// Listen is the data listener.
type Listen struct {
	Address           string        `yaml:"address"`
	TLS               TLS           `yaml:"tls"`
	ProxyProtocol     bool          `yaml:"proxy_protocol"`
	TrustedProxies    []string      `yaml:"trusted_proxies"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	MaxHeaderBytes    int           `yaml:"max_header_bytes"`
	MaxInflight       int           `yaml:"max_inflight"`
}

// TLS is the listener's certificate pair. Both empty means plaintext.
type TLS struct {
	Cert       string `yaml:"cert"`
	Key        string `yaml:"key"`
	MinVersion string `yaml:"min_version"`
}

// Backend is the one backend cluster and its endpoints.
type Backend struct {
	Scheme                string        `yaml:"scheme"`
	Endpoints             []string      `yaml:"endpoints"`
	TLS                   BackendTLS    `yaml:"tls"`
	DialTimeout           time.Duration `yaml:"dial_timeout"`
	ResponseHeaderTimeout time.Duration `yaml:"response_header_timeout"`
	IdleBodyTimeout       time.Duration `yaml:"idle_body_timeout"`
	MaxIdleConnsPerHost   int           `yaml:"max_idle_conns_per_host"`
	HealthCheck           HealthCheck   `yaml:"health_check"`
}

// BackendTLS is the client side of https to the backend.
type BackendTLS struct {
	CABundle           string `yaml:"ca_bundle"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
}

// HealthCheck is the active endpoint check.
type HealthCheck struct {
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
}

// Proxy is the data path's own knobs.
type Proxy struct {
	CopyBufferBytes int           `yaml:"copy_buffer_bytes"`
	DrainTimeout    time.Duration `yaml:"drain_timeout"`
}

// Cluster is the node-local multi-node divisor.
type Cluster struct {
	NodeCount int `yaml:"node_count"`
}

// RateLimits is the rule set (docs/SPEC.md, Rate limiting).
type RateLimits struct {
	DefaultOnLimit string `yaml:"default_on_limit"`
	Rules          []Rule `yaml:"rules"`
}

// Rule is one rate-limit rule. Validation is here; compilation into matchers and GCRA state is
// internal/limiter's (Phase 2).
type Rule struct {
	ID      string   `yaml:"id"`
	Match   Match    `yaml:"match"`
	GroupBy []string `yaml:"group_by"`
	Limit   Limit    `yaml:"limit"`
	OnLimit string   `yaml:"on_limit"`
}

// Match is a rule's conditions; every listed one must hold, an omitted one matches anything.
type Match struct {
	AccessKeys  []string `yaml:"access_keys"`
	Buckets     []string `yaml:"buckets"`
	Prefixes    []string `yaml:"prefixes"`
	SourceCIDRs []string `yaml:"source_cidrs"`
	Ops         []string `yaml:"ops"`
}

// Limit is rate and burst. Rate 0 blocks. Burst defaults to max(1, ceil(rate)).
type Limit struct {
	Rate  float64 `yaml:"rate"`
	Burst int     `yaml:"burst"`
}

// Limiter is the key store's bounds.
type Limiter struct {
	MaxKeys       int           `yaml:"max_keys"`
	SweepInterval time.Duration `yaml:"sweep_interval"`
}

// Showback is the hourly usage store.
type Showback struct {
	Dir              string              `yaml:"dir"`
	FlushInterval    time.Duration       `yaml:"flush_interval"`
	RetentionDays    *int                `yaml:"retention_days"` // nil = default; 0 = never delete
	Dimensions       []string            `yaml:"dimensions"`
	MaxTuplesPerHour int                 `yaml:"max_tuples_per_hour"`
	Prefixes         map[string][]string `yaml:"prefixes"`
}

// Admin is the admin listener.
type Admin struct {
	Address   string `yaml:"address"`
	TokenFile string `yaml:"token_file"`
	Token     string `yaml:"token"`
	Pprof     bool   `yaml:"pprof"`
}

// Logging is slog's configuration.
type Logging struct {
	Level          string `yaml:"level"`
	Access         bool   `yaml:"access"`
	ThrottleSample int    `yaml:"throttle_sample"`
}

// Mode names for throttle responses.
const (
	ModeSlowDown        = "slowdown"
	ModeTooManyRequests = "too_many_requests"
)

// Dimension names shared by group_by and showback.dimensions.
const (
	DimAccessKey   = "access_key"
	DimBucket      = "bucket"
	DimPrefix      = "prefix"
	DimSourceIP    = "source_ip"
	DimOperation   = "operation"
	DimStatusClass = "status_class"
)

// Defaults (docs/CONFIG.md). Each is applied only when the key is absent or zero.
const (
	DefaultListenAddress         = ":8443"
	DefaultMinTLSVersion         = "1.2"
	DefaultReadHeaderTimeout     = 10 * time.Second
	DefaultIdleTimeout           = 120 * time.Second
	DefaultMaxHeaderBytes        = 1 << 20
	DefaultMaxInflight           = 4096
	DefaultDialTimeout           = 2 * time.Second
	DefaultResponseHeaderTimeout = 300 * time.Second
	DefaultIdleBodyTimeout       = 60 * time.Second
	DefaultMaxIdleConnsPerHost   = 1024
	DefaultHealthInterval        = 5 * time.Second
	DefaultHealthTimeout         = 2 * time.Second
	DefaultCopyBufferBytes       = 128 << 10
	DefaultDrainTimeout          = 30 * time.Second
	DefaultNodeCount             = 1
	DefaultOnLimit               = ModeSlowDown
	DefaultMaxKeys               = 2_000_000
	DefaultSweepInterval         = 10 * time.Second
	DefaultFlushInterval         = 60 * time.Second
	DefaultRetentionDays         = 400
	DefaultMaxTuplesPerHour      = 250_000
	DefaultAdminAddress          = "127.0.0.1:9900"
	DefaultLogLevel              = "info"
	DefaultThrottleSample        = 100
)

// DefaultDimensions is showback.dimensions when unset.
var DefaultDimensions = []string{DimAccessKey, DimBucket, DimPrefix, DimOperation, DimStatusClass}

func (c *Config) applyDefaults() {
	if c.Listen.Address == "" {
		c.Listen.Address = DefaultListenAddress
	}
	if c.Listen.TLS.MinVersion == "" {
		c.Listen.TLS.MinVersion = DefaultMinTLSVersion
	}
	if c.Listen.ReadHeaderTimeout == 0 {
		c.Listen.ReadHeaderTimeout = DefaultReadHeaderTimeout
	}
	if c.Listen.IdleTimeout == 0 {
		c.Listen.IdleTimeout = DefaultIdleTimeout
	}
	if c.Listen.MaxHeaderBytes == 0 {
		c.Listen.MaxHeaderBytes = DefaultMaxHeaderBytes
	}
	if c.Listen.MaxInflight == 0 {
		c.Listen.MaxInflight = DefaultMaxInflight
	}
	if c.Backend.DialTimeout == 0 {
		c.Backend.DialTimeout = DefaultDialTimeout
	}
	if c.Backend.ResponseHeaderTimeout == 0 {
		c.Backend.ResponseHeaderTimeout = DefaultResponseHeaderTimeout
	}
	if c.Backend.IdleBodyTimeout == 0 {
		c.Backend.IdleBodyTimeout = DefaultIdleBodyTimeout
	}
	if c.Backend.MaxIdleConnsPerHost == 0 {
		c.Backend.MaxIdleConnsPerHost = DefaultMaxIdleConnsPerHost
	}
	if c.Backend.HealthCheck.Interval == 0 {
		c.Backend.HealthCheck.Interval = DefaultHealthInterval
	}
	if c.Backend.HealthCheck.Timeout == 0 {
		c.Backend.HealthCheck.Timeout = DefaultHealthTimeout
	}
	if c.Proxy.CopyBufferBytes == 0 {
		c.Proxy.CopyBufferBytes = DefaultCopyBufferBytes
	}
	if c.Proxy.DrainTimeout == 0 {
		c.Proxy.DrainTimeout = DefaultDrainTimeout
	}
	if c.Cluster.NodeCount == 0 {
		c.Cluster.NodeCount = DefaultNodeCount
	}
	if c.RateLimits.DefaultOnLimit == "" {
		c.RateLimits.DefaultOnLimit = DefaultOnLimit
	}
	for i := range c.RateLimits.Rules {
		r := &c.RateLimits.Rules[i]
		if r.OnLimit == "" {
			r.OnLimit = c.RateLimits.DefaultOnLimit
		}
		if r.Limit.Burst == 0 && r.Limit.Rate > 0 {
			r.Limit.Burst = int(r.Limit.Rate)
			if float64(r.Limit.Burst) < r.Limit.Rate {
				r.Limit.Burst++ // ceil
			}
			if r.Limit.Burst < 1 {
				r.Limit.Burst = 1
			}
		}
	}
	if c.Limiter.MaxKeys == 0 {
		c.Limiter.MaxKeys = DefaultMaxKeys
	}
	if c.Limiter.SweepInterval == 0 {
		c.Limiter.SweepInterval = DefaultSweepInterval
	}
	if c.Showback.FlushInterval == 0 {
		c.Showback.FlushInterval = DefaultFlushInterval
	}
	if c.Showback.RetentionDays == nil {
		d := DefaultRetentionDays
		c.Showback.RetentionDays = &d
	}
	if c.Showback.Dimensions == nil {
		c.Showback.Dimensions = append([]string(nil), DefaultDimensions...)
	}
	if c.Showback.MaxTuplesPerHour == 0 {
		c.Showback.MaxTuplesPerHour = DefaultMaxTuplesPerHour
	}
	if c.Admin.Address == "" {
		c.Admin.Address = DefaultAdminAddress
	}
	if c.Logging.Level == "" {
		c.Logging.Level = DefaultLogLevel
	}
	if c.Logging.ThrottleSample == 0 {
		c.Logging.ThrottleSample = DefaultThrottleSample
	}
}

// Retention is showback.retention_days with its default applied; 0 means never delete.
func (s *Showback) Retention() int {
	if s.RetentionDays == nil {
		return DefaultRetentionDays
	}
	return *s.RetentionDays
}

// Identity is the identity pass's configuration, derived once at load.
func (c *Config) Identity() *s3id.Config { return &c.identity }

// AdminToken is the bearer token the admin listener requires, "" for none. It is never part of
// the YAML round trip and never logged.
func (c *Config) AdminToken() string { return c.token }

// Redacted returns a copy safe to show: the inline token replaced by "<redacted>" when set. The
// token_file path itself is not secret.
func (c *Config) Redacted() *Config {
	out := *c
	if out.Admin.Token != "" {
		out.Admin.Token = "<redacted>"
	}
	out.token = ""
	return &out
}

// trustedPrefixes parses listen.trusted_proxies; a bare address is a /32 or /128.
func trustedPrefixes(entries []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(entries))
	for _, e := range entries {
		p, err := parseCIDR(e)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func parseCIDR(s string) (netip.Prefix, error) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}
