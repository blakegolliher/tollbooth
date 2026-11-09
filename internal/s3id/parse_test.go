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
// Adapted from github.com/blakegolliher/shunt internal/s3/request_test.go at commit
// 9b36d09ed109add4a2bedfe1dbfae6108038ee5b (Apache-2.0, Copyright 2026 Blake Golliher).
// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, 2026-11-09: identity cases
// for access keys, source addresses, trusted X-Forwarded-For, the list prefix, flags, and the
// raw request-target round trip; FuzzParse keeps shunt's seed shape and corpus and gains the
// determinism and round-trip invariants from the test plan.

package s3id

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

var testCfg = &Config{
	Domains:        NewDomains([]string{"tb.example.com", "*.s3.example.net"}),
	TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")},
}

// req builds a server-side request the way net/http does: RequestURI carries the raw target.
func req(method, host, target string) *http.Request {
	r := httptest.NewRequest(method, "http://placeholder/", nil)
	r.Method = method
	r.RequestURI = target
	r.Host = host
	r.RemoteAddr = "192.0.2.10:51000"
	return r
}

func parse(method, host, target string) RequestIdentity {
	return Parse(req(method, host, target), testCfg)
}

func TestParseStyles(t *testing.T) {
	cases := []struct {
		name         string
		method, host string
		target       string
		bucket, key  string
		addr         Addressing
		level        Level
		op           Op
	}{
		{"path bucket", "GET", "tb.example.com:8443", "/b?list-type=2", "b", "", AddrPath, LevelBucket, OpListObjectsV2},
		{"path object", "GET", "tb.example.com", "/b/dir/k.txt", "b", "dir/k.txt", AddrPath, LevelObject, OpGetObject},
		{"path bucket trailing slash", "PUT", "tb.example.com", "/b/", "b", "", AddrPath, LevelBucket, OpCreateBucket},
		{"path service", "GET", "tb.example.com", "/", "", "", AddrPath, LevelService, OpListBuckets},
		{"vhost bucket", "GET", "b.tb.example.com:8443", "/", "b", "", AddrVHost, LevelBucket, OpListObjects},
		{"vhost object", "PUT", "b.tb.example.com", "/k", "b", "k", AddrVHost, LevelObject, OpPutObject},
		{"vhost dotted bucket", "HEAD", "my.bucket.tb.example.com", "/", "my.bucket", "", AddrVHost, LevelBucket, OpHeadBucket},
		{"vhost second domain (wildcard form)", "GET", "x.s3.example.net", "/k?uploadId=1", "x", "k", AddrVHost, LevelObject, OpListParts},
		{"unknown host is path style", "GET", "127.0.0.1:3900", "/b/k", "b", "k", AddrPath, LevelObject, OpGetObject},
		{"IPv6 literal host is path style", "GET", "[2001:db8::1]:8443", "/b/k", "b", "k", AddrPath, LevelObject, OpGetObject},
		{"missing host is path style", "GET", "", "/b/k", "b", "k", AddrPath, LevelObject, OpGetObject},
		{"escaped key", "GET", "tb.example.com", "/b/a%20b/c%2Fd", "b", "a b/c/d", AddrPath, LevelObject, OpGetObject},
		{"percent itself", "GET", "tb.example.com", "/b/100%25", "b", "100%", AddrPath, LevelObject, OpGetObject},
		{"plus stays literal", "GET", "tb.example.com", "/b/a+b", "b", "a+b", AddrPath, LevelObject, OpGetObject},
		{"unicode key", "GET", "tb.example.com", "/b/%CF%80/%E6%97%A5", "b", "π/日", AddrPath, LevelObject, OpGetObject},
		{"double slash key", "GET", "tb.example.com", "/b//k", "b", "/k", AddrPath, LevelObject, OpGetObject},
		{"trailing slash key", "PUT", "tb.example.com", "/b/dir/", "b", "dir/", AddrPath, LevelObject, OpPutObject},
		{"malformed escape left raw", "GET", "tb.example.com", "/b/%zz", "b", "%zz", AddrPath, LevelObject, OpGetObject},
		{"upper-case host", "GET", "B.TB.EXAMPLE.COM", "/", "B", "", AddrVHost, LevelBucket, OpListObjects},
		{"escaped slash in bucket stays raw", "GET", "tb.example.com", "/a%2Fb/k", "a%2Fb", "k", AddrPath, LevelObject, OpGetObject},
		{"empty bucket segment is service level", "PUT", "tb.example.com", "//k", "", "", AddrPath, LevelService, OpUnknown},
		{"vhost leading slash is the key /k", "GET", "b.tb.example.com", "//k", "b", "/k", AddrVHost, LevelObject, OpGetObject},
		{"query only path", "GET", "tb.example.com", "/?x-id=ListBuckets", "", "", AddrPath, LevelService, OpListBuckets},
		{"preflight", "OPTIONS", "b.tb.example.com", "/k", "b", "k", AddrVHost, LevelObject, OpPreflight},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parse(c.method, c.host, c.target)
			if got.Bucket != c.bucket || got.Key != c.key || got.Addressing != c.addr || got.Level != c.level || got.Op != c.op {
				t.Fatalf("got bucket=%q key=%q addr=%s level=%s op=%s; want bucket=%q key=%q addr=%s level=%s op=%s",
					got.Bucket, got.Key, got.Addressing, got.Level, got.Op, c.bucket, c.key, c.addr, c.level, c.op)
			}
			if got.Class != c.op.Class() || got.Method != c.method {
				t.Fatalf("class %s method %s", got.Class, got.Method)
			}
		})
	}
}

