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
// Adapted from github.com/blakegolliher/shunt test/s3diff/main.go at commit
// 9b36d09ed109add4a2bedfe1dbfae6108038ee5b (Apache-2.0, Copyright 2026 Blake Golliher).
// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, 2026-11-09: the mixed and
// resign modes are gone; the header allow-list has no proxy headers because tollbooth adds none;
// a -record mode runs the proxy and the recording backend in-process and diffs the requests the
// backend received as well as the responses; checksum algorithms, Expect: 100-continue, and
// DeleteObjects with Content-MD5 join the matrix.

// Command s3diff runs an S3 operation matrix against a backend directly and through tollbooth and
// diffs status, headers (minus an allow-list of per-request values), and body. The proxy's core
// property is transparency; this measures it (test plan category 5). Test tool only: it imports
// aws-sdk-go-v2 (Apache-2.0, docs/deps.md) and never links into the proxy binary.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// headerAllow lists response headers that legitimately differ between direct and proxied
// responses: per-request values the backend mints, and hop-by-hop headers the proxy strips.
// Nothing here is a header tollbooth adds; it adds none.
var headerAllow = map[string]bool{
	"Date": true, "X-Amz-Request-Id": true, "X-Amz-Id-2": true,
	"Connection": true, "Keep-Alive": true, // hop-by-hop: stripped by the proxy, present direct
	"X-Amz-Version-Id": true, // each side does its own PUT; versioned backends mint a new id per write
	"X-Vast-Rcf-Id":    true, // VAST's per-request trace id
}

// xmlNoise strips per-request values from XML bodies before comparing them.
var xmlNoise = regexp.MustCompile(`<(UploadId|RequestId|HostId|LastModified|CreationDate|Initiated|Date|VersionId|DeleteMarkerVersionId)>[^<]*</`)

// locationPort strips the port from <Location>: backends echo the Host header into it, and the
// proxy's listener port differs from the backend's. Passthrough preserves Host by design.
var locationPort = regexp.MustCompile(`(<Location>[a-z]+://[^:/<]+):\d+`)

// probe is what we compare: the last HTTP response seen by a client.
type probe struct {
	status  int
	headers http.Header
	bodyLen int64
	bodySum string
	body    []byte // kept for XML normalisation only (small responses)
	err     string
}

// recorder captures every response on a transport since the last take(). Bodies are hashed as
// the SDK reads them, so a multi-step case (multipart) compares each step in order.
type recorder struct {
	rt   http.RoundTripper
	mu   sync.Mutex
	seen []*probe
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.rt.RoundTrip(req)
	p := &probe{}
	if err != nil {
		p.err = err.Error()
		r.set(p)
		return resp, err
	}
	p.status = resp.StatusCode
	p.headers = resp.Header.Clone()
	hb := &hashBody{ReadCloser: resp.Body, h: sha256.New(), p: p}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "xml") || ct == "" {
		hb.keep = &bytes.Buffer{} // XML, or unlabelled (Garage sends XML with no Content-Type)
	}
	resp.Body = hb
	r.set(p)
	return resp, nil
}

func (r *recorder) set(p *probe) { r.mu.Lock(); r.seen = append(r.seen, p); r.mu.Unlock() }
func (r *recorder) take() []*probe {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.seen
	r.seen = nil
	if len(out) == 0 {
		out = []*probe{{err: "no response recorded"}}
	}
	return out
}

type hashBody struct {
	io.ReadCloser
	h    hash.Hash
	p    *probe
	keep *bytes.Buffer
}

func (b *hashBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.h.Write(p[:n])
		b.p.bodyLen += int64(n)
		if b.keep != nil && b.keep.Len() < 1<<20 {
			b.keep.Write(p[:n])
		}
	}
	if err == io.EOF {
		b.p.bodySum = hex.EncodeToString(b.h.Sum(nil))
		if b.keep != nil {
			b.p.body = b.keep.Bytes()
		}
	}
	return n, err
}

