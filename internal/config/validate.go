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
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/blakegolliher/tollbooth/internal/s3id"
)

var ruleID = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Validate checks the whole file against the rules in docs/CONFIG.md and returns every finding,
// joined, so an operator fixes a file in one pass. It reads the TLS pair to prove it loads; it
// does not touch the network.
func (c *Config) Validate() error { //nolint:gocritic // one function, one pass over the file, in the file's order
	var errs []error
	bad := func(key, format string, args ...any) {
		errs = append(errs, &Error{Key: key, Msg: fmt.Sprintf(format, args...)})
	}

	if c.NodeID == "" {
		bad("node_id", "required; must be stable across restarts (a pod name is the wrong choice)")
	}
	for i, d := range c.BaseDomains {
		if d == "" || strings.ContainsAny(d, " /:") {
			bad(fmt.Sprintf("base_domains[%d]", i), "not a domain name: %q", d)
		}
	}

	if _, _, err := net.SplitHostPort(c.Listen.Address); err != nil {
		bad("listen.address", "%v", err)
	}
	switch {
	case c.Listen.TLS.Cert == "" && c.Listen.TLS.Key == "":
	case c.Listen.TLS.Cert == "" || c.Listen.TLS.Key == "":
		bad("listen.tls", "cert and key must be set together")
	default:
		if _, err := tls.LoadX509KeyPair(c.Listen.TLS.Cert, c.Listen.TLS.Key); err != nil {
			bad("listen.tls", "%v", err)
		}
	}
	if c.Listen.TLS.MinVersion != "1.2" && c.Listen.TLS.MinVersion != "1.3" {
		bad("listen.tls.min_version", "must be 1.2 or 1.3, got %q", c.Listen.TLS.MinVersion)
	}
	for i, p := range c.Listen.TrustedProxies {
		if _, err := parseCIDR(p); err != nil {
			bad(fmt.Sprintf("listen.trusted_proxies[%d]", i), "%v", err)
		}
	}
	positive := func(key string, v int64) {
		if v <= 0 {
			bad(key, "must be positive, got %d", v)
		}
	}
	positive("listen.read_header_timeout", int64(c.Listen.ReadHeaderTimeout))
	positive("listen.idle_timeout", int64(c.Listen.IdleTimeout))
	positive("listen.max_header_bytes", int64(c.Listen.MaxHeaderBytes))
	positive("listen.max_inflight", int64(c.Listen.MaxInflight))

	switch c.Backend.Scheme {
	case "":
		bad("backend.scheme", "required: http or https")
	case "http":
		if c.Backend.TLS.CABundle != "" || c.Backend.TLS.InsecureSkipVerify {
			bad("backend.tls", "not allowed with scheme http")
		}
	case "https":
		if c.Backend.TLS.CABundle != "" {
			if _, err := os.Stat(c.Backend.TLS.CABundle); err != nil {
				bad("backend.tls.ca_bundle", "%v", err)
			}
		}
	default:
		bad("backend.scheme", "must be http or https, got %q", c.Backend.Scheme)
	}
	if len(c.Backend.Endpoints) == 0 {
		bad("backend.endpoints", "required: at least one host:port")
	}
	for i, e := range c.Backend.Endpoints {
		key := fmt.Sprintf("backend.endpoints[%d]", i)
		if strings.Contains(e, "://") {
			bad(key, "no scheme here; backend.scheme applies to every endpoint")
			continue
		}
		if strings.Contains(e, "/") {
			bad(key, "no path allowed")
			continue
		}
		if _, _, err := net.SplitHostPort(e); err != nil {
			bad(key, "%v", err)
		}
	}
	positive("backend.dial_timeout", int64(c.Backend.DialTimeout))
	positive("backend.response_header_timeout", int64(c.Backend.ResponseHeaderTimeout))
	positive("backend.idle_body_timeout", int64(c.Backend.IdleBodyTimeout))
	positive("backend.max_idle_conns_per_host", int64(c.Backend.MaxIdleConnsPerHost))
	positive("backend.health_check.interval", int64(c.Backend.HealthCheck.Interval))
	positive("backend.health_check.timeout", int64(c.Backend.HealthCheck.Timeout))

	if c.Proxy.CopyBufferBytes < 4096 {
		bad("proxy.copy_buffer_bytes", "must be at least 4096, got %d", c.Proxy.CopyBufferBytes)
	}
	positive("proxy.drain_timeout", int64(c.Proxy.DrainTimeout))
	positive("cluster.node_count", int64(c.Cluster.NodeCount))

	if !validMode(c.RateLimits.DefaultOnLimit) {
		bad("rate_limits.default_on_limit", "must be %s or %s, got %q", ModeSlowDown, ModeTooManyRequests, c.RateLimits.DefaultOnLimit)
	}
	ids := map[string]int{}
	for i := range c.RateLimits.Rules {
		r := &c.RateLimits.Rules[i]
		key := fmt.Sprintf("rate_limits.rules[%d]", i)
		if !ruleID.MatchString(r.ID) {
			bad(key+".id", "required; [A-Za-z0-9_.-]{1,64}, got %q", r.ID)
		} else if prev, dup := ids[r.ID]; dup {
			bad(key+".id", "duplicate of rules[%d]", prev)
		} else {
			ids[r.ID] = i
		}
		for j, cidr := range r.Match.SourceCIDRs {
			if _, err := parseCIDR(cidr); err != nil {
				bad(fmt.Sprintf("%s.match.source_cidrs[%d]", key, j), "%v", err)
			}
		}
		for j, op := range r.Match.Ops {
			if _, ok := s3id.ParseClass(op); ok {
				continue
			}
			if _, ok := s3id.ParseOp(op); ok {
				continue
			}
			bad(fmt.Sprintf("%s.match.ops[%d]", key, j), "unknown operation or class %q (see docs/OPERATIONS_TABLE.md)", op)
		}
		for j, p := range r.Match.Prefixes {
			if p == "" {
				bad(fmt.Sprintf("%s.match.prefixes[%d]", key, j), "empty prefix; omit match.prefixes to match every key")
			}
		}
		seenDim := map[string]bool{}
		for j, g := range r.GroupBy {
			gk := fmt.Sprintf("%s.group_by[%d]", key, j)
			switch g {
			case DimAccessKey, DimBucket, DimSourceIP:
			case DimPrefix:
				if len(r.Match.Prefixes) == 0 {
					bad(gk, "prefix needs match.prefixes")
				}
			default:
				bad(gk, "must be one of access_key, bucket, prefix, source_ip, got %q", g)
			}
			if seenDim[g] {
				bad(gk, "repeated")
			}
			seenDim[g] = true
		}
		if r.Limit.Rate < 0 {
			bad(key+".limit.rate", "must be >= 0, got %v", r.Limit.Rate)
		}
		if r.Limit.Rate > 0 && r.Limit.Burst < 1 {
			bad(key+".limit.burst", "must be >= 1, got %d", r.Limit.Burst)
		}
		if !validMode(r.OnLimit) {
			bad(key+".on_limit", "must be %s or %s, got %q", ModeSlowDown, ModeTooManyRequests, r.OnLimit)
		}
	}
	positive("limiter.max_keys", int64(c.Limiter.MaxKeys))
	positive("limiter.sweep_interval", int64(c.Limiter.SweepInterval))

	if c.Showback.Dir == "" {
		bad("showback.dir", "required; a persistent path for the hourly files")
	}
	positive("showback.flush_interval", int64(c.Showback.FlushInterval))
	if c.Showback.Retention() < 0 {
		bad("showback.retention_days", "must be >= 0 (0 disables deletion)")
	}
	hasOp, hasStatus := false, false
	seenDim := map[string]bool{}
	for i, d := range c.Showback.Dimensions {
		key := fmt.Sprintf("showback.dimensions[%d]", i)
		switch d {
		case DimAccessKey, DimBucket, DimPrefix, DimSourceIP:
		case DimOperation:
			hasOp = true
		case DimStatusClass:
			hasStatus = true
		default:
			bad(key, "unknown dimension %q", d)
		}
		if seenDim[d] {
			bad(key, "repeated")
		}
		seenDim[d] = true
	}
	if !hasOp || !hasStatus {
		bad("showback.dimensions", "operation and status_class are always present and must be listed")
	}
	positive("showback.max_tuples_per_hour", int64(c.Showback.MaxTuplesPerHour))
	for b, ps := range c.Showback.Prefixes {
		if !ValidBucketName(b) {
			bad("showback.prefixes."+b, "not a valid bucket name")
		}
		for j, p := range ps {
			if p == "" {
				bad(fmt.Sprintf("showback.prefixes.%s[%d]", b, j), "empty prefix")
			}
		}
	}

	if _, _, err := net.SplitHostPort(c.Admin.Address); err != nil {
		bad("admin.address", "%v", err)
	}
	if c.Admin.Token != "" && c.Admin.TokenFile != "" {
		bad("admin", "token and token_file are mutually exclusive; prefer token_file")
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		bad("logging.level", "must be debug, info, warn, or error, got %q", c.Logging.Level)
	}
	positive("logging.throttle_sample", int64(c.Logging.ThrottleSample))

	return errors.Join(errs...)
}