func TestParseLongKey(t *testing.T) {
	key := strings.Repeat("k", 1024)
	got := parse("GET", "tb.example.com", "/b/"+key)
	if got.Key != key || got.Level != LevelObject {
		t.Fatalf("long key not preserved: len=%d level=%s", len(got.Key), got.Level)
	}
}

func TestRawTarget(t *testing.T) {
	cases := []struct {
		target, path, query string
		absolute            bool
	}{
		{"/b/k%2F%20?partNumber=1&uploadId=x", "/b/k%2F%20", "partNumber=1&uploadId=x", false},
		{"/b//k", "/b//k", "", false},
		{"//k", "//k", "", false},
		{"/?delete", "/", "delete", false},
		{"/b/k?", "/b/k", "", false},
		{"http://host/b/k?x=1", "/b/k", "x=1", true},
		{"*", "/", "", true},
		{"", "/", "", false}, // the test helper's URL fallback is origin-form
	}
	for _, c := range cases {
		r := req("GET", "h", c.target)
		p, q, abs := RawTarget(r)
		if p != c.path || q != c.query || abs != c.absolute {
			t.Errorf("RawTarget(%q) = %q %q %v, want %q %q %v", c.target, p, q, abs, c.path, c.query, c.absolute)
		}
	}
	// Client-side requests carry the target in the URL.
	r := httptest.NewRequest("GET", "http://h/b/k%2Fx?list-type=2", nil)
	r.RequestURI = ""
	if p, q, _ := RawTarget(r); p != "/b/k%2Fx" || q != "list-type=2" {
		t.Errorf("client-side target: %q %q", p, q)
	}
	if id := parse("GET", "h", "http://h/b/k"); id.Flags&FlagAbsolute == 0 || id.Bucket != "b" {
		t.Errorf("absolute-form: %+v", id)
	}
}

