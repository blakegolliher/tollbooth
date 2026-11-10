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

package main

// The raw signing-mode dimension: every client payload mode, sent by a raw client, because
// aws-sdk-go-v2 cannot emit the signed-chunk modes or SigV2 at all. The SDK's signer produces the
// SigV4 header and presigned forms; the aws-chunked framings, the per-chunk signatures, and SigV2
// are built here. This is a test tool: tollbooth itself never signs or verifies anything.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"  //nolint:gosec // Content-MD5 is what S3 asks for on DeleteObjects
	"crypto/sha1" //nolint:gosec // S3 checksum algorithm, not a security primitive
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"math/bits"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// errContent marks a GET whose body does not match what the case uploaded.
var errContent = errors.New("body does not match the uploaded payload")

const (
	unsignedPayload                 = "UNSIGNED-PAYLOAD"
	streamingSignedPayload          = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	streamingSignedPayloadTrailer   = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	streamingUnsignedPayloadTrailer = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	chunkSize                       = 64 << 10
	timeFormat                      = "20060102T150405Z"
	dateFormat                      = "20060102"
)

type sigMode struct {
	name    string
	payload string // x-amz-content-sha256 value, "sha256" for a hex digest, "presigned", "sigv2", "sigv2-presigned"
	algo    string // trailer checksum algorithm, "" for none
}

var checksumAlgos = []string{"crc32", "crc32c", "sha1", "sha256", "crc64nvme"}

func signingModes() []sigMode {
	m := []sigMode{
		{"unsigned", unsignedPayload, ""},
		{"sha256", "sha256", ""},
		{"streaming-signed", streamingSignedPayload, ""},
	}
	for _, a := range checksumAlgos {
		m = append(m, sigMode{"streaming-signed-trailer-" + a, streamingSignedPayloadTrailer, a})
	}
	for _, a := range checksumAlgos {
		m = append(m, sigMode{"unsigned-trailer-" + a, streamingUnsignedPayloadTrailer, a})
	}
	return append(m, sigMode{"presigned", "presigned", ""}, sigMode{"sigv2", "sigv2", ""}, sigMode{"sigv2-presigned", "sigv2-presigned", ""})
}

func newChecksum(algo string) hash.Hash {
	switch algo {
	case "crc32":
		return crc32.NewIEEE()
	case "crc32c":
		return crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case "sha1":
		return sha1.New() //nolint:gosec // S3 checksum algorithm
	case "sha256":
		return sha256.New()
	case "crc64nvme":
		return crc64.New(crc64.MakeTable(bits.Reverse64(0xad93d23594c93659)))
	}
	return nil
}

