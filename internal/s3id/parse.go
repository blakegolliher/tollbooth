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
// Adapted from github.com/blakegolliher/shunt internal/s3/request.go (Parse, Domains,
// bucketFromHost, unescape) and internal/sigv4/canonical.go (RawPath) at commit
// 9b36d09ed109add4a2bedfe1dbfae6108038ee5b (Apache-2.0, Copyright 2026 Blake Golliher).
// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, 2026-11-09: the result is a
// RequestIdentity with access key (SigV4 and SigV2, header and query), source address (peer,
// PROXY protocol via the listener, trusted X-Forwarded-For), the list prefix (ADR-0001), class,
// and flags; the path is taken from r.RequestURI, never from the parsed URL; the query is scanned
// without a map.

package s3id

import (
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// Addressing is how the client addressed the bucket. Diagnostics only.
type Addressing uint8

// Addressing styles.
const (
	AddrPath Addressing = iota
	AddrVHost
)

func (a Addressing) String() string {
	if a == AddrVHost {
		return "vhost"
	}
	return "path"
}

// Level is the resource level of the request path.
type Level uint8

// Resource levels.
const (
	LevelService Level = iota
	LevelBucket
	LevelObject
)

func (l Level) String() string {
	switch l {
	case LevelBucket:
		return "bucket"
	case LevelObject:
		return "key"
	}
	return "service"
}

// Flags are qualifiers the classifier records but never discriminates on.
type Flags uint8

// Flag bits.
const (
	FlagVersionID  Flags = 1 << iota // versionId query present
	FlagPartNumber                   // partNumber query present
	FlagRange                        // Range header present
	FlagXID                          // x-id query present (AWS SDK v2 family)
	FlagAbsolute                     // the request-target was absolute-form or otherwise not origin-form
)

// RequestIdentity is everything the limiter and showback key off, computed from headers and the
// request line only, before a single body byte is read (docs/SPEC.md, Request identity). Every
// string field is a substring of the request, except Key and ListPrefix when they needed
// percent-decoding.
type RequestIdentity struct {
	AccessKey  string
	Bucket     string
	Key        string
	ListPrefix string
	SourceIP   netip.Addr
	Op         Op
	Class      Class
	Flags      Flags
	Addressing Addressing
	Level      Level
	Method     string
	// RawPath and RawQuery are the request-target as sent, split at the first '?', so the
	// outbound request is built from the same bytes the identity was (docs/DESIGN.md §4.4).
	RawPath  string
	RawQuery string
}

// Config is the identity pass's configuration: the virtual-hosted base domains and the peers
// whose X-Forwarded-For is honored. Built once by the config loader; immutable.
type Config struct {
	Domains        Domains
	TrustedProxies []netip.Prefix
}

// Domains is the set of configured base domains, pre-processed for suffix matching.
type Domains struct {
	suffixes []string // ".example.net" for "example.net" or "*.example.net"
	bare     []string // "example.net"
}

// NewDomains accepts entries like "s3.example.net" or "*.s3.example.net" (the wildcard form shunt
// used). A Host that ends in ".<entry>" names the bucket in front of the suffix; the bare entry
// itself is path-style.
func NewDomains(entries []string) Domains {
	var d Domains
	for _, e := range entries {
		e = strings.ToLower(strings.TrimSuffix(e, "."))
		if e == "" {
			continue
		}
		base := strings.TrimPrefix(e, "*.")
		d.suffixes = append(d.suffixes, "."+base)
		d.bare = append(d.bare, base)
	}
	return d
}

// Bare lists the configured base domains, for check-config's endpoint warning.
func (d Domains) Bare() []string { return d.bare }

// bucketFromHost returns the bucket named by a virtual-host Host header, or "" for path style.
// Host matching is case-insensitive; the bucket is returned in the case the client sent so the
// identity never rewrites what the backend will see.
func (d Domains) bucketFromHost(host string) string {
	if len(d.suffixes) == 0 || host == "" {
		return ""
	}
	host = stripPort(host)
	for i, suf := range d.suffixes {
		if len(host) == len(d.bare[i]) && strings.EqualFold(host, d.bare[i]) {
			return ""
		}
		if len(host) > len(suf) && strings.EqualFold(host[len(host)-len(suf):], suf) {
			return host[:len(host)-len(suf)]
		}
	}
	return ""
}

// stripPort removes a trailing ":port" from a Host value without net.SplitHostPort, which
// allocates an error when there is no port (the common case for a base domain). An IPv6 literal
// keeps its brackets; it can never match a base domain anyway.
func stripPort(host string) string {
	if strings.HasPrefix(host, "[") {
		if i := strings.IndexByte(host, ']'); i > 0 {
			return host[:i+1]
		}
		return host
	}
	if i := strings.IndexByte(host, ':'); i >= 0 && strings.IndexByte(host[i+1:], ':') < 0 {
		return host[:i]
	}
	return host
}

// RawTarget returns the request-target exactly as sent on the wire, split at the first '?'. The
// path is never EscapedPath(): the client signed the bytes it sent and the backend must see them.
// A client-side request (tests) carries the path in the URL instead. A non-origin-form target
// (absolute-form, `*`) is reduced to its path and flagged.
func RawTarget(r *http.Request) (rawPath, rawQuery string, absolute bool) {
	uri := r.RequestURI
	if uri == "" {
		uri = r.URL.EscapedPath()
		if r.URL.RawQuery != "" {
			uri += "?" + r.URL.RawQuery
		}
	}
	if i := strings.IndexByte(uri, '?'); i >= 0 {
		rawPath, rawQuery = uri[:i], uri[i+1:]
	} else {
		rawPath = uri
	}
	if rawPath == "" || rawPath[0] != '/' {
		if u, err := url.ParseRequestURI(rawPath); err == nil && strings.HasPrefix(u.Path, "/") {
			return u.EscapedPath(), rawQuery, true
		}
		return "/", rawQuery, true
	}
	return rawPath, rawQuery, false
}

// Parse extracts the identity from r. It never fails: an unrecognizable request is
// LevelService/OpUnknown and still proxied.
func Parse(r *http.Request, c *Config) RequestIdentity {
	rawPath, rawQuery, absolute := RawTarget(r)
	id := RequestIdentity{Method: r.Method, RawPath: rawPath, RawQuery: rawQuery}
	if absolute {
		id.Flags |= FlagAbsolute
	}
	rest := rawPath[1:]

	if b := c.Domains.bucketFromHost(r.Host); b != "" {
		id.Addressing = AddrVHost
		id.Bucket = b
		id.Level = LevelBucket
		if rest != "" {
			id.Level = LevelObject
			id.Key = unescape(rest)
		}
	} else if bucket, key, hasKey := strings.Cut(rest, "/"); bucket != "" {
		// An empty bucket segment ("//key") is not a bucket request; it stays at service level
		// with OpUnknown and is proxied as-is (found by shunt's FuzzParse).
		// The bucket segment is taken raw: valid bucket names never need escaping, and decoding
		// would let "%2F" smuggle a "/" into a bucket name (found by shunt's FuzzParse).
		id.Bucket = bucket
		id.Level = LevelBucket
		if hasKey && key != "" {
			id.Level = LevelObject
			id.Key = unescape(key)
		}
	}

	q := scanQuery(rawQuery)
	id.Op = classify(r.Method, id.Level, &q, r.Header)
	id.Class = id.Op.Class()
	if q.has(qVersionID) {
		id.Flags |= FlagVersionID
	}
	if q.has(qPartNumber) {
		id.Flags |= FlagPartNumber
	}
	if q.has(qXID) {
		id.Flags |= FlagXID
	}
	if v := r.Header["Range"]; len(v) > 0 {
		id.Flags |= FlagRange
	}
	if id.Op.IsBucketList() && q.has(qPrefix) {
		id.ListPrefix = unescapeQuery(q.prefix)
	}
	id.AccessKey = accessKey(r.Header, &q)
	id.SourceIP = sourceIP(r.RemoteAddr, r.Header["X-Forwarded-For"], c.TrustedProxies)
	return id
}

// unescape decodes percent-encoding exactly once; '+' stays literal (S3 keys are path segments,
// not form data). Allocates only when the input contains '%'. A malformed escape leaves the raw
// bytes: the backend will judge them.
func unescape(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

// unescapeQuery decodes a query value once: '+' is a space here, as in every S3 SDK's encoding
// of the prefix parameter.
func unescapeQuery(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}
	if u, err := url.QueryUnescape(s); err == nil {
		return u
	}
	return s
}

// accessKey extracts the access key id from the SigV4 or SigV2 Authorization header, or from the
// presigned query parameters. The header wins when both are present. Every result is a substring.
func accessKey(h http.Header, q *query) string {
	if v := h["Authorization"]; len(v) > 0 && v[0] != "" {
		a := v[0]
		switch {
		case strings.HasPrefix(a, "AWS4-HMAC-SHA256"):
			// AWS4-HMAC-SHA256 Credential=<AKID>/<date>/<region>/s3/aws4_request, SignedHeaders=..., Signature=...
			i := strings.Index(a, "Credential=")
			if i < 0 {
				return ""
			}
			a = a[i+len("Credential="):]
			if j := strings.IndexByte(a, '/'); j >= 0 {
				return a[:j]
			}
			if j := strings.IndexByte(a, ','); j >= 0 {
				return strings.TrimSpace(a[:j])
			}
			return a
		case strings.HasPrefix(a, "AWS "):
			// AWS <AKID>:<signature>
			a = a[len("AWS "):]
			if j := strings.IndexByte(a, ':'); j >= 0 {
				return a[:j]
			}
			return a
		}
		return ""
	}
	if q.has(qAmzCredential) {
		// X-Amz-Credential=<AKID>%2F<date>%2F<region>%2Fs3%2Faws4_request, or with literal slashes.
		c := q.credential
		if j := strings.IndexByte(c, '/'); j >= 0 {
			c = c[:j]
		}
		if j := indexFold(c, "%2f"); j >= 0 {
			c = c[:j]
		}
		return c
	}
	if q.has(qAWSAccessKeyID) {
		return q.awsAccessKeyID
	}
	return ""
}

// indexFold is a case-insensitive strings.Index for a short ASCII needle, without allocating.
func indexFold(s, needle string) int {
	for i := 0; i+len(needle) <= len(s); i++ {
		if strings.EqualFold(s[i:i+len(needle)], needle) {
			return i
		}
	}
	return -1
}

// sourceIP is the TCP peer (which the PROXY protocol listener has already replaced with the
// client's address when enabled), unless the peer is a trusted proxy, in which case it is the
// rightmost X-Forwarded-For entry that is not itself a trusted proxy. From an untrusted peer the
// header is ignored entirely: a tenant must not be able to pick its own rate-limit bucket.
func sourceIP(remoteAddr string, xff []string, trusted []netip.Prefix) netip.Addr {
	peer := parseAddr(remoteAddr)
	if len(trusted) == 0 || len(xff) == 0 || !inPrefixes(peer, trusted) {
		return peer
	}
	for i := len(xff) - 1; i >= 0; i-- {
		list := xff[i]
		for list != "" {
			var entry string
			if j := strings.LastIndexByte(list, ','); j >= 0 {
				entry, list = list[j+1:], list[:j]
			} else {
				entry, list = list, ""
			}
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			a := parseAddr(entry)
			if !a.IsValid() {
				// An unparseable hop: stop walking; the nearest parseable trusted hop is the best
				// identity available, and that is the peer.
				return peer
			}
			if !inPrefixes(a, trusted) {
				return a
			}
		}
	}
	return peer
}

// parseAddr accepts "ip", "ip:port", "[v6]:port", and "[v6]"; IPv4-mapped IPv6 is unmapped so a
// dual-stack listener and a CIDR rule agree. The form is decided from the bytes first, because
// netip's parsers allocate an error on the way to a failed attempt, and the X-Forwarded-For walk
// must stay allocation-free.
func parseAddr(s string) netip.Addr {
	var a netip.Addr
	var err error
	switch {
	case strings.HasPrefix(s, "["):
		if strings.HasSuffix(s, "]") {
			a, err = netip.ParseAddr(s[1 : len(s)-1])
		} else {
			var ap netip.AddrPort
			ap, err = netip.ParseAddrPort(s)
			a = ap.Addr()
		}
	case strings.Count(s, ":") == 1:
		var ap netip.AddrPort
		ap, err = netip.ParseAddrPort(s)
		a = ap.Addr()
	default:
		a, err = netip.ParseAddr(s)
	}
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

func inPrefixes(a netip.Addr, ps []netip.Prefix) bool {
	if !a.IsValid() {
		return false
	}
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