// target is one side of the comparison: an SDK client and a raw client sharing one recorder.
type target struct {
	name     string
	client   *s3.Client
	rec      *recorder
	httpc    *http.Client
	endpoint string
	ak, sk   string
	region   string
}

// newTarget builds the clients. When addr is set, every *.domain / domain host dials addr.
func newTarget(name, endpoint, addr, domain, region, ak, sk, caFile string, pathStyle bool) (*target, error) {
	tr := &http.Transport{
		DisableCompression: true,
		ForceAttemptHTTP2:  false,
		TLSNextProto:       map[string]func(string, *tls.Conn) http.RoundTripper{},
		DialContext: func(ctx context.Context, network, host string) (net.Conn, error) {
			h, _, err := net.SplitHostPort(host)
			if addr != "" && err == nil && (h == domain || strings.HasSuffix(h, "."+domain)) {
				host = addr
			}
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, host)
		},
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(pem)
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	rec := &recorder{rt: tr}
	cfg := aws.Config{
		Region:                     region,
		Credentials:                credentials.NewStaticCredentialsProvider(ak, sk, ""),
		HTTPClient:                 &http.Client{Transport: rec},
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		RetryMaxAttempts:           1,
	}
	c := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = pathStyle
	})
	return &target{name: name, client: c, rec: rec, httpc: &http.Client{Transport: rec}, endpoint: endpoint, ak: ak, sk: sk, region: region}, nil
}

type result struct {
	name   string
	op     string
	direct []*probe
	via    []*probe
	diffs  []string
	gap    bool // the backend rejected a raw case on both sides identically: a backend gap, not a proxy diff
}

func (r *result) compare(compareBody bool) {
	if len(r.direct) != len(r.via) {
		r.diffs = append(r.diffs, fmt.Sprintf("step count: direct=%d via=%d", len(r.direct), len(r.via)))
		return
	}
	for i := range r.direct {
		r.compareStep(i, r.direct[i], r.via[i], compareBody)
	}
}

func (r *result) compareStep(i int, d, v *probe, compareBody bool) {
	prefix := ""
	if len(r.direct) > 1 {
		prefix = fmt.Sprintf("step %d: ", i+1)
	}
	add := func(format string, a ...any) { r.diffs = append(r.diffs, prefix+fmt.Sprintf(format, a...)) }
	if d.err != "" || v.err != "" {
		if d.err != v.err {
			add("error: direct=%q via=%q", d.err, v.err)
		}
		return
	}
	if d.status != v.status {
		add("status: direct=%d via=%d", d.status, v.status)
		if len(d.body) > 0 {
			add("direct body: %s", trunc(string(d.body)))
		}
		if len(v.body) > 0 {
			add("via body: %s", trunc(string(v.body)))
		}
	}
	keys := map[string]bool{}
	for k := range d.headers {
		keys[k] = true
	}
	for k := range v.headers {
		keys[k] = true
	}
	var sorted []string
	for k := range keys {
		if !headerAllow[k] {
			sorted = append(sorted, k)
		}
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		if k == "Content-Length" && (d.body != nil || v.body != nil) {
			continue // XML bodies are compared after normalisation; their length follows
		}
		if k == "Content-Length" && d.status == http.StatusNoContent && v.status == http.StatusNoContent {
			continue // RFC 9110 §8.6: a 204 must not carry Content-Length; Go's server drops a backend's "0"
		}
		if a, b := strings.Join(d.headers[k], ","), strings.Join(v.headers[k], ","); a != b {
			add("header %s: direct=%q via=%q", k, a, b)
		}
	}
	if !compareBody {
		return
	}
	if d.body != nil || v.body != nil {
		a, b := normalise(d.body), normalise(v.body)
		if a != b {
			add("xml body differs (normalised):\n  direct: %s\n  via:    %s", trunc(a), trunc(b))
		}
		return
	}
	if d.bodyLen != v.bodyLen || d.bodySum != v.bodySum {
		add("body: direct=%d/%s via=%d/%s", d.bodyLen, short(d.bodySum), v.bodyLen, short(v.bodySum))
	}
}

