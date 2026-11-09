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
// Adapted from github.com/blakegolliher/shunt internal/s3/classify.go at commit
// 9b36d09ed109add4a2bedfe1dbfae6108038ee5b (Apache-2.0, Copyright 2026 Blake Golliher).
// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, 2026-11-09: rows match a
// query bitmask produced by one allocation-free scan of the raw query (scanQuery) instead of a
// url.Values map; 18 operations added (session, abac, the metadata-table family, renameObject,
// annotation, object encryption); the table order and the lifted rows are otherwise unchanged.

package s3id

import (
	"net/http"
	"net/url"
	"strings"
)

// Query keys the classifier and the identity pass know. Each is one bit of query.mask. Keys are
// case-sensitive, as S3's are, and compared as the raw bytes on the wire: a percent-encoded key
// name (`upload%49d`) is not recognized, which matches S3 itself.
const (
	qUploads uint64 = 1 << iota
	qUploadID
	qPartNumber
	qVersionID
	qTagging
	qACL
	qVersioning
	qDelete
	qAttributes
	qRestore
	qSelect
	qSelectType
	qLocation
	qListType
	qVersions
	qSession
	qAnnotation
	qAnnotationName
	qRenameObject
	qEncryption
	qMetadataTable
	qMetadataConfiguration
	qMetadataInventoryTable
	qMetadataJournalTable
	qMetadataAnnotationTable
	qAbac
	qCors
	qLifecycle
	qLogging
	qNotification
	qObjectLock
	qOwnershipControls
	qPolicy
	qPolicyStatus
	qPublicAccessBlock
	qReplication
	qRequestPayment
	qWebsite
	qAccelerate
	qAnalytics
	qIntelligentTiering
	qInventory
	qMetrics
	qID
	qLegalHold
	qRetention
	qTorrent
	qPrefix
	qXID
	qAmzCredential
	qAWSAccessKeyID
)

// queryBits maps a raw query key to its bit. A map lookup with a string key allocates nothing.
var queryBits = map[string]uint64{
	"uploads": qUploads, "uploadId": qUploadID, "partNumber": qPartNumber, "versionId": qVersionID,
	"tagging": qTagging, "acl": qACL, "versioning": qVersioning, "delete": qDelete,
	"attributes": qAttributes, "restore": qRestore, "select": qSelect, "select-type": qSelectType,
	"location": qLocation, "list-type": qListType, "versions": qVersions, "session": qSession,
	"annotation": qAnnotation, "annotationName": qAnnotationName, "renameObject": qRenameObject,
	"encryption": qEncryption, "metadataTable": qMetadataTable,
	"metadataConfiguration": qMetadataConfiguration, "metadataInventoryTable": qMetadataInventoryTable,
	"metadataJournalTable": qMetadataJournalTable, "metadataAnnotationTable": qMetadataAnnotationTable,
	"abac": qAbac, "cors": qCors, "lifecycle": qLifecycle, "logging": qLogging,
	"notification": qNotification, "object-lock": qObjectLock, "ownershipControls": qOwnershipControls,
	"policy": qPolicy, "policyStatus": qPolicyStatus, "publicAccessBlock": qPublicAccessBlock,
	"replication": qReplication, "requestPayment": qRequestPayment, "website": qWebsite,
	"accelerate": qAccelerate, "analytics": qAnalytics, "intelligent-tiering": qIntelligentTiering,
	"inventory": qInventory, "metrics": qMetrics, "id": qID, "legal-hold": qLegalHold,
	"retention": qRetention, "torrent": qTorrent, "prefix": qPrefix, "x-id": qXID,
	"X-Amz-Credential": qAmzCredential, "AWSAccessKeyId": qAWSAccessKeyID,
}

// query is what one scan of the raw query string yields: a bit per known key and the raw values
// of the few keys whose value matters. Values are substrings of the request line; nothing is
// decoded here.
type query struct {
	mask           uint64
	listType       string
	selectType     string
	prefix         string // raw (percent-encoded) list prefix; decoded once by Parse when present
	xid            string
	credential     string // raw X-Amz-Credential value
	awsAccessKeyID string
}

