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
// Adapted from github.com/blakegolliher/shunt internal/s3/classify_test.go at commit
// 9b36d09ed109add4a2bedfe1dbfae6108038ee5b (Apache-2.0, Copyright 2026 Blake Golliher).
// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, 2026-11-09: cases for the 18
// added operations and the ListDirectoryBuckets fold; class checks; the scanner's zero-allocation
// assertion; IsData test dropped with the method.

package s3id

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestEveryRow builds the minimal request each table row describes and asserts the row wins.
// Rows deliberately shadowed by an earlier, more specific row are still asserted: the request
// built from the row's own discriminators must not match an earlier row.
func TestEveryRow(t *testing.T) {
	for i, r := range rules {
		q := url.Values{}
		for _, k := range r.query {
			q[k] = []string{"x"}
		}
		if r.value[0] != "" {
			q.Set(r.value[0], r.value[1])
		}
		h := http.Header{}
		if r.header != "" {
			h.Set(r.header, "src/key")
		}
		name := fmt.Sprintf("%02d_%s_%s_%s", i, r.method, r.level, r.op)
		t.Run(name, func(t *testing.T) {
			if got := classifyValues(r.method, r.level, q, h); got != r.op {
				t.Fatalf("row %d (%s %s %v %v %s): got %s want %s", i, r.method, r.level, r.query, r.value, r.header, got, r.op)
			}
		})
	}
}