func validMode(m string) bool { return m == ModeSlowDown || m == ModeTooManyRequests }

// Warnings returns the non-fatal findings check-config prints (docs/CONFIG.md, Validation rules).
// memLimit is the GOMEMLIMIT in bytes the checking environment carries, 0 when unset.
func (c *Config) Warnings(memLimit int64) []string {
	var w []string
	bare := c.identity.Domains.Bare()
	for _, e := range c.Backend.Endpoints {
		host, _, err := net.SplitHostPort(e)
		if err != nil {
			continue
		}
		if net.ParseIP(host) != nil {
			continue
		}
		found := false
		for _, d := range bare {
			if strings.EqualFold(host, d) {
				found = true
			}
		}
		if !found && len(bare) > 0 {
			w = append(w, fmt.Sprintf("backend endpoint %s is not among base_domains %v: because Host is preserved, the backend must accept the proxy's names as virtual-host domains, or clients must use path-style addressing (docs/OPERATIONS.md)", e, bare))
		}
	}
	if len(bare) == 0 && len(c.Backend.Endpoints) > 0 {
		w = append(w, "base_domains is empty: virtual-hosted-style requests will be treated as path-style; set it if clients use bucket.<domain> addressing")
	}
	if c.Backend.TLS.InsecureSkipVerify {
		w = append(w, "backend.tls.insecure_skip_verify is true: certificate verification is off (lab only)")
	}
	if c.Showback.Retention() == 0 {
		w = append(w, "showback.retention_days is 0: sealed files are never deleted")
	}
	if memLimit > 0 {
		worst := int64(c.Listen.MaxInflight) * int64(c.Proxy.CopyBufferBytes)
		if worst > memLimit/2 {
			w = append(w, fmt.Sprintf("listen.max_inflight (%d) × proxy.copy_buffer_bytes (%d) = %d bytes exceeds half of GOMEMLIMIT (%d); lower one of them or raise GOMEMLIMIT (D-024)",
				c.Listen.MaxInflight, c.Proxy.CopyBufferBytes, worst, memLimit))
		}
	}
	return w
}