func (q *query) has(bits uint64) bool { return q.mask&bits == bits }

// scanQuery walks rawQuery once. Keys and values are taken as raw bytes; the first occurrence of
// a value-bearing key wins. It allocates nothing.
func scanQuery(rawQuery string) query {
	var q query
	for rawQuery != "" {
		var pair string
		if i := strings.IndexByte(rawQuery, '&'); i >= 0 {
			pair, rawQuery = rawQuery[:i], rawQuery[i+1:]
		} else {
			pair, rawQuery = rawQuery, ""
		}
		if pair == "" {
			continue
		}
		key, value := pair, ""
		if i := strings.IndexByte(pair, '='); i >= 0 {
			key, value = pair[:i], pair[i+1:]
		}
		bit, ok := queryBits[key]
		if !ok {
			continue
		}
		first := q.mask&bit == 0
		q.mask |= bit
		if !first {
			continue
		}
		switch bit {
		case qListType:
			q.listType = value
		case qSelectType:
			q.selectType = value
		case qPrefix:
			q.prefix = value
		case qXID:
			q.xid = value
		case qAmzCredential:
			q.credential = value
		case qAWSAccessKeyID:
			q.awsAccessKeyID = value
		}
	}
	return q
}

// rule is one row of the classifier table. A row matches when the method and level match, every
// key in query is present, the optional value pair matches exactly, and the optional header is
// present. Rows are walked in order; the first match wins. query (names) is kept for the tests and
// the generated docs/OPERATIONS_TABLE.md; need is the same set as a bitmask for matching.
type rule struct {
	method string
	level  Level
	query  []string
	need   uint64
	value  [2]string // key, exact value; empty key means unused; key is list-type or select-type
	header string
	op     Op
}

const hdrCopySource = "X-Amz-Copy-Source"

// subresource lists bucket-level query keys with their GET/PUT/DELETE operations. Zero means
// the verb has no operation for that key and falls through.
type subresource struct {
	key         string
	get, put, d Op
}

var bucketSubresources = []subresource{
	{"location", OpGetBucketLocation, 0, 0},
	{"tagging", OpGetBucketTagging, OpPutBucketTagging, OpDeleteBucketTagging},
	{"ownershipControls", OpGetBucketOwnershipControls, OpPutBucketOwnershipControls, OpDeleteBucketOwnershipControls},
	{"versioning", OpGetBucketVersioning, OpPutBucketVersioning, 0},
	{"policyStatus", OpGetBucketPolicyStatus, 0, 0},
	{"policy", OpGetBucketPolicy, OpPutBucketPolicy, OpDeleteBucketPolicy},
	{"cors", OpGetBucketCors, OpPutBucketCors, OpDeleteBucketCors},
	{"object-lock", OpGetObjectLockConfiguration, OpPutObjectLockConfiguration, 0},
	{"acl", OpGetBucketAcl, OpPutBucketAcl, 0},
	{"uploads", OpListMultipartUploads, 0, 0},
	{"versions", OpListObjectVersions, 0, 0},
	{"encryption", OpGetBucketEncryption, OpPutBucketEncryption, OpDeleteBucketEncryption},
	{"lifecycle", OpGetBucketLifecycleConfiguration, OpPutBucketLifecycleConfiguration, OpDeleteBucketLifecycle},
	{"logging", OpGetBucketLogging, OpPutBucketLogging, 0},
	{"requestPayment", OpGetBucketRequestPayment, OpPutBucketRequestPayment, 0},
	{"replication", OpGetBucketReplication, OpPutBucketReplication, OpDeleteBucketReplication},
	{"publicAccessBlock", OpGetPublicAccessBlock, OpPutPublicAccessBlock, OpDeletePublicAccessBlock},
	{"notification", OpGetBucketNotificationConfiguration, OpPutBucketNotificationConfiguration, 0},
	{"accelerate", OpGetBucketAccelerateConfiguration, OpPutBucketAccelerateConfiguration, 0},
	{"website", OpGetBucketWebsite, OpPutBucketWebsite, OpDeleteBucketWebsite},
	// Added 2026-11-08 (docs/OPERATIONS_TABLE.md).
	{"session", OpCreateSession, 0, 0},
	{"abac", OpGetBucketAbac, OpPutBucketAbac, 0},
	{"metadataTable", OpGetBucketMetadataTableConfiguration, 0, OpDeleteBucketMetadataTableConfiguration},
	{"metadataConfiguration", OpGetBucketMetadataConfiguration, 0, OpDeleteBucketMetadataConfiguration},
	{"metadataInventoryTable", 0, OpUpdateBucketMetadataInventoryTableConfiguration, 0},
	{"metadataJournalTable", 0, OpUpdateBucketMetadataJournalTableConfiguration, 0},
	{"metadataAnnotationTable", 0, OpUpdateBucketMetadataAnnotationTableConfiguration, 0},
}