func TestPrecedence(t *testing.T) {
	cases := []struct {
		name   string
		method string
		level  Level
		query  string
		header map[string]string
		want   Op
	}{
		{"uploadId before uploads on object POST", "POST", LevelObject, "uploads&uploadId=1", nil, OpCreateMultipartUpload},
		{"uploadId+partNumber+copy-source is UploadPartCopy", "PUT", LevelObject, "uploadId=1&partNumber=2", map[string]string{"X-Amz-Copy-Source": "b/k"}, OpUploadPartCopy},
		{"uploadId+partNumber without copy-source is UploadPart", "PUT", LevelObject, "uploadId=1&partNumber=2", nil, OpUploadPart},
		{"copy-source alone is CopyObject", "PUT", LevelObject, "", map[string]string{"X-Amz-Copy-Source": "b/k"}, OpCopyObject},
		{"uploadId without partNumber is not UploadPart", "PUT", LevelObject, "uploadId=1", nil, OpPutObject},
		{"partNumber on GET is still GetObject", "GET", LevelObject, "partNumber=3", nil, OpGetObject},
		{"versionId on GET is still GetObject", "GET", LevelObject, "versionId=abc", nil, OpGetObject},
		{"versionId on DELETE is still DeleteObject", "DELETE", LevelObject, "versionId=abc", nil, OpDeleteObject},
		{"list-type=2 exact", "GET", LevelBucket, "list-type=2&prefix=a", nil, OpListObjectsV2},
		{"list-type=3 is v1", "GET", LevelBucket, "list-type=3", nil, OpListObjects},
		{"plain bucket GET is v1", "GET", LevelBucket, "prefix=a&delimiter=/", nil, OpListObjects},
		{"analytics with id is Get", "GET", LevelBucket, "analytics&id=x", nil, OpGetBucketAnalyticsConfiguration},
		{"analytics without id is List", "GET", LevelBucket, "analytics", nil, OpListBucketAnalyticsConfigurations},
		{"policyStatus is not policy", "GET", LevelBucket, "policyStatus", nil, OpGetBucketPolicyStatus},
		{"object-lock is bucket level", "PUT", LevelBucket, "object-lock", nil, OpPutObjectLockConfiguration},
		{"select needs select-type=2", "POST", LevelObject, "select&select-type=1", nil, OpUnknown},
		{"select with select-type=2", "POST", LevelObject, "select&select-type=2", nil, OpSelectObjectContent},
		{"delete on bucket POST", "POST", LevelBucket, "delete", nil, OpDeleteObjects},
		{"bucket POST without delete is PostObject", "POST", LevelBucket, "", nil, OpPostObject},
		{"object-level uploads is Unknown", "GET", LevelObject, "uploads", nil, OpUnknown},
		{"object-level versions is Unknown", "GET", LevelObject, "versions", nil, OpUnknown},
		{"OPTIONS is preflight", "OPTIONS", LevelObject, "", nil, OpPreflight},
		{"PATCH is unknown", "PATCH", LevelBucket, "", nil, OpUnknown},
		{"service PUT is unknown", "PUT", LevelService, "", nil, OpUnknown},
		{"tagging on object GET", "GET", LevelObject, "tagging", nil, OpGetObjectTagging},
		{"tagging on bucket DELETE", "DELETE", LevelBucket, "tagging", nil, OpDeleteBucketTagging},
		{"HEAD bucket", "HEAD", LevelBucket, "acl", nil, OpHeadBucket},
		{"HEAD object ignores query", "HEAD", LevelObject, "versionId=1&partNumber=2", nil, OpHeadObject},
		// Added operations (docs/OPERATIONS_TABLE.md, 2026-11-08).
		{"session is CreateSession, not ListObjects", "GET", LevelBucket, "session", nil, OpCreateSession},
		{"abac GET", "GET", LevelBucket, "abac", nil, OpGetBucketAbac},
		{"abac PUT", "PUT", LevelBucket, "abac", nil, OpPutBucketAbac},
		{"metadataTable POST is Create", "POST", LevelBucket, "metadataTable", nil, OpCreateBucketMetadataTableConfiguration},
		{"metadataTable GET is Get", "GET", LevelBucket, "metadataTable", nil, OpGetBucketMetadataTableConfiguration},
		{"metadataTable DELETE", "DELETE", LevelBucket, "metadataTable", nil, OpDeleteBucketMetadataTableConfiguration},
		{"metadataConfiguration POST is Create", "POST", LevelBucket, "metadataConfiguration", nil, OpCreateBucketMetadataConfiguration},
		{"metadataConfiguration GET", "GET", LevelBucket, "metadataConfiguration", nil, OpGetBucketMetadataConfiguration},
		{"metadataConfiguration DELETE", "DELETE", LevelBucket, "metadataConfiguration", nil, OpDeleteBucketMetadataConfiguration},
		{"metadataInventoryTable PUT", "PUT", LevelBucket, "metadataInventoryTable", nil, OpUpdateBucketMetadataInventoryTableConfiguration},
		{"metadataJournalTable PUT", "PUT", LevelBucket, "metadataJournalTable", nil, OpUpdateBucketMetadataJournalTableConfiguration},
		{"metadataAnnotationTable PUT", "PUT", LevelBucket, "metadataAnnotationTable", nil, OpUpdateBucketMetadataAnnotationTableConfiguration},
		{"renameObject PUT", "PUT", LevelObject, "renameObject", map[string]string{"X-Amz-Rename-Source": "b/k"}, OpRenameObject},
		{"annotation with name is Get", "GET", LevelObject, "annotation&annotationName=x", nil, OpGetObjectAnnotation},
		{"annotation without name is List", "GET", LevelObject, "annotation&annotation-prefix=x", nil, OpListObjectAnnotations},
		{"annotation PUT", "PUT", LevelObject, "annotation&annotationName=x", nil, OpPutObjectAnnotation},
		{"annotation DELETE", "DELETE", LevelObject, "annotation&annotationName=x", nil, OpDeleteObjectAnnotation},
		{"encryption on a key is UpdateObjectEncryption", "PUT", LevelObject, "encryption&versionId=v", nil, OpUpdateObjectEncryption},
		{"encryption on a bucket is GetBucketEncryption", "GET", LevelBucket, "encryption", nil, OpGetBucketEncryption},
		// Folds (D-009): deprecated aliases and ListDirectoryBuckets share a request line.
		{"GET lifecycle is the Configuration name", "GET", LevelBucket, "lifecycle", nil, OpGetBucketLifecycleConfiguration},
		{"GET notification is the Configuration name", "GET", LevelBucket, "notification", nil, OpGetBucketNotificationConfiguration},
		{"x-id=ListDirectoryBuckets folds into ListBuckets", "GET", LevelService, "x-id=ListDirectoryBuckets", nil, OpListBuckets},
		// Raw-scanner behavior: an unknown key is ignored, a repeated key keeps its first value.
		{"unknown query key is ignored", "GET", LevelObject, "frobnicate=1", nil, OpGetObject},
		{"repeated list-type keeps the first value", "GET", LevelBucket, "list-type=2&list-type=3", nil, OpListObjectsV2},
		{"empty copy-source header does not count", "PUT", LevelObject, "", map[string]string{"X-Amz-Copy-Source": ""}, OpPutObject},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range c.header {
				h.Set(k, v)
			}
			q := scanQuery(c.query)
			if got := classify(c.method, c.level, &q, h); got != c.want {
				t.Fatalf("got %s want %s", got, c.want)
			}
		})
	}
}