func TestAccessKey(t *testing.T) {
	cases := []struct {
		name  string
		auth  string
		query string
		want  string
	}{
		{"sigv4 header", "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE1/20261109/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc", "", "AKIAEXAMPLE1"},
		{"sigv4 header temporary key", "AWS4-HMAC-SHA256 Credential=ASIATEMP/20261109/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc", "", "ASIATEMP"},
		{"sigv4 header without slash", "AWS4-HMAC-SHA256 Credential=AKIAODD, SignedHeaders=host", "", "AKIAODD"},
		{"sigv4 header malformed", "AWS4-HMAC-SHA256 bad", "", ""},
		{"sigv2 header", "AWS AKIAV2KEY:frob=", "", "AKIAV2KEY"},
		{"sigv2 header no colon", "AWS AKIAV2KEY", "", "AKIAV2KEY"},
		{"other auth scheme", "Bearer xyz", "", ""},
		{"sigv4 presigned encoded", "", "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAPRESIGN%2F20261109%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20261109T000000Z", "AKIAPRESIGN"},
		{"sigv4 presigned lower-case escape", "", "X-Amz-Credential=AKIALOWER%2f20261109%2fus-east-1%2fs3%2faws4_request", "AKIALOWER"},
		{"sigv4 presigned literal slashes", "", "X-Amz-Credential=AKIASLASH/20261109/us-east-1/s3/aws4_request", "AKIASLASH"},
		{"sigv2 presigned", "", "AWSAccessKeyId=AKIAV2Q&Expires=1&Signature=x", "AKIAV2Q"},
		{"header wins over query", "AWS4-HMAC-SHA256 Credential=AKIAHDR/x", "X-Amz-Credential=AKIAQRY%2Fx", "AKIAHDR"},
		{"anonymous", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			target := "/b/k"
			if c.query != "" {
				target += "?" + c.query
			}
			r := req("GET", "tb.example.com", target)
			if c.auth != "" {
				r.Header.Set("Authorization", c.auth)
			}
			if got := Parse(r, testCfg).AccessKey; got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestSourceIP(t *testing.T) {
	trusted := testCfg.TrustedProxies
	cases := []struct {
		name   string
		remote string
		xff    []string
		want   string
	}{
		{"peer only", "192.0.2.10:51000", nil, "192.0.2.10"},
		{"peer v6", "[2001:db8::7]:443", nil, "2001:db8::7"},
		{"peer v4-mapped v6 is unmapped", "[::ffff:192.0.2.10]:443", nil, "192.0.2.10"},
		{"untrusted peer ignores xff", "192.0.2.10:51000", []string{"203.0.113.9"}, "192.0.2.10"},
		{"trusted peer uses xff", "10.1.2.3:51000", []string{"203.0.113.9"}, "203.0.113.9"},
		{"trusted peer, rightmost untrusted entry", "10.1.2.3:51000", []string{"203.0.113.9, 198.51.100.4, 10.9.9.9"}, "198.51.100.4"},
		{"trusted peer, multiple header lines", "10.1.2.3:51000", []string{"203.0.113.9", "198.51.100.4, 10.9.9.9"}, "198.51.100.4"},
		{"trusted peer, all entries trusted", "10.1.2.3:51000", []string{"10.5.5.5, 10.9.9.9"}, "10.1.2.3"},
		{"trusted peer, forged garbage stops the walk", "10.1.2.3:51000", []string{"203.0.113.9, not-an-ip, 10.9.9.9"}, "10.1.2.3"},
		{"trusted peer, bracketed v6 with port", "[fd00::1]:443", []string{"[2001:db8::9]:1234"}, "2001:db8::9"},
		{"trusted peer, empty xff", "10.1.2.3:51000", []string{""}, "10.1.2.3"},
		{"unparseable peer", "unix", nil, "invalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sourceIP(c.remote, c.xff, trusted)
			if c.want == "invalid" {
				if got.IsValid() {
					t.Fatalf("got %s want invalid", got)
				}
				return
			}
			if got != netip.MustParseAddr(c.want) {
				t.Fatalf("got %s want %s", got, c.want)
			}
		})
	}
	// No trusted proxies configured: the header is never consulted.
	if got := sourceIP("10.1.2.3:1", []string{"203.0.113.9"}, nil); got != netip.MustParseAddr("10.1.2.3") {
		t.Fatalf("no trusted list: %s", got)
	}
}

func TestListPrefixAndFlags(t *testing.T) {
	id := parse("GET", "tb.example.com", "/b?list-type=2&prefix=ckpt%2Fa+b&x-id=ListObjectsV2")
	if id.Op != OpListObjectsV2 || id.ListPrefix != "ckpt/a b" || id.Flags&FlagXID == 0 {
		t.Fatalf("list: %+v", id)
	}
	if id := parse("GET", "tb.example.com", "/b?versions&prefix=logs/"); id.Op != OpListObjectVersions || id.ListPrefix != "logs/" {
		t.Fatalf("versions list: %+v", id)
	}
	if id := parse("GET", "tb.example.com", "/b?uploads&prefix=x"); id.Op != OpListMultipartUploads || id.ListPrefix != "x" {
		t.Fatalf("uploads list: %+v", id)
	}
	if id := parse("GET", "tb.example.com", "/b"); id.ListPrefix != "" {
		t.Fatalf("no prefix param: %q", id.ListPrefix)
	}
	// prefix on a non-list operation is not a list prefix (ADR-0001: ListParts is keyed by key).
	if id := parse("GET", "tb.example.com", "/b/k?uploadId=1&prefix=x"); id.Op != OpListParts || id.ListPrefix != "" {
		t.Fatalf("ListParts: %+v", id)
	}
	r := req("GET", "tb.example.com", "/b/k?versionId=v1&partNumber=3")
	r.Header.Set("Range", "bytes=0-1")
	id = Parse(r, testCfg)
	if id.Op != OpGetObject || id.Flags != FlagVersionID|FlagPartNumber|FlagRange {
		t.Fatalf("flags: %+v", id)
	}
}

// The identity pass allocates nothing for a request whose key and prefix need no decoding
// (docs/DESIGN.md §9, stage 3).
func TestParseAllocatesNothing(t *testing.T) {
	r := req("PUT", "bucket.tb.example.com:8443", "/dir/k?partNumber=3&uploadId=abc&x-id=UploadPart")
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE1/20261109/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc")
	r.RemoteAddr = "10.1.2.3:51000"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.9.9.9")
	allocs := testing.AllocsPerRun(1000, func() {
		id := Parse(r, testCfg)
		if id.Op != OpUploadPart || id.AccessKey != "AKIAEXAMPLE1" || id.SourceIP != netip.MustParseAddr("203.0.113.9") {
			t.Fatal("misparsed")
		}
	})
	if allocs != 0 {
		t.Fatalf("Parse allocates %.1f per request; the budget is 0", allocs)
	}
	// Path-style with a bare host (no port): net.SplitHostPort would allocate an error here.
	r2 := req("GET", "tb.example.com", "/training-data/ckpt/step-000120/rank-0007.pt")
	r2.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE1/20261109/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc")
	if allocs := testing.AllocsPerRun(1000, func() {
		if id := Parse(r2, testCfg); id.Op != OpGetObject || id.Bucket != "training-data" {
			t.Fatal("misparsed")
		}
	}); allocs != 0 {
		t.Fatalf("Parse (bare host) allocates %.1f per request; the budget is 0", allocs)
	}
	// Anonymous, unknown method: still nothing.
	r3 := req("PATCH", "127.0.0.1:9000", "/b/k")
	if allocs := testing.AllocsPerRun(1000, func() {
		if id := Parse(r3, testCfg); id.Op != OpUnknown {
			t.Fatal("misparsed")
		}
	}); allocs != 0 {
		t.Fatalf("Parse (unknown method) allocates %.1f per request; the budget is 0", allocs)
	}
}

func BenchmarkParse(b *testing.B) {
	r := req("PUT", "bucket.tb.example.com:8443", "/k?partNumber=3&uploadId=abc")
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE1/20261109/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc")
	b.ReportAllocs()
	for b.Loop() {
		if Parse(r, testCfg).Op != OpUploadPart {
			b.Fatal("misparsed")
		}
	}
}

func BenchmarkParseGetObject100Rules(b *testing.B) {
	r := req("GET", "tb.example.com", "/training-data/ckpt/step-000120/rank-0007.pt")
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE1/20261109/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc")
	b.ReportAllocs()
	for b.Loop() {
		if Parse(r, testCfg).Op != OpGetObject {
			b.Fatal("misparsed")
		}
	}
}

// FuzzParse: never panics, classification is deterministic, and the raw path round-trips
// byte-for-byte for every origin-form target (test plan category 4). The seed shape is shunt's
// so its committed corpus applies.
func FuzzParse(f *testing.F) {
	f.Add("GET", "b.tb.example.com", "/k?uploadId=1")
	f.Add("PUT", "tb.example.com:8443", "/b/k%2F%20?partNumber=1&uploadId=x")
	f.Add("POST", "127.0.0.1", "/b?delete")
	f.Add("GET", "", "//")
	f.Add("OPTIONS", "b.s3.example.net", "/%zz")
	f.Add("GET", "", "/0%2F0")
	f.Add("PUT", "", "//%20")
	f.Add("GET", "b.tb.example.com", "/?list-type=2&prefix=a%2Fb+c&X-Amz-Credential=AK%2Fd%2Fr%2Fs3%2Faws4_request")
	f.Add("GET", "tb.example.com", "/b/k?annotation&annotationName=n&versionId=v")
	f.Add("GET", "tb.example.com", "http://tb.example.com/b/k")
	f.Add("DELETE", "[::1]:9", "/b/%E6%97%A5/x?tagging")
	f.Fuzz(func(t *testing.T, method, host, target string) {
		if strings.ContainsAny(target, " \r\n") || strings.ContainsAny(host, " /\r\n") {
			return
		}
		r := req(method, host, target)
		r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+host+"/x")
		id := Parse(r, testCfg)
		if strings.Contains(id.Bucket, "/") {
			t.Fatalf("bucket contains '/': %q from %q", id.Bucket, target)
		}
		if id.Level == LevelService && (id.Bucket != "" || id.Key != "") {
			t.Fatalf("service level with bucket/key: %+v", id)
		}
		if id.Level == LevelBucket && id.Key != "" {
			t.Fatalf("bucket level with key: %+v", id)
		}
		if id.Level == LevelObject && (id.Bucket == "" || id.Key == "") {
			t.Fatalf("object level missing bucket or key: %+v", id)
		}
		if id.Class != id.Op.Class() {
			t.Fatalf("class mismatch: %+v", id)
		}
		if again := Parse(r, testCfg); again != id {
			t.Fatalf("not deterministic: %+v vs %+v", id, again)
		}
		p, q, abs := RawTarget(r)
		if !abs {
			want := target
			if i := strings.IndexByte(target, '?'); i >= 0 {
				if p != target[:i] || q != target[i+1:] {
					t.Fatalf("raw target did not round-trip: %q -> %q %q", target, p, q)
				}
			} else if p != want || q != "" {
				t.Fatalf("raw target did not round-trip: %q -> %q %q", target, p, q)
			}
		}
		_ = id.Op.String()
	})
}

// FuzzScanQuery: the scanner never panics and agrees with url.ParseQuery on which known keys are
// present whenever the query is one ParseQuery accepts (the only disagreement allowed is a
// percent-encoded key name, which the scanner treats as unknown, as S3 does).
func FuzzScanQuery(f *testing.F) {
	for _, s := range []string{"", "a=1", "uploads", "uploadId=1&partNumber=2", "list-type=2&list-type=3", "&&=&", "prefix=%zz", "x-id=GetObject&versionId=", "%75ploads"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		q := scanQuery(raw)
		if strings.Contains(raw, "%") {
			return
		}
		for key, bit := range queryBits {
			present := false
			for _, pair := range strings.Split(raw, "&") {
				k, _, _ := strings.Cut(pair, "=")
				if k == key {
					present = true
				}
			}
			if present != q.has(bit) {
				t.Fatalf("key %q: scanner says %v, split says %v (%q)", key, q.has(bit), present, raw)
			}
		}
	})
}