// configured lists the bucket-level "configuration" families: GET with ?id is Get…, GET without
// is List…; PUT and DELETE carry ?id.
type configured struct {
	key               string
	get, list, put, d Op
}

var bucketConfigured = []configured{
	{"analytics", OpGetBucketAnalyticsConfiguration, OpListBucketAnalyticsConfigurations, OpPutBucketAnalyticsConfiguration, OpDeleteBucketAnalyticsConfiguration},
	{"intelligent-tiering", OpGetBucketIntelligentTieringConfiguration, OpListBucketIntelligentTieringConfigurations, OpPutBucketIntelligentTieringConfiguration, OpDeleteBucketIntelligentTieringConfiguration},
	{"inventory", OpGetBucketInventoryConfiguration, OpListBucketInventoryConfigurations, OpPutBucketInventoryConfiguration, OpDeleteBucketInventoryConfiguration},
	{"metrics", OpGetBucketMetricsConfiguration, OpListBucketMetricsConfigurations, OpPutBucketMetricsConfiguration, OpDeleteBucketMetricsConfiguration},
}

// rules is the table. Built once; order is precedence. Comments name the trap each order avoids.
var rules = buildRules()

// byMethodLevel indexes rules by (method, level) so a decision walks only the rows that can
// match (about ten) instead of the whole table. Order within a bucket is table order.
var byMethodLevel = indexRules(rules)

// methodIndex maps the five methods the table knows to a slot; anything else has no rows.
func methodIndex(method string) int {
	switch method {
	case http.MethodGet:
		return 0
	case http.MethodHead:
		return 1
	case http.MethodPut:
		return 2
	case http.MethodPost:
		return 3
	case http.MethodDelete:
		return 4
	}
	return -1
}

func indexRules(t []rule) [5][3][]*rule {
	var out [5][3][]*rule
	for i := range t {
		m := methodIndex(t[i].method)
		if m < 0 {
			panic("classifier row with an unindexed method: " + t[i].method)
		}
		out[m][t[i].level] = append(out[m][t[i].level], &t[i])
	}
	return out
}