// The (method, level) index holds every row exactly once, in table order.
func TestIndexCoversTable(t *testing.T) {
	n := 0
	for m := range byMethodLevel {
		for l := range byMethodLevel[m] {
			last := -1
			for _, r := range byMethodLevel[m][l] {
				idx := -1
				for i := range rules {
					if &rules[i] == r {
						idx = i
					}
				}
				if idx <= last {
					t.Fatalf("index for %d/%d is out of table order at row %d", m, l, idx)
				}
				last = idx
				n++
			}
		}
	}
	if n != len(rules) {
		t.Fatalf("index holds %d rows, table has %d", n, len(rules))
	}
}

func TestOpNamesComplete(t *testing.T) {
	seenName := map[string]Op{}
	for o := Op(0); o < opCount; o++ {
		if ops[o].name == "" {
			t.Errorf("op %d has no name", o)
		}
		if o != OpUnknown && o.String() == "Unknown" {
			t.Errorf("op %d stringifies as Unknown", o)
		}
		if prev, dup := seenName[o.String()]; dup {
			t.Errorf("ops %d and %d share the name %s", prev, o, o.String())
		}
		seenName[o.String()] = o
		if o == OpUnknown && o.Class() != ClassUnknown {
			t.Errorf("Unknown must be ClassUnknown")
		}
		if o != OpUnknown && o.Class() == ClassUnknown {
			t.Errorf("%s has no class", o)
		}
		if back, ok := ParseOp(o.String()); !ok || back != o {
			t.Errorf("ParseOp(%s) = %d, %v", o, back, ok)
		}
	}
	if opCount.String() != "Unknown" || Op(255).String() != "Unknown" || Op(255).Class() != ClassUnknown {
		t.Error("out-of-range ops must stringify as Unknown with ClassUnknown")
	}
	seen := map[Op]bool{}
	for _, r := range rules {
		seen[r.op] = true
	}
	var missing []string
	for o := OpListBuckets; o < opCount; o++ {
		if !seen[o] {
			missing = append(missing, o.String())
		}
	}
	if len(missing) > 0 {
		t.Errorf("ops with no classifier row: %s", strings.Join(missing, ", "))
	}
	if n := len(Ops()); n != NumOps || NumOps != 113 {
		t.Errorf("Ops() has %d entries; the table documents 113", n)
	}
	for _, c := range []Class{ClassUnknown, ClassList, ClassRead, ClassWrite, ClassDelete, ClassBucket} {
		if back, ok := ParseClass(c.String()); !ok || back != c {
			t.Errorf("ParseClass(%s) = %d, %v", c, back, ok)
		}
	}
	if _, ok := ParseClass("READ "); ok {
		t.Error("ParseClass must be exact")
	}
	if Class(200).String() != "UNKNOWN" {
		t.Error("out-of-range class must stringify as UNKNOWN")
	}
}