// ReloadCompatible reports the keys that cannot change on reload (docs/CONFIG.md, Reload
// semantics). A change to any of them is a reload error that keeps the running config.
func (c *Config) ReloadCompatible(prev *Config) error {
	var errs []error
	fixed := func(key, old, cur string) {
		if old != cur {
			errs = append(errs, &Error{Key: key, Msg: fmt.Sprintf("cannot change on reload (%q → %q); restart tollbooth", old, cur)})
		}
	}
	fixed("listen.address", prev.Listen.Address, c.Listen.Address)
	fixed("admin.address", prev.Admin.Address, c.Admin.Address)
	fixed("node_id", prev.NodeID, c.NodeID)
	fixed("showback.dir", prev.Showback.Dir, c.Showback.Dir)
	fixed("listen.proxy_protocol", strconv.FormatBool(prev.Listen.ProxyProtocol), strconv.FormatBool(c.Listen.ProxyProtocol))
	return errors.Join(errs...)
}

// ValidBucketName reports whether name follows AWS's rules for general purpose bucket names:
// 3–63 characters of lowercase letters, digits, '.' and '-'; starts and ends with a letter or
// digit; no two adjacent periods; not formatted as an IPv4 address; none of AWS's reserved
// prefixes or suffixes. Adapted from shunt internal/s3/bucketname.go at 9b36d09 (Apache-2.0).
func ValidBucketName(name string) bool {
	if len(name) < 3 || len(name) > 63 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '.' || c == '-':
			if i == 0 || i == len(name)-1 {
				return false
			}
			if c == '.' && name[i-1] == '.' {
				return false
			}
		default:
			return false
		}
	}
	if ip := net.ParseIP(name); ip != nil {
		return false
	}
	for _, p := range []string{"xn--", "sthree-", "amzn-s3-demo-"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	for _, s := range []string{"-s3alias", "--ol-s3", ".mrap", "--x-s3", "--table-s3"} {
		if strings.HasSuffix(name, s) {
			return false
		}
	}
	return true
}
