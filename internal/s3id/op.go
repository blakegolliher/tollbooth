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
// Adapted from github.com/blakegolliher/shunt internal/s3/op.go at commit
// 9b36d09ed109add4a2bedfe1dbfae6108038ee5b (Apache-2.0, Copyright 2026 Blake Golliher).
// Modified by Blake Golliher for github.com/blakegolliher/tollbooth, 2026-11-09: added the 18
// operations the S3 API reference lists that shunt lacked, the operation Class of every Op, and
// dropped IsData (tollbooth has one deadline model for every operation).

package s3id

// Op identifies an S3 API operation. OpUnknown is still proxied and counted; it is never rejected.
type Op uint8

// Operations, named after the AWS API reference. The order here is arbitrary; the classifier
// table in classify.go decides precedence. New operations are appended so the numbering of the
// lifted ones is stable across diffs.
const (
	OpUnknown   Op = iota
	OpPreflight    // CORS preflight (OPTIONS) at any level

	// Service level.
	OpListBuckets

	// Bucket level.
	OpCreateBucket
	OpDeleteBucket
	OpHeadBucket
	OpListObjects
	OpListObjectsV2
	OpListObjectVersions
	OpListMultipartUploads
	OpDeleteObjects
	OpPostObject
	OpGetBucketLocation
	OpGetBucketAcl //nolint:revive // AWS API name
	OpPutBucketAcl //nolint:revive // AWS API name
	OpGetBucketCors
	OpPutBucketCors
	OpDeleteBucketCors
	OpGetBucketLifecycleConfiguration
	OpPutBucketLifecycleConfiguration
	OpDeleteBucketLifecycle
	OpGetBucketLogging
	OpPutBucketLogging
	OpGetBucketNotificationConfiguration
	OpPutBucketNotificationConfiguration
	OpGetObjectLockConfiguration
	OpPutObjectLockConfiguration
	OpGetBucketOwnershipControls
	OpPutBucketOwnershipControls
	OpDeleteBucketOwnershipControls
	OpGetBucketPolicy
	OpPutBucketPolicy
	OpDeleteBucketPolicy
	OpGetBucketPolicyStatus
	OpGetBucketReplication
	OpPutBucketReplication
	OpDeleteBucketReplication
	OpGetBucketRequestPayment
	OpPutBucketRequestPayment
	OpGetBucketTagging
	OpPutBucketTagging
	OpDeleteBucketTagging
	OpGetBucketVersioning
	OpPutBucketVersioning
	OpGetBucketWebsite
	OpPutBucketWebsite
	OpDeleteBucketWebsite
	OpGetBucketEncryption
	OpPutBucketEncryption
	OpDeleteBucketEncryption
	OpGetBucketAccelerateConfiguration
	OpPutBucketAccelerateConfiguration
	OpGetPublicAccessBlock
	OpPutPublicAccessBlock
	OpDeletePublicAccessBlock
	OpGetBucketAnalyticsConfiguration
	OpPutBucketAnalyticsConfiguration
	OpDeleteBucketAnalyticsConfiguration
	OpListBucketAnalyticsConfigurations
	OpGetBucketIntelligentTieringConfiguration
	OpPutBucketIntelligentTieringConfiguration
	OpDeleteBucketIntelligentTieringConfiguration
	OpListBucketIntelligentTieringConfigurations
	OpGetBucketInventoryConfiguration
	OpPutBucketInventoryConfiguration
	OpDeleteBucketInventoryConfiguration
	OpListBucketInventoryConfigurations
	OpGetBucketMetricsConfiguration
	OpPutBucketMetricsConfiguration
	OpDeleteBucketMetricsConfiguration
	OpListBucketMetricsConfigurations

	// Object level.
	OpGetObject
	OpHeadObject
	OpPutObject
	OpCopyObject
	OpDeleteObject
	OpGetObjectAcl //nolint:revive // AWS API name
	OpPutObjectAcl //nolint:revive // AWS API name
	OpGetObjectAttributes
	OpGetObjectTagging
	OpPutObjectTagging
	OpDeleteObjectTagging
	OpGetObjectLegalHold
	OpPutObjectLegalHold
	OpGetObjectRetention
	OpPutObjectRetention
	OpGetObjectTorrent
	OpRestoreObject
	OpSelectObjectContent
	OpCreateMultipartUpload
	OpUploadPart
	OpUploadPartCopy
	OpCompleteMultipartUpload
	OpAbortMultipartUpload
	OpListParts

	// Added 2026-11-08 from the S3 API reference (docs/OPERATIONS_TABLE.md).
	OpCreateSession
	OpGetBucketAbac
	OpPutBucketAbac
	OpCreateBucketMetadataTableConfiguration
	OpGetBucketMetadataTableConfiguration
	OpDeleteBucketMetadataTableConfiguration
	OpCreateBucketMetadataConfiguration
	OpGetBucketMetadataConfiguration
	OpDeleteBucketMetadataConfiguration
	OpUpdateBucketMetadataInventoryTableConfiguration
	OpUpdateBucketMetadataJournalTableConfiguration
	OpUpdateBucketMetadataAnnotationTableConfiguration
	OpRenameObject
	OpGetObjectAnnotation
	OpPutObjectAnnotation
	OpDeleteObjectAnnotation
	OpListObjectAnnotations
	OpUpdateObjectEncryption

	opCount
)