func TestScanQuery(t *testing.T) {
	q := scanQuery("list-type=2&prefix=ckpt%2Fa&x-id=ListObjectsV2&X-Amz-Credential=AKIAX%2F20261109%2Fus-east-1%2Fs3%2Faws4_request&&=&junk")
	if !q.has(qListType|qPrefix|qXID|qAmzCredential) || q.has(qUploads) {
		t.Fatalf("mask %b", q.mask)
	}
	if q.listType != "2" || q.prefix != "ckpt%2Fa" || q.xid != "ListObjectsV2" || !strings.HasPrefix(q.credential, "AKIAX%2F") {
		t.Fatalf("values %+v", q)
	}
	if q := scanQuery(""); q.mask != 0 {
		t.Fatal("empty query has bits")
	}
}

// The scanner and the classifier allocate nothing: a decision is a few string compares and map
// lookups with string keys (docs/DESIGN.md §9).
func TestClassifyAllocatesNothing(t *testing.T) {
	h := http.Header{"X-Amz-Copy-Source": {"b/k"}}
	raw := "uploadId=abc&partNumber=7&x-id=UploadPartCopy&versionId=1"
	allocs := testing.AllocsPerRun(1000, func() {
		q := scanQuery(raw)
		if classify(http.MethodPut, LevelObject, &q, h) != OpUploadPartCopy {
			t.Fatal("misclassified")
		}
	})
	if allocs != 0 {
		t.Fatalf("classify allocates %.1f per request; the budget is 0", allocs)
	}
}

func BenchmarkClassify(b *testing.B) {
	h := http.Header{}
	raw := "uploadId=1&partNumber=7"
	b.ReportAllocs()
	for b.Loop() {
		q := scanQuery(raw)
		if classify(http.MethodPut, LevelObject, &q, h) != OpUploadPart {
			b.Fatal("misclassified")
		}
	}
}

func BenchmarkClassifyGetObject(b *testing.B) {
	h := http.Header{}
	b.ReportAllocs()
	for b.Loop() {
		q := scanQuery("")
		if classify(http.MethodGet, LevelObject, &q, h) != OpGetObject {
			b.Fatal("misclassified")
		}
	}
}

// FuzzClassify: the classifier never panics and is deterministic for any method, level, raw
// query, and copy-source header; a method the table does not know is Unknown; OPTIONS is always
// Preflight.
func FuzzClassify(f *testing.F) {
	f.Add("GET", uint8(2), "uploadId=1&partNumber=2", "")
	f.Add("PUT", uint8(2), "uploadId=1&partNumber=2", "b/k")
	f.Add("POST", uint8(1), "delete", "")
	f.Add("OPTIONS", uint8(0), "", "")
	f.Add("GET", uint8(1), "list-type=2&prefix=a", "")
	f.Add("PATCH", uint8(3), "frob", "")
	f.Fuzz(func(t *testing.T, method string, level uint8, raw, copySource string) {
		h := http.Header{}
		if copySource != "" {
			h.Set(hdrCopySource, copySource)
		}
		q := scanQuery(raw)
		got := classify(method, Level(level), &q, h)
		if again := classify(method, Level(level), &q, h); again != got {
			t.Fatalf("not deterministic: %s vs %s", got, again)
		}
		if method == http.MethodOptions && got != OpPreflight {
			t.Fatalf("OPTIONS classified as %s", got)
		}
		if methodIndex(method) < 0 && method != http.MethodOptions && got != OpUnknown {
			t.Fatalf("method %q classified as %s", method, got)
		}
		if level > uint8(LevelObject) && got != OpUnknown && method != http.MethodOptions {
			t.Fatalf("level %d classified as %s", level, got)
		}
		_ = got.String()
		_ = got.Class().String()
	})
}
