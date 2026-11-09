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
// Adapted from github.com/blakegolliher/shunt internal/s3/errors.go at commit
// 9b36d09ed109add4a2bedfe1dbfae6108038ee5b (Apache-2.0, Copyright 2026 Blake Golliher).
// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, 2026-11-09: the table is
// reduced to the four codes tollbooth itself originates (it never answers for the backend);
// TooManyRequests added; Retry-After and a generated x-amz-request-id on every response; the body
// is built in a pooled buffer with one Write; headers are stored under canonical keys with shared
// values; x-amz-retry-after carries the exact wait in milliseconds.

package proxy

import (
	"encoding/xml"
	// Request ids are opaque correlation ids, not secrets (D-041); the crypto-randomness rule
	// does not apply.
	"math/rand/v2" // nosemgrep
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Code is an S3 error code as it appears in the <Code> element of an error response.
type Code string

// The codes tollbooth originates. Everything else on the wire comes from the backend untouched.
// Wire values (code, HTTP status, message) are AWS's, from the S3 API "Error responses" reference.
const (
	CodeSlowDown           Code = "SlowDown"
	CodeTooManyRequests    Code = "TooManyRequests"
	CodeServiceUnavailable Code = "ServiceUnavailable"
	CodeInvalidURI         Code = "InvalidURI"
)

// s3Error is one row: the wire code, its HTTP status, and AWS's message.
type s3Error struct {
	status  int
	message string
}

var errorTable = map[Code]s3Error{
	CodeSlowDown:           {http.StatusServiceUnavailable, "Please reduce your request rate."},
	CodeTooManyRequests:    {http.StatusTooManyRequests, "Please reduce your request rate."},
	CodeServiceUnavailable: {http.StatusServiceUnavailable, "Service is unable to handle request."},
	CodeInvalidURI:         {http.StatusBadRequest, "Couldn't parse the specified URI."},
}

// errorBodyPool holds scratch buffers for error bodies; a throttle storm must not allocate one
// per denial.
var errorBodyPool = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}

// newRequestID returns 128 random bits as 32 hex characters, generated only on the paths where
// tollbooth answers for itself. It is a correlation id, not a secret, so math/rand/v2's
// goroutine-safe generator serves: no interface call, no syscall, and exactly one allocation (the
// string) in every build mode, which the denied-path allocation test pins.
func newRequestID() string {
	var out [32]byte
	hi, lo := rand.Uint64(), rand.Uint64() //nolint:gosec // an opaque correlation id, not a credential
	for i := 15; i >= 0; i-- {
		out[i], out[16+i] = hexDigits[hi&15], hexDigits[lo&15]
		hi, lo = hi>>4, lo>>4
	}
	return string(out[:])
}

const hexDigits = "0123456789abcdef"

// Shared header values and canonical keys for the error response, so writing it costs no
// canonicalization and no allocation beyond the values that vary.
var (
	xmlContentType  = []string{"application/xml"}
	hdrContentType  = "Content-Type"
	hdrLength       = "Content-Length"
	hdrRequestID    = "X-Amz-Request-Id" // the canonical form of x-amz-request-id, as net/http would store it
	hdrRetryAfter   = "Retry-After"
	hdrRetryAfterMS = "X-Amz-Retry-After" // milliseconds; the AWS SDKs' exact-wait header (D-043)
	retryAfterCache = [...][]string{{"1"}, {"2"}, {"3"}, {"4"}, {"5"}, {"6"}, {"7"}, {"8"}, {"9"}, {"10"}}
)

// writeS3Error answers the client with an S3 error body for code. resource is the raw path, which
// S3 itself echoes in <Resource>. A positive wait adds Retry-After (whole seconds, rounded up, at
// least 1, which every SDK can read) and x-amz-retry-after (milliseconds, rounded up, which
// aws-sdk-go-v2 and the clients built on it honor exactly; D-043). Rule identifiers never appear
// here. Returns the request id written.
func writeS3Error(w http.ResponseWriter, code Code, resource string, wait time.Duration) string {
	e := errorTable[code]
	rid := newRequestID()
	bp := errorBodyPool.Get().(*[]byte) //nolint:errcheck // the pool holds only *[]byte
	b := (*bp)[:0]
	b = append(b, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+`<Error><Code>`...)
	b = append(b, code...)
	b = append(b, `</Code><Message>`...)
	b = append(b, e.message...)
	b = append(b, `</Message><Resource>`...)
	b = appendXMLEscaped(b, resource)
	b = append(b, `</Resource><RequestId>`...)
	b = append(b, rid...)
	b = append(b, `</RequestId><HostId>tollbooth</HostId></Error>`...)
	h := w.Header()
	h[hdrContentType] = xmlContentType
	h[hdrLength] = []string{strconv.Itoa(len(b))}
	h[hdrRequestID] = []string{rid}
	if wait > 0 {
		secs := int((wait + time.Second - 1) / time.Second)
		if secs <= len(retryAfterCache) {
			h[hdrRetryAfter] = retryAfterCache[secs-1]
		} else {
			h[hdrRetryAfter] = []string{strconv.Itoa(secs)}
		}
		ms := int64((wait + time.Millisecond - 1) / time.Millisecond)
		h[hdrRetryAfterMS] = []string{strconv.FormatInt(ms, 10)}
	}
	w.WriteHeader(e.status)
	_, _ = w.Write(b)
	*bp = b[:0]
	errorBodyPool.Put(bp)
	return rid
}

// appendXMLEscaped appends s with the five XML special characters escaped, the way
// encoding/xml would, without the encoder.
func appendXMLEscaped(b []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '<':
			b = append(b, "&lt;"...)
		case '>':
			b = append(b, "&gt;"...)
		case '&':
			b = append(b, "&amp;"...)
		case '"':
			b = append(b, "&#34;"...)
		case '\'':
			b = append(b, "&#39;"...)
		default:
			b = append(b, c)
		}
	}
	return b
}

// ErrorResponse is the S3 error XML body, for tests that parse what the proxy sent.
type ErrorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      Code     `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource"`
	RequestID string   `xml:"RequestId"`
	HostID    string   `xml:"HostId"`
}