// Class groups operations the way S3 partitions its own limits, so a rule can say "WRITE" and
// showback can roll up (docs/SPEC.md, S3 operation classifier).
type Class uint8

// Classes. ClassUnknown is only OpUnknown's.
const (
	ClassUnknown Class = iota
	ClassList
	ClassRead
	ClassWrite
	ClassDelete
	ClassBucket
	numClasses
)

var classNames = [...]string{
	ClassUnknown: "UNKNOWN",
	ClassList:    "LIST",
	ClassRead:    "READ",
	ClassWrite:   "WRITE",
	ClassDelete:  "DELETE",
	ClassBucket:  "BUCKET",
}

// String returns the class name used in rule config and metric labels.
func (c Class) String() string {
	if int(c) < len(classNames) {
		return classNames[c]
	}
	return "UNKNOWN"
}

// ParseClass maps a config name to a Class.
func ParseClass(s string) (Class, bool) {
	for c, n := range classNames {
		if n == s {
			return Class(c), true
		}
	}
	return 0, false
}

// NumClasses is the number of classes, for per-class indexing.
const NumClasses = int(numClasses)

// opInfo is the name and class of one operation.
type opInfo struct {
	name  string
	class Class
}

var ops = [...]opInfo{
	OpUnknown:   {"Unknown", ClassUnknown},
	OpPreflight: {"Preflight", ClassBucket},

	OpListBuckets: {"ListBuckets", ClassList},

	OpCreateBucket:                                {"CreateBucket", ClassBucket},
	OpDeleteBucket:                                {"DeleteBucket", ClassBucket},
	OpHeadBucket:                                  {"HeadBucket", ClassBucket},
	OpListObjects:                                 {"ListObjects", ClassList},
	OpListObjectsV2:                               {"ListObjectsV2", ClassList},
	OpListObjectVersions:                          {"ListObjectVersions", ClassList},
	OpListMultipartUploads:                        {"ListMultipartUploads", ClassList},
	OpDeleteObjects:                               {"DeleteObjects", ClassDelete},
	OpPostObject:                                  {"PostObject", ClassWrite},
	OpGetBucketLocation:                           {"GetBucketLocation", ClassBucket},
	OpGetBucketAcl:                                {"GetBucketAcl", ClassBucket},
	OpPutBucketAcl:                                {"PutBucketAcl", ClassBucket},
	OpGetBucketCors:                               {"GetBucketCors", ClassBucket},
	OpPutBucketCors:                               {"PutBucketCors", ClassBucket},
	OpDeleteBucketCors:                            {"DeleteBucketCors", ClassBucket},
	OpGetBucketLifecycleConfiguration:             {"GetBucketLifecycleConfiguration", ClassBucket},
	OpPutBucketLifecycleConfiguration:             {"PutBucketLifecycleConfiguration", ClassBucket},
	OpDeleteBucketLifecycle:                       {"DeleteBucketLifecycle", ClassBucket},
	OpGetBucketLogging:                            {"GetBucketLogging", ClassBucket},
	OpPutBucketLogging:                            {"PutBucketLogging", ClassBucket},
	OpGetBucketNotificationConfiguration:          {"GetBucketNotificationConfiguration", ClassBucket},
	OpPutBucketNotificationConfiguration:          {"PutBucketNotificationConfiguration", ClassBucket},
	OpGetObjectLockConfiguration:                  {"GetObjectLockConfiguration", ClassBucket},
	OpPutObjectLockConfiguration:                  {"PutObjectLockConfiguration", ClassBucket},
	OpGetBucketOwnershipControls:                  {"GetBucketOwnershipControls", ClassBucket},
	OpPutBucketOwnershipControls:                  {"PutBucketOwnershipControls", ClassBucket},
	OpDeleteBucketOwnershipControls:               {"DeleteBucketOwnershipControls", ClassBucket},
	OpGetBucketPolicy:                             {"GetBucketPolicy", ClassBucket},
	OpPutBucketPolicy:                             {"PutBucketPolicy", ClassBucket},
	OpDeleteBucketPolicy:                          {"DeleteBucketPolicy", ClassBucket},
	OpGetBucketPolicyStatus:                       {"GetBucketPolicyStatus", ClassBucket},
	OpGetBucketReplication:                        {"GetBucketReplication", ClassBucket},
	OpPutBucketReplication:                        {"PutBucketReplication", ClassBucket},
	OpDeleteBucketReplication:                     {"DeleteBucketReplication", ClassBucket},
	OpGetBucketRequestPayment:                     {"GetBucketRequestPayment", ClassBucket},
	OpPutBucketRequestPayment:                     {"PutBucketRequestPayment", ClassBucket},
	OpGetBucketTagging:                            {"GetBucketTagging", ClassBucket},
	OpPutBucketTagging:                            {"PutBucketTagging", ClassBucket},
	OpDeleteBucketTagging:                         {"DeleteBucketTagging", ClassBucket},
	OpGetBucketVersioning:                         {"GetBucketVersioning", ClassBucket},
	OpPutBucketVersioning:                         {"PutBucketVersioning", ClassBucket},
	OpGetBucketWebsite:                            {"GetBucketWebsite", ClassBucket},
	OpPutBucketWebsite:                            {"PutBucketWebsite", ClassBucket},
	OpDeleteBucketWebsite:                         {"DeleteBucketWebsite", ClassBucket},
	OpGetBucketEncryption:                         {"GetBucketEncryption", ClassBucket},
	OpPutBucketEncryption:                         {"PutBucketEncryption", ClassBucket},
	OpDeleteBucketEncryption:                      {"DeleteBucketEncryption", ClassBucket},
	OpGetBucketAccelerateConfiguration:            {"GetBucketAccelerateConfiguration", ClassBucket},
	OpPutBucketAccelerateConfiguration:            {"PutBucketAccelerateConfiguration", ClassBucket},
	OpGetPublicAccessBlock:                        {"GetPublicAccessBlock", ClassBucket},
	OpPutPublicAccessBlock:                        {"PutPublicAccessBlock", ClassBucket},
	OpDeletePublicAccessBlock:                     {"DeletePublicAccessBlock", ClassBucket},
	OpGetBucketAnalyticsConfiguration:             {"GetBucketAnalyticsConfiguration", ClassBucket},
	OpPutBucketAnalyticsConfiguration:             {"PutBucketAnalyticsConfiguration", ClassBucket},
	OpDeleteBucketAnalyticsConfiguration:          {"DeleteBucketAnalyticsConfiguration", ClassBucket},
	OpListBucketAnalyticsConfigurations:           {"ListBucketAnalyticsConfigurations", ClassBucket},
	OpGetBucketIntelligentTieringConfiguration:    {"GetBucketIntelligentTieringConfiguration", ClassBucket},
	OpPutBucketIntelligentTieringConfiguration:    {"PutBucketIntelligentTieringConfiguration", ClassBucket},
	OpDeleteBucketIntelligentTieringConfiguration: {"DeleteBucketIntelligentTieringConfiguration", ClassBucket},
	OpListBucketIntelligentTieringConfigurations:  {"ListBucketIntelligentTieringConfigurations", ClassBucket},
	OpGetBucketInventoryConfiguration:             {"GetBucketInventoryConfiguration", ClassBucket},
	OpPutBucketInventoryConfiguration:             {"PutBucketInventoryConfiguration", ClassBucket},
	OpDeleteBucketInventoryConfiguration:          {"DeleteBucketInventoryConfiguration", ClassBucket},
	OpListBucketInventoryConfigurations:           {"ListBucketInventoryConfigurations", ClassBucket},
	OpGetBucketMetricsConfiguration:               {"GetBucketMetricsConfiguration", ClassBucket},
	OpPutBucketMetricsConfiguration:               {"PutBucketMetricsConfiguration", ClassBucket},
	OpDeleteBucketMetricsConfiguration:            {"DeleteBucketMetricsConfiguration", ClassBucket},
	OpListBucketMetricsConfigurations:             {"ListBucketMetricsConfigurations", ClassBucket},

	OpGetObject:               {"GetObject", ClassRead},
	OpHeadObject:              {"HeadObject", ClassRead},
	OpPutObject:               {"PutObject", ClassWrite},
	OpCopyObject:              {"CopyObject", ClassWrite},
	OpDeleteObject:            {"DeleteObject", ClassDelete},
	OpGetObjectAcl:            {"GetObjectAcl", ClassRead},
	OpPutObjectAcl:            {"PutObjectAcl", ClassWrite},
	OpGetObjectAttributes:     {"GetObjectAttributes", ClassRead},
	OpGetObjectTagging:        {"GetObjectTagging", ClassRead},
	OpPutObjectTagging:        {"PutObjectTagging", ClassWrite},
	OpDeleteObjectTagging:     {"DeleteObjectTagging", ClassDelete},
	OpGetObjectLegalHold:      {"GetObjectLegalHold", ClassRead},
	OpPutObjectLegalHold:      {"PutObjectLegalHold", ClassWrite},
	OpGetObjectRetention:      {"GetObjectRetention", ClassRead},
	OpPutObjectRetention:      {"PutObjectRetention", ClassWrite},
	OpGetObjectTorrent:        {"GetObjectTorrent", ClassRead},
	OpRestoreObject:           {"RestoreObject", ClassWrite},
	OpSelectObjectContent:     {"SelectObjectContent", ClassRead},
	OpCreateMultipartUpload:   {"CreateMultipartUpload", ClassWrite},
	OpUploadPart:              {"UploadPart", ClassWrite},
	OpUploadPartCopy:          {"UploadPartCopy", ClassWrite},
	OpCompleteMultipartUpload: {"CompleteMultipartUpload", ClassWrite},
	OpAbortMultipartUpload:    {"AbortMultipartUpload", ClassDelete},
	OpListParts:               {"ListParts", ClassList},

	OpCreateSession:                                    {"CreateSession", ClassBucket},
	OpGetBucketAbac:                                    {"GetBucketAbac", ClassBucket},
	OpPutBucketAbac:                                    {"PutBucketAbac", ClassBucket},
	OpCreateBucketMetadataTableConfiguration:           {"CreateBucketMetadataTableConfiguration", ClassBucket},
	OpGetBucketMetadataTableConfiguration:              {"GetBucketMetadataTableConfiguration", ClassBucket},
	OpDeleteBucketMetadataTableConfiguration:           {"DeleteBucketMetadataTableConfiguration", ClassBucket},
	OpCreateBucketMetadataConfiguration:                {"CreateBucketMetadataConfiguration", ClassBucket},
	OpGetBucketMetadataConfiguration:                   {"GetBucketMetadataConfiguration", ClassBucket},
	OpDeleteBucketMetadataConfiguration:                {"DeleteBucketMetadataConfiguration", ClassBucket},
	OpUpdateBucketMetadataInventoryTableConfiguration:  {"UpdateBucketMetadataInventoryTableConfiguration", ClassBucket},
	OpUpdateBucketMetadataJournalTableConfiguration:    {"UpdateBucketMetadataJournalTableConfiguration", ClassBucket},
	OpUpdateBucketMetadataAnnotationTableConfiguration: {"UpdateBucketMetadataAnnotationTableConfiguration", ClassBucket},
	OpRenameObject:                                     {"RenameObject", ClassWrite},
	OpGetObjectAnnotation:                              {"GetObjectAnnotation", ClassRead},
	OpPutObjectAnnotation:                              {"PutObjectAnnotation", ClassWrite},
	OpDeleteObjectAnnotation:                           {"DeleteObjectAnnotation", ClassDelete},
	OpListObjectAnnotations:                            {"ListObjectAnnotations", ClassList},
	OpUpdateObjectEncryption:                           {"UpdateObjectEncryption", ClassWrite},
}