// owned reports whether a CreateBucket failed only because the bucket is already ours.
func owned(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "BucketAlreadyOwnedByYou") || strings.Contains(msg, "BucketAlreadyExists")
}

// parseSizes reads the -sizes list.
func parseSizes(list string) ([]int, error) {
	parts := strings.Split(list, ",")
	sizes := make([]int, 0, len(parts))
	for _, s := range parts {
		var n int
		if _, err := fmt.Sscan(s, &n); err != nil {
			return nil, fmt.Errorf("bad size %q: %w", s, err)
		}
		sizes = append(sizes, n)
	}
	return sizes, nil
}

func normalise(body []byte) string {
	out := xmlNoise.ReplaceAllString(string(body), "<$1>*</")
	out = locationPort.ReplaceAllString(out, "$1")
	return sortBuckets(out)
}

// bucketEntry matches one <Bucket> of a ListBuckets response. Backends are not required to return
// buckets in a stable order (Garage does not), so the comparison sorts them; the proxy forwards
// the backend's bytes unchanged either way.
var bucketEntry = regexp.MustCompile(`<Bucket>.*?</Bucket>`)

func sortBuckets(body string) string {
	i, j := strings.Index(body, "<Buckets>"), strings.Index(body, "</Buckets>")
	if i < 0 || j < i {
		return body
	}
	inner := body[i+len("<Buckets>") : j]
	entries := bucketEntry.FindAllString(inner, -1)
	sort.Strings(entries)
	return body[:i+len("<Buckets>")] + strings.Join(entries, "") + body[j:]
}

// truncLen caps the diff detail printed per body; S3DIFF_TRUNC raises it while investigating one.
var truncLen = func() int {
	if v := os.Getenv("S3DIFF_TRUNC"); v != "" {
		var n int
		if _, err := fmt.Sscan(v, &n); err == nil && n > 0 {
			return n
		}
	}
	return 300
}()

