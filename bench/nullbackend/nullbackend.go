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

// Package nullbackend is the in-repo S3 stand-in for tests and benchmarks (docs/SPEC.md,
// Performance targets): it answers every operation with a canned response of configurable size
// and injected latency, records exactly what it received when asked (the recording backend for
// the byte-equivalence tests), and can be told to fail, stall, or reset. It is a test tool; it is
// never linked into bin/tollbooth.
package nullbackend

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blakegolliher/tollbooth/internal/s3id"
)

// Request is what the backend saw of one request, as net/http parsed it. Header order is lost
// on both the direct and the proxied path alike, so a diff over these is a diff over what
// matters to a signature: method, target, Host, header values, body bytes, trailers.
type Request struct {
	Method     string
	RequestURI string
	Host       string
	Header     http.Header
	Body       []byte
	Trailer    http.Header
	Remote     string
	Op         s3id.Op
}

// Server is the backend. Fields are read on every request; the atomics may be flipped while
// serving to inject faults.
type Server struct {
	// BodySize is the size of a GetObject body.
	BodySize int
	// Latency is added before every response.
	Latency time.Duration
	// Record keeps every request (with its full body) for Requests().
	Record bool
	// Fail, when non-zero, answers every request with that status and an S3 error body.
	Fail atomic.Int32
	// Stall holds every request open until the client gives up.
	Stall atomic.Bool
	// Reset hijacks the connection and closes it without a response.
	Reset atomic.Bool
	// ResetAfter, when > 0, writes headers and this many body bytes, then closes the connection.
	ResetAfter atomic.Int64

	served atomic.Int64
	body   []byte
	mu     sync.Mutex
	reqs   []Request
	ident  s3id.Config
}

// New returns a backend whose GetObject bodies are bodySize bytes of a fixed pattern.
func New(bodySize int) *Server {
	b := make([]byte, bodySize)
	for i := range b {
		b[i] = "0123456789abcdef"[i%16]
	}
	return &Server{BodySize: bodySize, body: b, ident: s3id.Config{Domains: s3id.NewDomains([]string{"null.example.com"})}}
}

// Served returns the number of requests completed.
func (s *Server) Served() int64 { return s.served.Load() }

// Requests returns a copy of the recorded requests, in arrival order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

// Clear drops the recorded requests.
func (s *Server) Clear() {
	s.mu.Lock()
	s.reqs = nil
	s.mu.Unlock()
}

const xmlHeader = `<?xml version="1.0" encoding="UTF-8"?>` + "\n"

// ServeHTTP answers with enough S3 for the proxy to be the only thing under test.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer s.served.Add(1)
	id := s3id.Parse(r, &s.ident)
	body, _ := io.ReadAll(r.Body) // the backend always drains what the client sent
	if s.Record {
		s.mu.Lock()
		s.reqs = append(s.reqs, Request{Method: r.Method, RequestURI: r.RequestURI, Host: r.Host, Header: r.Header.Clone(), Body: body, Trailer: r.Trailer.Clone(), Remote: r.RemoteAddr, Op: id.Op})
		s.mu.Unlock()
	}
	if s.Latency > 0 {
		time.Sleep(s.Latency)
	}
	if s.Stall.Load() {
		<-r.Context().Done()
		return
	}
	if s.Reset.Load() {
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close() // the point is to drop it
		}
		return
	}
	h := w.Header()
	h.Set("x-amz-request-id", "NULLBACKEND000001")
	h.Set("x-amz-id-2", "null/backend")
	if st := int(s.Fail.Load()); st != 0 {
		h.Set("Content-Type", "application/xml")
		w.WriteHeader(st)
		_, _ = fmt.Fprintf(w, xmlHeader+`<Error><Code>InternalError</Code><Message>injected</Message><RequestId>NULLBACKEND000001</RequestId></Error>`)
		return
	}
	if n := s.ResetAfter.Load(); n > 0 {
		h.Set("Content-Length", strconv.Itoa(s.BodySize))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(s.body[:min(int(n), len(s.body))])
		_ = http.NewResponseController(w).Flush()
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close() // injected reset
		}
		return
	}
	switch id.Op {
	case s3id.OpGetObject:
		h.Set("ETag", `"null"`)
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Length", strconv.Itoa(len(s.body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(s.body)
	case s3id.OpHeadObject:
		h.Set("ETag", `"null"`)
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Length", strconv.Itoa(len(s.body)))
		w.WriteHeader(http.StatusOK)
	case s3id.OpPutObject, s3id.OpUploadPart, s3id.OpPutObjectTagging, s3id.OpPutObjectAcl:
		h.Set("ETag", `"null"`)
		w.WriteHeader(http.StatusOK)
	case s3id.OpCopyObject, s3id.OpUploadPartCopy:
		h.Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, xmlHeader+`<CopyObjectResult><ETag>"null"</ETag><LastModified>2026-11-09T00:00:00.000Z</LastModified></CopyObjectResult>`)
	case s3id.OpDeleteObject, s3id.OpAbortMultipartUpload, s3id.OpDeleteObjectTagging:
		w.WriteHeader(http.StatusNoContent)
	case s3id.OpCreateMultipartUpload:
		h.Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, xmlHeader+`<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>null-upload</UploadId></InitiateMultipartUploadResult>`, id.Bucket, id.Key)
	case s3id.OpCompleteMultipartUpload:
		h.Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, xmlHeader+`<CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><ETag>"null-2"</ETag></CompleteMultipartUploadResult>`, id.Bucket, id.Key)
	case s3id.OpListBuckets:
		h.Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, xmlHeader+`<ListAllMyBucketsResult><Buckets><Bucket><Name>b</Name><CreationDate>2026-11-09T00:00:00.000Z</CreationDate></Bucket></Buckets></ListAllMyBucketsResult>`)
	case s3id.OpListObjects, s3id.OpListObjectsV2, s3id.OpListObjectVersions, s3id.OpListMultipartUploads, s3id.OpListParts:
		h.Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, xmlHeader+`<ListBucketResult><Name>%s</Name><Prefix>%s</Prefix><KeyCount>0</KeyCount><IsTruncated>false</IsTruncated></ListBucketResult>`, id.Bucket, id.ListPrefix)
	case s3id.OpDeleteObjects:
		h.Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, xmlHeader+`<DeleteResult></DeleteResult>`)
	case s3id.OpHeadBucket, s3id.OpCreateBucket:
		w.WriteHeader(http.StatusOK)
	case s3id.OpDeleteBucket:
		w.WriteHeader(http.StatusNoContent)
	case s3id.OpPreflight:
		h.Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusOK)
	}
}