func buildRules() []rule {
	var t []rule
	add := func(r rule) {
		for _, k := range r.query {
			bit, ok := queryBits[k]
			if !ok {
				panic("classifier row names an unknown query key: " + k)
			}
			r.need |= bit
		}
		if r.value[0] != "" {
			if _, ok := queryBits[r.value[0]]; !ok {
				panic("classifier row names an unknown value key: " + r.value[0])
			}
			r.need |= queryBits[r.value[0]]
		}
		t = append(t, r)
	}

	// Service.
	add(rule{method: http.MethodGet, level: LevelService, op: OpListBuckets})

	// Bucket GET: configuration families first (?key&id before ?key), then subresources,
	// then list-type=2 by exact value, then ListObjects v1 as the fallback.
	for _, c := range bucketConfigured {
		add(rule{method: http.MethodGet, level: LevelBucket, query: []string{c.key, "id"}, op: c.get})
		add(rule{method: http.MethodGet, level: LevelBucket, query: []string{c.key}, op: c.list})
		add(rule{method: http.MethodPut, level: LevelBucket, query: []string{c.key}, op: c.put})
		add(rule{method: http.MethodDelete, level: LevelBucket, query: []string{c.key}, op: c.d})
	}
	for _, s := range bucketSubresources {
		if s.get != 0 {
			add(rule{method: http.MethodGet, level: LevelBucket, query: []string{s.key}, op: s.get})
		}
		if s.put != 0 {
			add(rule{method: http.MethodPut, level: LevelBucket, query: []string{s.key}, op: s.put})
		}
		if s.d != 0 {
			add(rule{method: http.MethodDelete, level: LevelBucket, query: []string{s.key}, op: s.d})
		}
	}
	add(rule{method: http.MethodGet, level: LevelBucket, value: [2]string{"list-type", "2"}, op: OpListObjectsV2})
	add(rule{method: http.MethodGet, level: LevelBucket, op: OpListObjects})
	add(rule{method: http.MethodHead, level: LevelBucket, op: OpHeadBucket})
	add(rule{method: http.MethodPut, level: LevelBucket, op: OpCreateBucket})
	add(rule{method: http.MethodDelete, level: LevelBucket, op: OpDeleteBucket})
	// Bucket POST: the metadata creates and ?delete before the bare form-upload fallback.
	add(rule{method: http.MethodPost, level: LevelBucket, query: []string{"metadataTable"}, op: OpCreateBucketMetadataTableConfiguration})
	add(rule{method: http.MethodPost, level: LevelBucket, query: []string{"metadataConfiguration"}, op: OpCreateBucketMetadataConfiguration})
	add(rule{method: http.MethodPost, level: LevelBucket, query: []string{"delete"}, op: OpDeleteObjects})
	add(rule{method: http.MethodPost, level: LevelBucket, op: OpPostObject})

	// Object GET.
	add(rule{method: http.MethodGet, level: LevelObject, query: []string{"acl"}, op: OpGetObjectAcl})
	add(rule{method: http.MethodGet, level: LevelObject, query: []string{"attributes"}, op: OpGetObjectAttributes})
	add(rule{method: http.MethodGet, level: LevelObject, query: []string{"legal-hold"}, op: OpGetObjectLegalHold})
	add(rule{method: http.MethodGet, level: LevelObject, query: []string{"retention"}, op: OpGetObjectRetention})
	add(rule{method: http.MethodGet, level: LevelObject, query: []string{"tagging"}, op: OpGetObjectTagging})
	add(rule{method: http.MethodGet, level: LevelObject, query: []string{"uploadId"}, op: OpListParts})
	add(rule{method: http.MethodGet, level: LevelObject, query: []string{"torrent"}, op: OpGetObjectTorrent})
	// annotation with a name is Get, without one List (added 2026-11-08).
	add(rule{method: http.MethodGet, level: LevelObject, query: []string{"annotation", "annotationName"}, op: OpGetObjectAnnotation})
	add(rule{method: http.MethodGet, level: LevelObject, query: []string{"annotation"}, op: OpListObjectAnnotations})
	// Object-level ?uploads / ?versions are bucket listings misaddressed; leave them Unknown.
	add(rule{method: http.MethodGet, level: LevelObject, query: []string{"uploads"}, op: OpUnknown})
	add(rule{method: http.MethodGet, level: LevelObject, query: []string{"versions"}, op: OpUnknown})
	add(rule{method: http.MethodGet, level: LevelObject, op: OpGetObject}) // partNumber, versionId, Range are not discriminators

	// Object HEAD / DELETE.
	add(rule{method: http.MethodHead, level: LevelObject, op: OpHeadObject})
	add(rule{method: http.MethodDelete, level: LevelObject, query: []string{"uploadId"}, op: OpAbortMultipartUpload})
	add(rule{method: http.MethodDelete, level: LevelObject, query: []string{"tagging"}, op: OpDeleteObjectTagging})
	add(rule{method: http.MethodDelete, level: LevelObject, query: []string{"annotation"}, op: OpDeleteObjectAnnotation})
	add(rule{method: http.MethodDelete, level: LevelObject, op: OpDeleteObject})

	// Object POST.
	add(rule{method: http.MethodPost, level: LevelObject, query: []string{"uploads"}, op: OpCreateMultipartUpload})
	add(rule{method: http.MethodPost, level: LevelObject, query: []string{"uploadId"}, op: OpCompleteMultipartUpload})
	add(rule{method: http.MethodPost, level: LevelObject, query: []string{"restore"}, op: OpRestoreObject})
	add(rule{method: http.MethodPost, level: LevelObject, query: []string{"select"}, value: [2]string{"select-type", "2"}, op: OpSelectObjectContent})

	// Object PUT: subresources, then uploadId+partNumber (with and without copy-source) BEFORE the
	// bare copy-source test, otherwise UploadPartCopy collapses into CopyObject.
	add(rule{method: http.MethodPut, level: LevelObject, query: []string{"tagging"}, op: OpPutObjectTagging})
	add(rule{method: http.MethodPut, level: LevelObject, query: []string{"retention"}, op: OpPutObjectRetention})
	add(rule{method: http.MethodPut, level: LevelObject, query: []string{"legal-hold"}, op: OpPutObjectLegalHold})
	add(rule{method: http.MethodPut, level: LevelObject, query: []string{"acl"}, op: OpPutObjectAcl})
	add(rule{method: http.MethodPut, level: LevelObject, query: []string{"renameObject"}, op: OpRenameObject})
	add(rule{method: http.MethodPut, level: LevelObject, query: []string{"annotation"}, op: OpPutObjectAnnotation})
	add(rule{method: http.MethodPut, level: LevelObject, query: []string{"encryption"}, op: OpUpdateObjectEncryption})
	add(rule{method: http.MethodPut, level: LevelObject, query: []string{"uploadId", "partNumber"}, header: hdrCopySource, op: OpUploadPartCopy})
	add(rule{method: http.MethodPut, level: LevelObject, query: []string{"uploadId", "partNumber"}, op: OpUploadPart})
	add(rule{method: http.MethodPut, level: LevelObject, header: hdrCopySource, op: OpCopyObject})
	add(rule{method: http.MethodPut, level: LevelObject, op: OpPutObject})

	return t
}