func trunc(s string) string {
	if len(s) > truncLen {
		return s[:truncLen] + "…"
	}
	return s
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

var trickyKeys = map[string]string{
	"plain":    "dir/plain.bin",
	"spaces":   "dir with space/file name.bin",
	"unicode":  "dir/ünïcödé-日本語.bin",
	"plus":     "dir/a+b.bin",
	"percent":  "dir/100%.bin",
	"dslash":   "dir//double.bin",
	"trailing": "dir/trailing/",
	"long":     strings.Repeat("k", 1024),
}

// matrixOptions drive one run of the matrix for one addressing style.
type matrixOptions struct {
	ctx      context.Context
	style    string
	bucket   string
	prefix   string // "" unless an existing bucket is used
	existing bool
	sizes    []int
	keep     bool
	sigModes bool
	// afterCase runs after each case with the recorded requests from both sides (record mode).
	afterCase func(r *result)
	// checkContent compares a GET's body with what was uploaded. Off in record mode: the null
	// backend answers every GET with a canned body, so content is not what it tests.
	checkContent bool
	// setup runs a case's preparatory traffic (a PUT before a DELETE); record mode uses it to
	// keep that traffic out of the request comparison.
	setup func(f func() error) error
}

func (o *matrixOptions) prep(f func() error) error {
	if o.setup != nil {
		return o.setup(f)
	}
	return f()
}

// runMatrix runs every case against direct and via and returns the results.
func runMatrix(direct, via *target, o matrixOptions) (results []result, failed, gaps int) { //nolint:gocritic // one matrix, one function, in the order the cases run
	ctx, bkt, prefix := o.ctx, o.bucket, o.prefix
	run := func(name, op string, body bool, f func(*target) error) {
		r := result{name: o.style + "/" + name, op: op}
		ed := f(direct)
		r.direct = direct.rec.take()
		ev := f(via)
		r.via = via.rec.take()
		r.compare(body)
		if errors.Is(ed, errContent) {
			r.diffs = append(r.diffs, "direct: "+errContent.Error())
		}
		if errors.Is(ev, errContent) {
			r.diffs = append(r.diffs, "via: "+errContent.Error())
		}
		if o.afterCase != nil {
			o.afterCase(&r)
		}
		if strings.HasPrefix(name, "sig/") && len(r.diffs) == 0 && r.direct[len(r.direct)-1].status >= 400 {
			// The backend refused this signing mode identically on both sides: a backend gap the
			// proxy relayed faithfully, reported apart from passes so the gate counts it.
			r.gap = true
			gaps++
		} else if len(r.diffs) > 0 {
			failed++
		}
		results = append(results, r)
	}

	// Bucket lifecycle: create direct, list via both, delete at the end via both (second is 404).
	if !o.existing {
		if err := o.prep(func() error {
			_, err := direct.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bkt})
			if err != nil && owned(err) {
				return nil
			}
			return err
		}); err != nil {
			fmt.Fprintf(os.Stderr, "create bucket %s: %v\n", bkt, err)
			os.Exit(2)
		}
		direct.rec.take() // setup, not a compared step
	}
	run("bucket", "HeadBucket", false, func(t *target) error {
		_, err := t.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &bkt})
		return err
	})
	run("bucket", "ListBuckets", true, func(t *target) error {
		_, err := t.client.ListBuckets(ctx, &s3.ListBucketsInput{})
		return err
	})
	run("bucket", "GetBucketLocation", true, func(t *target) error {
		_, err := t.client.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: &bkt})
		return err
	})

	for kname, key := range trickyKeys {
		if kname == "long" {
			key = strings.Repeat("k", 1024-len(prefix)-len("-1048576"))
		}
		key = prefix + key
		for _, size := range o.sizes {
			if kname != "plain" && size > 4096 {
				continue // tricky keys are about naming; sizes are about streaming
			}
			payload := deterministic(size)
			sum := sha256.Sum256(payload)
			k := key
			if size > 0 || kname == "plain" {
				k = fmt.Sprintf("%s-%d", key, size)
				if kname == "trailing" {
					k = key + fmt.Sprint(size) + "/"
				}
			}
			label := fmt.Sprintf("%s/%d", kname, size)
			run(label, "PutObject", false, func(t *target) error {
				_, err := t.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &bkt, Key: &k, Body: bytes.NewReader(payload), ContentLength: aws.Int64(int64(size))})
				return err
			})
			run(label, "HeadObject", false, func(t *target) error {
				_, err := t.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bkt, Key: &k})
				return err
			})
			run(label, "GetObject", true, func(t *target) error {
				out, err := t.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bkt, Key: &k})
				if err != nil {
					return err
				}
				got, _ := io.ReadAll(out.Body)
				_ = out.Body.Close()
				if o.checkContent && sha256.Sum256(got) != sum {
					return errContent
				}
				return nil
			})
			if size > 100 {
				run(label, "GetObject(range)", true, func(t *target) error {
					out, err := t.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bkt, Key: &k, Range: aws.String("bytes=1-100")})
					if err != nil {
						return err
					}
					_, _ = io.Copy(io.Discard, out.Body)
					return out.Body.Close()
				})
			}
			if kname == "plain" && size == 4096 {
				run(label, "GetObjectAttributes", true, func(t *target) error {
					_, err := t.client.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{Bucket: &bkt, Key: &k,
						ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesEtag, types.ObjectAttributesObjectSize}})
					return err
				})
			}
		}
	}

	// Checksum algorithms through the SDK (header form), and every raw signing mode once,
	// path-style, after the SDK matrix's PUTs.
	if o.sigModes && o.style == "path" {
		for _, algo := range []types.ChecksumAlgorithm{types.ChecksumAlgorithmCrc32, types.ChecksumAlgorithmCrc32c, types.ChecksumAlgorithmSha1, types.ChecksumAlgorithmSha256, types.ChecksumAlgorithmCrc64nvme} {
			payload := deterministic(8192)
			k := prefix + "sdk-checksum/" + strings.ToLower(string(algo))
			run("checksum/"+strings.ToLower(string(algo)), "PutObject", false, func(t *target) error {
				_, err := t.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &bkt, Key: &k, Body: bytes.NewReader(payload), ContentLength: aws.Int64(8192), ChecksumAlgorithm: algo})
				return err
			})
			run("checksum/"+strings.ToLower(string(algo)), "HeadObject(checksum)", false, func(t *target) error {
				_, err := t.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bkt, Key: &k, ChecksumMode: types.ChecksumModeEnabled})
				return err
			})
		}
		for _, m := range signingModes() {
			for _, size := range []int{1, 200 << 10} {
				payload := deterministic(size)
				sum := sha256.Sum256(payload)
				k := prefix + fmt.Sprintf("sig/%s-%d", m.name, size)
				label := fmt.Sprintf("sig/%s/%d", m.name, size)
				run(label, "PutObject", false, func(t *target) error { return t.rawPut(ctx, bkt, k, payload, m) })
				switch m.payload {
				case "presigned":
					run(label, "GetObject(presigned)", true, func(t *target) error { return t.rawPresignedGet(ctx, bkt, k, sum, o.checkContent) })
				case "sigv2", "sigv2-presigned":
					run(label, "GetObject(sigv2)", true, func(t *target) error { return t.rawGetV2(ctx, bkt, k, m.payload == "sigv2-presigned") })
				default:
					run(label, "GetObject", true, func(t *target) error {
						out, err := t.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bkt, Key: &k})
						if err != nil {
							return err
						}
						got, _ := io.ReadAll(out.Body)
						_ = out.Body.Close()
						if o.checkContent && sha256.Sum256(got) != sum {
							return errContent
						}
						return nil
					})
				}
			}
		}
		// Expect: 100-continue on a raw PUT, accepted and refused (bad signature) paths.
		run("expect/accepted", "PutObject(expect)", false, func(t *target) error {
			return t.rawPutExpect(ctx, bkt, prefix+"expect/ok", deterministic(64<<10), true)
		})
		run("expect/refused", "PutObject(expect,badsig)", false, func(t *target) error {
			return t.rawPutExpect(ctx, bkt, prefix+"expect/bad", deterministic(64<<10), false)
		})
	}

	run("list", "ListObjectsV2", true, func(t *target) error {
		_, err := t.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &bkt, Prefix: aws.String(prefix + "dir")})
		return err
	})
	run("list", "ListObjectsV2(delim)", true, func(t *target) error {
		_, err := t.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &bkt, Prefix: aws.String(prefix), Delimiter: aws.String("/"), MaxKeys: aws.Int32(3)})
		return err
	})
	run("list", "ListObjects", true, func(t *target) error {
		_, err := t.client.ListObjects(ctx, &s3.ListObjectsInput{Bucket: &bkt, Prefix: aws.String(prefix + "dir/")})
		return err
	})

	// Multipart: 5 parts of 5 MiB + a final 1 MiB, same key sequentially on both sides.
	mpKey := prefix + "dir/multipart.bin"
	parts := [][]byte{deterministic(5 << 20), deterministic(5<<20 + 1), deterministic(5<<20 + 2), deterministic(5<<20 + 3), deterministic(5<<20 + 4), deterministic(1 << 20)}
	run("multipart", "CreateMultipartUpload", true, func(t *target) error {
		out, err := t.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &bkt, Key: &mpKey})
		if err != nil {
			return err
		}
		uid := out.UploadId
		var done []types.CompletedPart
		for i, p := range parts {
			up, uerr := t.client.UploadPart(ctx, &s3.UploadPartInput{Bucket: &bkt, Key: &mpKey, UploadId: uid, PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(p), ContentLength: aws.Int64(int64(len(p)))}) //nolint:gosec // G115: six parts
			if uerr != nil {
				return uerr
			}
			done = append(done, types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(int32(i + 1))}) //nolint:gosec // G115: six parts
		}
		if _, err = t.client.ListParts(ctx, &s3.ListPartsInput{Bucket: &bkt, Key: &mpKey, UploadId: uid}); err != nil {
			return err
		}
		if _, err = t.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: &bkt}); err != nil {
			return err
		}
		_, err = t.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &bkt, Key: &mpKey, UploadId: uid,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: done}})
		return err
	})
	run("multipart", "HeadObject", false, func(t *target) error {
		_, err := t.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bkt, Key: &mpKey})
		return err
	})
	run("multipart", "GetObject(partNumber)", false, func(t *target) error {
		out, err := t.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bkt, Key: &mpKey, PartNumber: aws.Int32(2)})
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, out.Body)
		return out.Body.Close()
	})
	run("multipart", "HeadObject(partNumber)", false, func(t *target) error {
		_, err := t.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bkt, Key: &mpKey, PartNumber: aws.Int32(2)})
		return err
	})
	run("multipart", "UploadPartCopy", true, func(t *target) error {
		dst := prefix + "dir/multipart-copy.bin"
		out, err := t.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &bkt, Key: &dst})
		if err != nil {
			return err
		}
		up, err := t.client.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: &bkt, Key: &dst, UploadId: out.UploadId, PartNumber: aws.Int32(1), CopySource: aws.String(bkt + "/" + mpKey), CopySourceRange: aws.String("bytes=0-5242879")})
		if err != nil {
			return err
		}
		_, err = t.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &bkt, Key: &dst, UploadId: out.UploadId,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{ETag: up.CopyPartResult.ETag, PartNumber: aws.Int32(1)}}}})
		return err
	})
	run("multipart", "AbortMultipartUpload", false, func(t *target) error {
		out, err := t.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &bkt, Key: aws.String(prefix + "dir/aborted.bin")})
		if err != nil {
			return err
		}
		_, err = t.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &bkt, Key: aws.String(prefix + "dir/aborted.bin"), UploadId: out.UploadId})
		return err
	})
	run("copy", "CopyObject", true, func(t *target) error {
		_, err := t.client.CopyObject(ctx, &s3.CopyObjectInput{Bucket: &bkt, Key: aws.String(prefix + "dir/copied.bin"), CopySource: aws.String(bkt + "/" + prefix + "dir/plain.bin-4096")})
		return err
	})

	// Errors: the proxy must relay them unchanged.
	run("errors", "GetObject(404)", true, func(t *target) error {
		_, err := t.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &bkt, Key: aws.String(prefix + "does/not/exist")})
		return err
	})
	run("errors", "HeadBucket(404)", false, func(t *target) error {
		_, err := t.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bkt + "-nope")})
		return err
	})
	run("errors", "GetObject(badsig)", true, func(t *target) error { return t.rawGetBadSignature(ctx, bkt, prefix+"dir/plain.bin-4096") })

	// Deletes: put direct, delete via each side in turn (the second sees the object again).
	delKey := prefix + "dir/todelete.bin"
	run("delete", "DeleteObject", false, func(t *target) error {
		if err := o.prep(func() error {
			_, err := direct.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &bkt, Key: &delKey, Body: bytes.NewReader([]byte("x"))})
			return err
		}); err != nil {
			return err
		}
		direct.rec.take() // setup
		_, err := t.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bkt, Key: &delKey})
		return err
	})
	run("delete", "DeleteObjects", true, func(t *target) error {
		var ids []types.ObjectIdentifier
		if err := o.prep(func() error {
			for i := 0; i < 3; i++ {
				k := fmt.Sprintf("%sdir/batch-%d", prefix, i)
				if _, err := direct.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &bkt, Key: &k, Body: bytes.NewReader([]byte("x"))}); err != nil {
					return err
				}
				ids = append(ids, types.ObjectIdentifier{Key: aws.String(k)})
			}
			return nil
		}); err != nil {
			return err
		}
		direct.rec.take() // setup
		_, err := t.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &bkt, Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(false)}})
		return err
	})
	if o.style == "path" {
		run("delete", "DeleteObjects(Content-MD5)", true, func(t *target) error {
			var keys []string
			if err := o.prep(func() error {
				for i := 0; i < 2; i++ {
					k := fmt.Sprintf("%sdir/md5-%d", prefix, i)
					if _, err := direct.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &bkt, Key: &k, Body: bytes.NewReader([]byte("x"))}); err != nil {
						return err
					}
					keys = append(keys, k)
				}
				return nil
			}); err != nil {
				return err
			}
			direct.rec.take() // setup
			return t.rawDeleteObjectsMD5(ctx, bkt, keys)
		})
	}

	if !o.keep {
		// Remove everything this run wrote (only the run's prefix in an existing bucket).
		_ = o.prep(func() error { sweep(ctx, direct, bkt, prefix); return nil })
		direct.rec.take()
	}
	if !o.keep && !o.existing {

		run("bucket", "DeleteBucket", false, func(t *target) error {
			if err := o.prep(func() error {
				if _, err := direct.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &bkt}); err != nil {
					if _, err := direct.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bkt}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return err
			}
			direct.rec.take() // setup traffic is not part of the comparison
			_, err := t.client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bkt})
			return err
		})
	}
	return results, failed, gaps
}