// escapeKey percent-encodes each path segment the way S3 clients do, keeping "/".
func escapeKey(key string) string {
	parts := strings.Split(key, "/")
	for i := range parts {
		parts[i] = awsEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

// awsEscape is the SigV4 URI encoding: unreserved characters stay, everything else is %XX.
func awsEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func (t *target) objectURL(bucket, key string) string {
	return strings.TrimRight(t.endpoint, "/") + "/" + bucket + "/" + escapeKey(key)
}

func (t *target) creds() aws.Credentials {
	return aws.Credentials{AccessKeyID: t.ak, SecretAccessKey: t.sk}
}

func (t *target) send(req *http.Request) error {
	resp, err := t.httpc.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// signV4 signs req with the SDK's signer for the given payload hash and fixed time.
func (t *target) signV4(ctx context.Context, req *http.Request, payloadHash string, now time.Time) error {
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	return v4.NewSigner().SignHTTP(ctx, t.creds(), req, payloadHash, "s3", t.region, now)
}

// signingKey derives kSigning for date/region/s3, as the SigV4 specification defines it.
func signingKey(secret, date, region string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	k = hmacSHA256(k, []byte(region))
	k = hmacSHA256(k, []byte("s3"))
	return hmacSHA256(k, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// rawPut uploads payload to bucket/key in signing mode m.
func (t *target) rawPut(ctx context.Context, bucket, key string, payload []byte, m sigMode) error {
	now := t.now()
	u := t.objectURL(bucket, key)
	n := int64(len(payload))
	switch m.payload {
	case "presigned":
		// The signer signs whatever query is present; X-Amz-Expires is the S3 presigner's job, so
		// it is added here, as every SDK's presigner does.
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, u+"?X-Amz-Expires=300", http.NoBody)
		if err != nil {
			return err
		}
		signed, _, err := v4.NewSigner().PresignHTTP(ctx, t.creds(), req, unsignedPayload, "s3", t.region, now)
		if err != nil {
			return err
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPut, signed, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.ContentLength = n
		return t.send(req)

	case "sigv2":
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.ContentLength = n
		req.Header.Set("Content-Type", "application/octet-stream")
		t.signV2(req, bucket, key, now, false)
		return t.send(req)

	case "sigv2-presigned":
		exp := now.Add(5 * time.Minute).Unix()
		q := url.Values{"AWSAccessKeyId": {t.ak}, "Expires": {strconv.FormatInt(exp, 10)}}
		q.Set("Signature", t.v2Signature(http.MethodPut, "", "", strconv.FormatInt(exp, 10), nil, "/"+bucket+"/"+key))
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, u+"?"+q.Encode(), bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.ContentLength = n
		return t.send(req)

	case unsignedPayload, "sha256":
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.ContentLength = n
		hashValue := unsignedPayload
		if m.payload == "sha256" {
			hashValue = sha256Hex(payload)
		}
		if err := t.signV4(ctx, req, hashValue, now); err != nil {
			return err
		}
		return t.send(req)

	case streamingUnsignedPayloadTrailer:
		name := "x-amz-checksum-" + m.algo
		body := encodeUnsignedChunks(payload, name, m.algo)
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Encoding", "aws-chunked")
		req.Header.Set("X-Amz-Decoded-Content-Length", strconv.FormatInt(n, 10))
		req.Header.Set("X-Amz-Trailer", name)
		if err := t.signV4(ctx, req, streamingUnsignedPayloadTrailer, now); err != nil {
			return err
		}
		return t.send(req)

	case streamingSignedPayload, streamingSignedPayloadTrailer:
		name := ""
		if m.algo != "" {
			name = "x-amz-checksum-" + m.algo
		}
		// Sign the headers first with a placeholder body length, then build the body from the
		// seed signature. The encoded length is deterministic, so Content-Length is set before
		// signing (it is not a signed header in SigV4, but the Transport needs it).
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, http.NoBody)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Encoding", "aws-chunked")
		req.Header.Set("X-Amz-Decoded-Content-Length", strconv.FormatInt(n, 10))
		if name != "" {
			req.Header.Set("X-Amz-Trailer", name)
		}
		if err := t.signV4(ctx, req, m.payload, now); err != nil {
			return err
		}
		auth := req.Header.Get("Authorization")
		seed := auth[strings.LastIndex(auth, "Signature=")+len("Signature="):]
		scope := now.Format(dateFormat) + "/" + t.region + "/s3/aws4_request"
		key := signingKey(t.sk, now.Format(dateFormat), t.region)
		body := encodeSignedChunks(payload, key, seed, now.Format(timeFormat), scope, name, m.algo)
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		return t.send(req)
	}
	return errors.New("unknown signing mode " + m.name)
}

// encodeUnsignedChunks frames payload as aws-chunked with an unsigned trailing checksum.
func encodeUnsignedChunks(payload []byte, trailerName, algo string) []byte {
	var b bytes.Buffer
	h := newChecksum(algo)
	for off := 0; off < len(payload); off += chunkSize {
		end := min(off+chunkSize, len(payload))
		fmt.Fprintf(&b, "%x\r\n", end-off)
		b.Write(payload[off:end])
		h.Write(payload[off:end])
		b.WriteString("\r\n")
	}
	b.WriteString("0\r\n")
	fmt.Fprintf(&b, "%s:%s\r\n\r\n", trailerName, base64.StdEncoding.EncodeToString(h.Sum(nil)))
	return b.Bytes()
}

// encodeSignedChunks frames payload as aws-chunked with a signature per chunk, and, when
// trailerName is set, a signed trailing checksum.
func encodeSignedChunks(payload, key []byte, seed, ts, scope, trailerName, algo string) []byte {
	var b bytes.Buffer
	prev := seed
	emptyHash := sha256Hex(nil)
	chunkSig := func(data []byte) string {
		sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + ts + "\n" + scope + "\n" + prev + "\n" + emptyHash + "\n" + sha256Hex(data)
		prev = hex.EncodeToString(hmacSHA256(key, []byte(sts)))
		return prev
	}
	var h hash.Hash
	if trailerName != "" {
		h = newChecksum(algo)
	}
	for off := 0; off < len(payload); off += chunkSize {
		end := min(off+chunkSize, len(payload))
		chunk := payload[off:end]
		fmt.Fprintf(&b, "%x;chunk-signature=%s\r\n", len(chunk), chunkSig(chunk))
		b.Write(chunk)
		b.WriteString("\r\n")
		if h != nil {
			h.Write(chunk)
		}
	}
	fmt.Fprintf(&b, "0;chunk-signature=%s\r\n", chunkSig(nil))
	if trailerName != "" {
		trailer := trailerName + ":" + base64.StdEncoding.EncodeToString(h.Sum(nil)) + "\n"
		sts := "AWS4-HMAC-SHA256-TRAILER\n" + ts + "\n" + scope + "\n" + prev + "\n" + sha256Hex([]byte(trailer))
		sig := hex.EncodeToString(hmacSHA256(key, []byte(sts)))
		b.WriteString(strings.TrimSuffix(trailer, "\n") + "\r\n")
		b.WriteString("x-amz-trailer-signature:" + sig + "\r\n")
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

// rawPresignedGet reads bucket/key through a SigV4 presigned URL and checks the body.
func (t *target) rawPresignedGet(ctx context.Context, bucket, key string, want [32]byte, checkContent bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.objectURL(bucket, key)+"?X-Amz-Expires=300", http.NoBody)
	if err != nil {
		return err
	}
	signed, _, err := v4.NewSigner().PresignHTTP(ctx, t.creds(), req, unsignedPayload, "s3", t.region, t.now())
	if err != nil {
		return err
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, signed, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := t.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // tool
	got, _ := io.ReadAll(resp.Body)
	if checkContent && resp.StatusCode == http.StatusOK && sha256.Sum256(got) != want {
		return errContent
	}
	return nil
}

// rawGetBadSignature sends a SigV4 request whose signature is wrong; both sides must refuse it
// the same way (the proxy never judges signatures).
func (t *target) rawGetBadSignature(ctx context.Context, bucket, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.objectURL(bucket, key), http.NoBody)
	if err != nil {
		return err
	}
	bad := *t
	bad.sk = "not-the-secret"
	if err := bad.signV4(ctx, req, unsignedPayload, t.now()); err != nil {
		return err
	}
	return t.send(req)
}

// rawPutExpect sends a PUT with Expect: 100-continue; with good=false the signature is wrong, so
// the backend refuses from the headers and no body byte should be needed on either side.
func (t *target) rawPutExpect(ctx context.Context, bucket, key string, payload []byte, good bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, t.objectURL(bucket, key), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(payload))
	req.Header.Set("Expect", "100-continue")
	signer := *t
	if !good {
		signer.sk = "not-the-secret"
	}
	if serr := signer.signV4(ctx, req, unsignedPayload, t.now()); serr != nil {
		return serr
	}
	// A transport of its own, so the 100-continue timeout is the SDK's 1 s and the connection is
	// not reused after a refused body.
	tr := &http.Transport{ExpectContinueTimeout: time.Second, DisableKeepAlives: true, DisableCompression: true}
	if base, ok := t.rec.rt.(*http.Transport); ok {
		tr.DialContext, tr.TLSClientConfig = base.DialContext, base.TLSClientConfig
	}
	rec := &recorder{rt: tr}
	resp, err := (&http.Client{Transport: rec}).Do(req)
	t.rec.seen = append(t.rec.seen, rec.seen...)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// rawDeleteObjectsMD5 sends DeleteObjects with Content-MD5 the way older clients do.
func (t *target) rawDeleteObjectsMD5(ctx context.Context, bucket string, keys []string) error {
	var b strings.Builder
	b.WriteString(`<Delete>`)
	for _, k := range keys {
		fmt.Fprintf(&b, `<Object><Key>%s</Key></Object>`, k)
	}
	b.WriteString(`</Delete>`)
	body := []byte(b.String())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(t.endpoint, "/")+"/"+bucket+"?delete", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(body))
	sum := md5.Sum(body) //nolint:gosec // Content-MD5
	req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
	req.Header.Set("Content-Type", "application/xml")
	if err := t.signV4(ctx, req, sha256Hex(body), t.now()); err != nil {
		return err
	}
	return t.send(req)
}

// rawGetV2 reads bucket/key with a SigV2 header or presigned URL.
func (t *target) rawGetV2(ctx context.Context, bucket, key string, presigned bool) error {
	now := t.now()
	u := t.objectURL(bucket, key)
	var req *http.Request
	var err error
	if presigned {
		exp := now.Add(5 * time.Minute).Unix()
		q := url.Values{"AWSAccessKeyId": {t.ak}, "Expires": {strconv.FormatInt(exp, 10)}}
		q.Set("Signature", t.v2Signature(http.MethodGet, "", "", strconv.FormatInt(exp, 10), nil, "/"+bucket+"/"+key))
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, u+"?"+q.Encode(), http.NoBody)
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
		if err == nil {
			t.signV2(req, bucket, key, now, true)
		}
	}
	if err != nil {
		return err
	}
	return t.send(req)
}

// signV2 adds a SigV2 Authorization header (AWS <AKID>:<base64 HMAC-SHA1>).
func (t *target) signV2(req *http.Request, bucket, key string, now time.Time, get bool) {
	req.Header.Set("Date", now.Format(http.TimeFormat))
	ct := ""
	if !get {
		ct = req.Header.Get("Content-Type")
	}
	amz := http.Header{}
	for k, v := range req.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-") {
			amz[strings.ToLower(k)] = v
		}
	}
	sig := t.v2Signature(req.Method, req.Header.Get("Content-MD5"), ct, now.Format(http.TimeFormat), amz, "/"+bucket+"/"+key)
	req.Header.Set("Authorization", "AWS "+t.ak+":"+sig)
}

// v2Signature computes the SigV2 string-to-sign and its HMAC-SHA1.
func (t *target) v2Signature(method, md5sum, contentType, dateOrExpires string, amz http.Header, resource string) string {
	var b strings.Builder
	b.WriteString(method + "\n" + md5sum + "\n" + contentType + "\n" + dateOrExpires + "\n")
	keys := make([]string, 0, len(amz))
	for k := range amz {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(k + ":" + strings.Join(amz[k], ",") + "\n")
	}
	b.WriteString(resource)
	m := hmac.New(sha1.New, []byte(t.sk)) //nolint:gosec // SigV2 is HMAC-SHA1 by definition
	m.Write([]byte(b.String()))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}