// classify maps a request to an operation. OPTIONS at any level is a CORS preflight; anything
// that matches no row is OpUnknown and is still proxied.
func classify(method string, level Level, q *query, h http.Header) Op {
	if method == http.MethodOptions {
		return OpPreflight
	}
	m := methodIndex(method)
	if m < 0 || level > LevelObject {
		return OpUnknown
	}
	for _, r := range byMethodLevel[m][level] {
		if matches(r, q, h) {
			return r.op
		}
	}
	return OpUnknown
}

func matches(r *rule, q *query, h http.Header) bool {
	if !q.has(r.need) {
		return false
	}
	if r.value[0] != "" {
		var got string
		switch r.value[0] {
		case "list-type":
			got = q.listType
		case "select-type":
			got = q.selectType
		}
		if got != r.value[1] {
			return false
		}
	}
	if r.header != "" {
		v := h[r.header] // canonical key; a direct index allocates nothing
		if len(v) == 0 || v[0] == "" {
			return false
		}
	}
	return true
}

// classifyValues is the url.Values form used by tests and the table generator: it runs the raw
// scanner over the encoded query so the two paths cannot drift.
func classifyValues(method string, level Level, v url.Values, h http.Header) Op {
	q := scanQuery(v.Encode())
	return classify(method, level, &q, h)
}