// report prints the table and the tallies; returns the exit code.
func report(results []result, failed, gaps int, header string) int {
	fmt.Printf("%-44s %-26s %-9s %-9s %s\n", "case", "op", "direct", "via", "result")
	type tally struct{ cases, ok, diff, gap int }
	groups := map[string]*tally{}
	var order []string
	for _, r := range results {
		res := "ok"
		switch {
		case r.gap:
			res = "GAP(backend)"
		case len(r.diffs) > 0:
			res = "DIFF"
		}
		fmt.Printf("%-44s %-26s %-9s %-9s %s\n", r.name, r.op, st(r.direct), st(r.via), res)
		for _, d := range r.diffs {
			fmt.Printf("    %s\n", d)
		}
		g := "sdk matrix (" + strings.SplitN(r.name, "/", 2)[0] + ")"
		if parts := strings.Split(r.name, "/"); len(parts) > 2 && (parts[1] == "sig" || parts[1] == "checksum" || parts[1] == "expect") {
			g = parts[1] + " " + parts[2]
		}
		if groups[g] == nil {
			groups[g] = &tally{}
			order = append(order, g)
		}
		t := groups[g]
		t.cases++
		switch {
		case r.gap:
			t.gap++
		case len(r.diffs) > 0:
			t.diff++
		default:
			t.ok++
		}
	}
	fmt.Printf("\n%s\n", header)
	fmt.Printf("%-48s %6s %6s %6s %6s\n", "group", "cases", "ok", "diff", "gap")
	for _, g := range order {
		t := groups[g]
		fmt.Printf("%-48s %6d %6d %6d %6d\n", g, t.cases, t.ok, t.diff, t.gap)
	}
	fmt.Printf("\n%d cases, %d diffs, %d backend gaps\n", len(results), failed, gaps)
	if failed > 0 {
		return 1
	}
	return 0
}