// NumOps is the number of operations including Unknown and Preflight, for per-op indexing
// (pre-resolved metric handles, docs/DESIGN.md §9.3).
const NumOps = int(opCount)

// Ops lists every operation in enum order.
func Ops() []Op {
	out := make([]Op, 0, opCount)
	for o := Op(0); o < opCount; o++ {
		out = append(out, o)
	}
	return out
}

// String returns the AWS API name, used as the `operation` metric label and the showback column.
func (o Op) String() string {
	if o < opCount {
		return ops[o].name
	}
	return "Unknown"
}

// Class returns the operation's class.
func (o Op) Class() Class {
	if o < opCount {
		return ops[o].class
	}
	return ClassUnknown
}

// ParseOp maps an AWS API name to an Op. Used by the rule loader for `match.ops`.
func ParseOp(name string) (Op, bool) {
	for o := Op(0); o < opCount; o++ {
		if ops[o].name == name {
			return o, true
		}
	}
	return 0, false
}

// IsBucketList reports whether the operation is one of the four bucket-level listings whose
// `prefix` query parameter a prefix rule matches (ADR-0001).
func (o Op) IsBucketList() bool {
	switch o {
	case OpListObjects, OpListObjectsV2, OpListObjectVersions, OpListMultipartUploads:
		return true
	}
	return false
}