func st(ps []*probe) string {
	p := ps[len(ps)-1]
	if p.err != "" && p.status == 0 {
		return "ERR"
	}
	if len(ps) > 1 {
		return fmt.Sprintf("%d(%d)", p.status, len(ps))
	}
	return fmt.Sprint(p.status)
}

// deterministic returns n bytes that differ across positions so truncation and reordering show.
func deterministic(n int) []byte {
	b := make([]byte, n)
	var x uint32 = 2463534242
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x) //nolint:gosec // G115: the low byte is the intent
	}
	return b
}

func main() {
	var (
		directEP  = flag.String("direct", "http://127.0.0.1:3900", "backend endpoint URL (its host is rewritten to -direct-addr when set)")
		directAd  = flag.String("direct-addr", "", "backend host:port to dial for the direct side")
		viaEP     = flag.String("via", "http://127.0.0.1:8443", "tollbooth endpoint URL")
		viaAd     = flag.String("via-addr", "", "tollbooth host:port to dial for the via side")
		domain    = flag.String("domain", "tb.example.com", "virtual-hosted base domain (vhost style dials -*-addr for *.domain)")
		region    = flag.String("region", "garage", "signing region")
		caFile    = flag.String("ca", "", "CA for a TLS tollbooth endpoint")
		bucket    = flag.String("bucket", "s3diff", "bucket name (created and deleted)")
		sizesFlag = flag.String("sizes", "0,1,4096,1048576,67108864", "object sizes")
		keep      = flag.Bool("keep", false, "keep the bucket afterwards")
		akEnv     = flag.String("access-key-env", "AWS_ACCESS_KEY_ID", "env var with the access key")
		skEnv     = flag.String("secret-env", "AWS_SECRET_ACCESS_KEY", "env var with the secret")
		stylesArg = flag.String("styles", "path,vhost", "addressing styles to run")
		existing  = flag.String("existing-bucket", "", "use this existing bucket; nothing is created or deleted except keys under a unique prefix")
		sigModes  = flag.Bool("signing-modes", true, "run the raw signing-mode, checksum, and Expect cases")
		record    = flag.Bool("record", false, "self-contained: run the recording backend and tollbooth in-process and diff the requests the backend received too")
	)
	flag.Parse()
	sizes, err := parseSizes(*sizesFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if *record {
		os.Exit(runRecord(sizes, strings.Split(*stylesArg, ","), *sigModes))
	}
	ak, sk := os.Getenv(*akEnv), os.Getenv(*skEnv)
	if ak == "" || sk == "" {
		fmt.Fprintf(os.Stderr, "credentials required in %s and %s\n", *akEnv, *skEnv)
		os.Exit(2)
	}
	var runID [4]byte
	_, _ = rand.Read(runID[:])
	ctx := context.Background()
	var all []result
	failed, gaps := 0, 0
	for _, style := range strings.Split(*stylesArg, ",") {
		pathStyle := style == "path"
		direct, err := newTarget("direct", *directEP, *directAd, *domain, *region, ak, sk, "", pathStyle)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		via, err := newTarget("via", *viaEP, *viaAd, *domain, *region, ak, sk, *caFile, pathStyle)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		bkt, prefix := *bucket+"-"+style, ""
		if *existing != "" {
			bkt, prefix = *existing, "s3diff/"+hex.EncodeToString(runID[:])+"/"+style+"/"
		}
		rs, f, g := runMatrix(direct, via, matrixOptions{ctx: ctx, style: style, bucket: bkt, prefix: prefix, existing: *existing != "", sizes: sizes, keep: *keep, sigModes: *sigModes, checkContent: true})
		all = append(all, rs...)
		failed += f
		gaps += g
	}
	os.Exit(report(all, failed, gaps, fmt.Sprintf("direct=%s via=%s region=%s", *directEP, *viaEP, *region)))
}

// sweep deletes every key under prefix in bucket on the direct side.
func sweep(ctx context.Context, direct *target, bkt, prefix string) {
	var token *string
	for {
		list, err := direct.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &bkt, Prefix: aws.String(prefix), ContinuationToken: token})
		if err != nil {
			return
		}
		for _, obj := range list.Contents {
			_, _ = direct.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bkt, Key: obj.Key})
		}
		if list.IsTruncated == nil || !*list.IsTruncated {
			return
		}
		token = list.NextContinuationToken
	}
}
