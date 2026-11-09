# S3 operations table

Reference: [Amazon S3 API operations](https://docs.aws.amazon.com/AmazonS3/latest/API/API_Operations_Amazon_Simple_Storage_Service.html), fetched **2026-11-08**, listing **116** operations for the Amazon S3 service (the page carries no version date; the fetch date is the version). S3 Control, Tables, Vectors, Outposts, and Files operations are out of scope.

Source table: shunt `internal/s3/op.go` and `classify.go` at commit `9b36d09` (95 entries: 93 named operations, `Preflight`, `Unknown`). This file is the Phase 0 check of that table against the reference. **In Phase 1 the Go table in `internal/s3id` becomes the single source of truth and a test regenerates this file from it, failing when the two drift** (docs/SPEC.md, S3 operation classifier). Until then it is hand-written.

## Result of the check

The arithmetic, computed by a script over the fetched list and shunt's enum (not by hand):

| Set | Count | Derivation |
| --- | --- | --- |
| AWS operations on the reference page | **116** | fetched 2026-11-08 |
| shunt table entries at `9b36d09` | **95** | 92 AWS names + 3 entries with no AWS counterpart (`Unknown`, `Preflight`, `PostObject`) |
| AWS names kept from shunt | 92 | in both lists |
| AWS names **added** | 18 | in the reference, not in shunt |
| AWS names **folded** into a kept entry | 5 | identical request line, or distinguishable only by `x-id` |
| AWS names **excluded** | 1 | unreachable through an S3-compatible backend |
| check: 92 + 18 + 5 + 1 | **116** | every AWS name accounted for |
| tollbooth table entries | **113** | 92 kept + 18 added + 3 non-AWS (`Unknown`, `Preflight`, `PostObject`) |

The earlier version of this file said "95 → 113, 18 added" without separating the three non-AWS
entries, which made the sums look wrong (Gate 0 review). They were never part of the 116.

### Entries with no AWS counterpart

| Entry | Why it exists |
| --- | --- |
| `Unknown` | Sentinel: nothing matched; passed through and counted. |
| `Preflight` | CORS `OPTIONS` at any level; not an API action but real traffic. Class `BUCKET`, anonymous. |
| `PostObject` | Browser-form upload (`POST /` on a bucket with `multipart/form-data`), documented by AWS at [POST Object](https://docs.aws.amazon.com/AmazonS3/latest/API/RESTObjectPOST.html) outside the operations list. |

### Reconciliation: every AWS name

| AWS operation (link) | Disposition | Note |
| --- | --- | --- |
| [AbortMultipartUpload](https://docs.aws.amazon.com/AmazonS3/latest/API/API_AbortMultipartUpload.html) | table entry (from shunt) | — |
| [CompleteMultipartUpload](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CompleteMultipartUpload.html) | table entry (from shunt) | — |
| [CopyObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CopyObject.html) | table entry (from shunt) | — |
| [CreateBucket](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateBucket.html) | table entry (from shunt) | — |
| [CreateBucketMetadataConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateBucketMetadataConfiguration.html) | table entry (**added 2026-11-08**) | request syntax read from the linked page |
| [CreateBucketMetadataTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateBucketMetadataTableConfiguration.html) | table entry (**added 2026-11-08**) | request syntax read from the linked page |
| [CreateMultipartUpload](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateMultipartUpload.html) | table entry (from shunt) | — |
| [CreateSession](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateSession.html) | table entry (**added 2026-11-08**) | request syntax read from the linked page |
| [DeleteBucket](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucket.html) | table entry (from shunt) | — |
| [DeleteBucketAnalyticsConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketAnalyticsConfiguration.html) | table entry (from shunt) | — |
| [DeleteBucketCors](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketCors.html) | table entry (from shunt) | — |
| [DeleteBucketEncryption](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketEncryption.html) | table entry (from shunt) | — |
| [DeleteBucketIntelligentTieringConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketIntelligentTieringConfiguration.html) | table entry (from shunt) | — |
| [DeleteBucketInventoryConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketInventoryConfiguration.html) | table entry (from shunt) | — |
| [DeleteBucketLifecycle](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketLifecycle.html) | table entry (from shunt) | — |
| [DeleteBucketMetadataConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketMetadataConfiguration.html) | table entry (**added 2026-11-08**) | request syntax read from the linked page |
| [DeleteBucketMetadataTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketMetadataTableConfiguration.html) | table entry (**added 2026-11-08**) | request syntax read from the linked page |
| [DeleteBucketMetricsConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketMetricsConfiguration.html) | table entry (from shunt) | — |
| [DeleteBucketOwnershipControls](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketOwnershipControls.html) | table entry (from shunt) | — |
| [DeleteBucketPolicy](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketPolicy.html) | table entry (from shunt) | — |
| [DeleteBucketReplication](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketReplication.html) | table entry (from shunt) | — |
| [DeleteBucketTagging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketTagging.html) | table entry (from shunt) | — |
| [DeleteBucketWebsite](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketWebsite.html) | table entry (from shunt) | — |
| [DeleteObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObject.html) | table entry (from shunt) | — |
| [DeleteObjectAnnotation](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObjectAnnotation.html) | table entry (**added 2026-11-08**) | request syntax read from the linked page |
| [DeleteObjects](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObjects.html) | table entry (from shunt) | — |
| [DeleteObjectTagging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObjectTagging.html) | table entry (from shunt) | — |
| [DeletePublicAccessBlock](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeletePublicAccessBlock.html) | table entry (from shunt) | — |
| [GetBucketAbac](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketAbac.html) | table entry (**added 2026-11-08**) | request syntax read from the linked page |
| [GetBucketAccelerateConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketAccelerateConfiguration.html) | table entry (from shunt) | — |
| [GetBucketAcl](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketAcl.html) | table entry (from shunt) | — |
| [GetBucketAnalyticsConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketAnalyticsConfiguration.html) | table entry (from shunt) | — |
| [GetBucketCors](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketCors.html) | table entry (from shunt) | — |
| [GetBucketEncryption](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketEncryption.html) | table entry (from shunt) | — |
| [GetBucketIntelligentTieringConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketIntelligentTieringConfiguration.html) | table entry (from shunt) | — |
| [GetBucketInventoryConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketInventoryConfiguration.html) | table entry (from shunt) | — |
| [GetBucketLifecycle](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketLifecycle.html) | **folded** into `GetBucketLifecycleConfiguration` | identical request line |
| [GetBucketLifecycleConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketLifecycleConfiguration.html) | table entry (from shunt) | — |
| [GetBucketLocation](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketLocation.html) | table entry (from shunt) | — |
| [GetBucketLogging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketLogging.html) | table entry (from shunt) | — |
| [GetBucketMetadataConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketMetadataConfiguration.html) | table entry (**added 2026-11-08**) | request syntax read from the linked page |
| [GetBucketMetadataTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketMetadataTableConfiguration.html) | table entry (**added 2026-11-08**) | request syntax read from the linked page |
| [GetBucketMetricsConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketMetricsConfiguration.html) | table entry (from shunt) | — |
| [GetBucketNotification](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketNotification.html) | **folded** into `GetBucketNotificationConfiguration` | identical request line |
| [GetBucketNotificationConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketNotificationConfiguration.html) | table entry (from shunt) | — |
| [GetBucketOwnershipControls](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketOwnershipControls.html) | table entry (from shunt) | — |
| [GetBucketPolicy](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketPolicy.html) | table entry (from shunt) | — |
| [GetBucketPolicyStatus](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketPolicyStatus.html) | table entry (from shunt) | — |
| [GetBucketReplication](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketReplication.html) | table entry (from shunt) | — |
| [GetBucketRequestPayment](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketRequestPayment.html) | table entry (from shunt) | — |
| [GetBucketTagging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketTagging.html) | table entry (from shunt) | — |
| [GetBucketVersioning](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketVersioning.html) | table entry (from shunt) | — |
| [GetBucketWebsite](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketWebsite.html) | table entry (from shunt) | — |
| [GetObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObject.html) | table entry (from shunt) | — |
| [GetObjectAcl](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectAcl.html) | table entry (from shunt) | — |
| [GetObjectAnnotation](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectAnnotation.html) | table entry (**added 2026-11-08**) | request syntax read from the linked page |
| [GetObjectAttributes](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectAttributes.html) | table entry (from shunt) | — |
| [GetObjectLegalHold](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectLegalHold.html) | table entry (from shunt) | — |
| [GetObjectLockConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectLockConfiguration.html) | table entry (from shunt) | — |
| [GetObjectRetention](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectRetention.html) | table entry (from shunt) | — |
| [GetObjectTagging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectTagging.html) | table entry (from shunt) | — |
| [GetObjectTorrent](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectTorrent.html) | table entry (from shunt) | — |
| [GetPublicAccessBlock](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetPublicAccessBlock.html) | table entry (from shunt) | — |
| [HeadBucket](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadBucket.html) | table entry (from shunt) | — |
| [HeadObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_HeadObject.html) | table entry (from shunt) | — |
| [ListBucketAnalyticsConfigurations](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListBucketAnalyticsConfigurations.html) | table entry (from shunt) | — |
| [ListBucketIntelligentTieringConfigurations](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListBucketIntelligentTieringConfigurations.html) | table entry (from shunt) | — |
| [ListBucketInventoryConfigurations](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListBucketInventoryConfigurations.html) | table entry (from shunt) | — |
| [ListBucketMetricsConfigurations](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListBucketMetricsConfigurations.html) | table entry (from shunt) | — |
| [ListBuckets](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListBuckets.html) | table entry (from shunt) | — |
| [ListDirectoryBuckets](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListDirectoryBuckets.html) | **folded** into `ListBuckets` | identical request line |
| [ListMultipartUploads](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListMultipartUploads.html) | table entry (from shunt) | — |
| [ListObjectAnnotations](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectAnnotations.html) | table entry (**added 2026-11-08**) | request syntax read from the linked page |
| [ListObjects](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjects.html) | table entry (from shunt) | — |
| [ListObjectsV2](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectsV2.html) | table entry (from shunt) | — |
| [ListObjectVersions](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectVersions.html) | table entry (from shunt) | — |
| [ListParts](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListParts.html) | table entry (from shunt) | — |
| [PutBucketAbac](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketAbac.html) | table entry (**added 2026-11-08**) | request syntax read from the linked page |
| [PutBucketAccelerateConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketAccelerateConfiguration.html) | table entry (from shunt) | — |
| [PutBucketAcl](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketAcl.html) | table entry (from shunt) | — |
| [PutBucketAnalyticsConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketAnalyticsConfiguration.html) | table entry (from shunt) | — |
| [PutBucketCors](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketCors.html) | table entry (from shunt) | — |
| [PutBucketEncryption](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketEncryption.html) | table entry (from shunt) | — |
| [PutBucketIntelligentTieringConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketIntelligentTieringConfiguration.html) | table entry (from shunt) | — |
| [PutBucketInventoryConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketInventoryConfiguration.html) | table entry (from shunt) | — |
| [PutBucketLifecycle](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketLifecycle.html) | **folded** into `PutBucketLifecycleConfiguration` | identical request line |
| [PutBucketLifecycleConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketLifecycleConfiguration.html) | table entry (from shunt) | — |
| [PutBucketLogging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketLogging.html) | table entry (from shunt) | — |
| [PutBucketMetricsConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketMetricsConfiguration.html) | table entry (from shunt) | — |
| [PutBucketNotification](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketNotification.html) | **folded** into `PutBucketNotificationConfiguration` | identical request line |
| [PutBucketNotificationConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketNotificationConfiguration.html) | table entry (from shunt) | — |
| [PutBucketOwnershipControls](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketOwnershipControls.html) | table entry (from shunt) | — |
| [PutBucketPolicy](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketPolicy.html) | table entry (from shunt) | — |
| [PutBucketReplication](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketReplication.html) | table entry (from shunt) | — |
| [PutBucketRequestPayment](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketRequestPayment.html) | table entry (from shunt) | — |
| [PutBucketTagging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketTagging.html) | table entry (from shunt) | — |
| [PutBucketVersioning](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketVersioning.html) | table entry (from shunt) | — |
| [PutBucketWebsite](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketWebsite.html) | table entry (from shunt) | — |
| [PutObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html) | table entry (from shunt) | — |
| [PutObjectAcl](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObjectAcl.html) | table entry (from shunt) | — |
| [PutObjectAnnotation](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObjectAnnotation.html) | table entry (**added 2026-11-08**) | request syntax read from the linked page |
| [PutObjectLegalHold](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObjectLegalHold.html) | table entry (from shunt) | — |
| [PutObjectLockConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObjectLockConfiguration.html) | table entry (from shunt) | — |
| [PutObjectRetention](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObjectRetention.html) | table entry (from shunt) | — |
| [PutObjectTagging](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObjectTagging.html) | table entry (from shunt) | — |
| [PutPublicAccessBlock](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutPublicAccessBlock.html) | table entry (from shunt) | — |
| [RenameObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_RenameObject.html) | table entry (**added 2026-11-08**) | request syntax read from the linked page |
| [RestoreObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_RestoreObject.html) | table entry (from shunt) | — |
| [SelectObjectContent](https://docs.aws.amazon.com/AmazonS3/latest/API/API_SelectObjectContent.html) | table entry (from shunt) | — |
| [UpdateBucketMetadataAnnotationTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UpdateBucketMetadataAnnotationTableConfiguration.html) | table entry (**added 2026-11-08**) | request syntax read from the linked page |
| [UpdateBucketMetadataInventoryTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UpdateBucketMetadataInventoryTableConfiguration.html) | table entry (**added 2026-11-08**) | request syntax read from the linked page |
| [UpdateBucketMetadataJournalTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UpdateBucketMetadataJournalTableConfiguration.html) | table entry (**added 2026-11-08**) | request syntax read from the linked page |
| [UpdateObjectEncryption](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UpdateObjectEncryption.html) | table entry (**added 2026-11-08**) | request syntax read from the linked page |
| [UploadPart](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UploadPart.html) | table entry (from shunt) | — |
| [UploadPartCopy](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UploadPartCopy.html) | table entry (from shunt) | — |
| [WriteGetObjectResponse](https://docs.aws.amazon.com/AmazonS3/latest/API/API_WriteGetObjectResponse.html) | **excluded** | `POST /WriteGetObjectResponse` to an S3 Object Lambda endpoint only; unreachable through an S3-compatible backend (classifies as PostObject if ever seen) |

### The 18 added operations, with the request syntax read from the linked pages

| Operation | Request line (from "Request Syntax") | Scope note |
| --- | --- | --- |
| [CreateSession](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateSession.html) | `GET /?session` | directory buckets (zonal endpoint) |
| [RenameObject](https://docs.aws.amazon.com/AmazonS3/latest/API/API_RenameObject.html) | `PUT /{Key+}?renameObject` with required `x-amz-rename-source` | directory buckets |
| [CreateBucketMetadataTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateBucketMetadataTableConfiguration.html) | `POST /?metadataTable` | general purpose (V1) |
| [GetBucketMetadataTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketMetadataTableConfiguration.html) | `GET /?metadataTable` | general purpose (V1) |
| [DeleteBucketMetadataTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketMetadataTableConfiguration.html) | `DELETE /?metadataTable` | general purpose (V1) |
| [CreateBucketMetadataConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_CreateBucketMetadataConfiguration.html) | `POST /?metadataConfiguration` | general purpose (V2) |
| [GetBucketMetadataConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketMetadataConfiguration.html) | `GET /?metadataConfiguration` | general purpose (V2) |
| [DeleteBucketMetadataConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteBucketMetadataConfiguration.html) | `DELETE /?metadataConfiguration` | general purpose (V2) |
| [UpdateBucketMetadataInventoryTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UpdateBucketMetadataInventoryTableConfiguration.html) | `PUT /?metadataInventoryTable` | general purpose |
| [UpdateBucketMetadataJournalTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UpdateBucketMetadataJournalTableConfiguration.html) | `PUT /?metadataJournalTable` | general purpose |
| [UpdateBucketMetadataAnnotationTableConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UpdateBucketMetadataAnnotationTableConfiguration.html) | `PUT /?metadataAnnotationTable` | general purpose |
| [GetBucketAbac](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketAbac.html) | `GET /?abac` | general purpose |
| [PutBucketAbac](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutBucketAbac.html) | `PUT /?abac` | general purpose |
| [GetObjectAnnotation](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObjectAnnotation.html) | `GET /{Key+}?annotation&annotationName=…` | general purpose; not directory buckets |
| [PutObjectAnnotation](https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObjectAnnotation.html) | `PUT /{Key+}?annotation&annotationName=…` | general purpose |
| [DeleteObjectAnnotation](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeleteObjectAnnotation.html) | `DELETE /{Key+}?annotation&annotationName=…` | general purpose |
| [ListObjectAnnotations](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectAnnotations.html) | `GET /{Key+}?annotation&annotation-prefix=…` (no `annotationName`) | general purpose |
| [UpdateObjectEncryption](https://docs.aws.amazon.com/AmazonS3/latest/API/API_UpdateObjectEncryption.html) | `PUT /{Key+}?encryption&versionId=…` | general purpose; not directory buckets |

Folded pairs, verified on the linked pages: [GetBucketLifecycle](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketLifecycle.html) and [GetBucketLifecycleConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketLifecycleConfiguration.html) are both `GET /?lifecycle`; [GetBucketNotification](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketNotification.html) and [GetBucketNotificationConfiguration](https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetBucketNotificationConfiguration.html) are both `GET /?notification`; the `Put` pairs mirror them. [ListDirectoryBuckets](https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListDirectoryBuckets.html) is `GET /` on the `s3express-control` regional endpoint, distinguishable from `ListBuckets` only by host or `x-id`. [WriteGetObjectResponse](https://docs.aws.amazon.com/AmazonS3/latest/API/API_WriteGetObjectResponse.html) is `POST /WriteGetObjectResponse` with required `x-amz-request-route` and `x-amz-request-token`, served by S3 Object Lambda endpoints only.

## Classes

| Class | Meaning | Mirrors |
| --- | --- | --- |
| `LIST` | Enumerations: buckets, objects, versions, uploads, parts, annotations | S3's own per-prefix LIST guidance |
| `READ` | Object reads and object-metadata reads | S3's GET/HEAD per-prefix rate |
| `WRITE` | Object writes, including every multipart step except abort | S3's PUT/COPY/POST per-prefix rate |
| `DELETE` | Object deletes and multipart abort | S3's DELETE per-prefix rate |
| `BUCKET` | Bucket lifecycle and every bucket configuration subresource, `CreateSession`, CORS preflight | Control-plane traffic |
| `UNKNOWN` | Nothing matched; passed through and counted | |

Decisions recorded in `docs/DECISIONS.md`: `List*Configurations` are `BUCKET` (they read bucket configuration, not keys); `Preflight` is `BUCKET` (it is answered from the bucket's CORS configuration); `ListObjectAnnotations` is `LIST`; `AbortMultipartUpload` is `DELETE` as the specification's table says.

## Matching precedence

Highest first, as in shunt's table and the specification: (1) a subresource query key; (2) a distinguishing header (`x-amz-copy-source`); (3) request shape (service, bucket, key). `partNumber`, `versionId`, and `Range` are flags, never discriminators. `x-id` is a test oracle and a tiebreaker, never the sole source. `OPTIONS` at any level is `Preflight`. Everything else is `Unknown`.

Shape column: `service` = no bucket; `bucket` = bucket, no key; `key` = bucket and key. Qualifier = query key(s) that must be present, `k=v` for an exact value, `hdr:` for a header.

## The table

### Service

| Operation | Class | Method | Shape | Qualifier | Origin |
| --- | --- | --- | --- | --- | --- |
| ListBuckets | LIST | GET | service | — | shunt |

### Bucket: lifecycle and listings

| Operation | Class | Method | Shape | Qualifier | Origin |
| --- | --- | --- | --- | --- | --- |
| CreateBucket | BUCKET | PUT | bucket | — | shunt |
| DeleteBucket | BUCKET | DELETE | bucket | — | shunt |
| HeadBucket | BUCKET | HEAD | bucket | — (query ignored) | shunt |
| ListObjects | LIST | GET | bucket | — (no `list-type=2`) | shunt |
| ListObjectsV2 | LIST | GET | bucket | `list-type=2` | shunt |
| ListObjectVersions | LIST | GET | bucket | `versions` | shunt |
| ListMultipartUploads | LIST | GET | bucket | `uploads` | shunt |
| DeleteObjects | DELETE | POST | bucket | `delete` | shunt |
| PostObject | WRITE | POST | bucket | — (no subresource) | shunt |
| GetBucketLocation | BUCKET | GET | bucket | `location` | shunt |
| CreateSession | BUCKET | GET | bucket | `session` | added 2026-11-08 (directory buckets) |

### Bucket: subresources (GET / PUT / DELETE on one query key)

| Query key | GET | PUT | DELETE | Origin |
| --- | --- | --- | --- | --- |
| `acl` | GetBucketAcl | PutBucketAcl | — | shunt |
| `cors` | GetBucketCors | PutBucketCors | DeleteBucketCors | shunt |
| `encryption` | GetBucketEncryption | PutBucketEncryption | DeleteBucketEncryption | shunt |
| `lifecycle` | GetBucketLifecycleConfiguration | PutBucketLifecycleConfiguration | DeleteBucketLifecycle | shunt (absorbs deprecated `GetBucketLifecycle`, `PutBucketLifecycle`) |
| `logging` | GetBucketLogging | PutBucketLogging | — | shunt |
| `notification` | GetBucketNotificationConfiguration | PutBucketNotificationConfiguration | — | shunt (absorbs deprecated `GetBucketNotification`, `PutBucketNotification`) |
| `object-lock` | GetObjectLockConfiguration | PutObjectLockConfiguration | — | shunt |
| `ownershipControls` | GetBucketOwnershipControls | PutBucketOwnershipControls | DeleteBucketOwnershipControls | shunt |
| `policy` | GetBucketPolicy | PutBucketPolicy | DeleteBucketPolicy | shunt |
| `policyStatus` | GetBucketPolicyStatus | — | — | shunt |
| `publicAccessBlock` | GetPublicAccessBlock | PutPublicAccessBlock | DeletePublicAccessBlock | shunt |
| `replication` | GetBucketReplication | PutBucketReplication | DeleteBucketReplication | shunt |
| `requestPayment` | GetBucketRequestPayment | PutBucketRequestPayment | — | shunt |
| `tagging` | GetBucketTagging | PutBucketTagging | DeleteBucketTagging | shunt |
| `versioning` | GetBucketVersioning | PutBucketVersioning | — | shunt |
| `website` | GetBucketWebsite | PutBucketWebsite | DeleteBucketWebsite | shunt |
| `accelerate` | GetBucketAccelerateConfiguration | PutBucketAccelerateConfiguration | — | shunt |
| `abac` | GetBucketAbac | PutBucketAbac | — | added 2026-11-08 |
| `metadataTable` | GetBucketMetadataTableConfiguration | — (POST = CreateBucketMetadataTableConfiguration) | DeleteBucketMetadataTableConfiguration | added 2026-11-08 |
| `metadataConfiguration` | GetBucketMetadataConfiguration | — (POST = CreateBucketMetadataConfiguration) | DeleteBucketMetadataConfiguration | added 2026-11-08 |
| `metadataInventoryTable` | — | UpdateBucketMetadataInventoryTableConfiguration | — | added 2026-11-08 |
| `metadataJournalTable` | — | UpdateBucketMetadataJournalTableConfiguration | — | added 2026-11-08 |
| `metadataAnnotationTable` | — | UpdateBucketMetadataAnnotationTableConfiguration | — | added 2026-11-08 |

All rows in this section are class `BUCKET`. The two `POST /?metadataTable` and `POST /?metadataConfiguration` creates are new rows in the bucket-POST group, ahead of `delete` and the bare `PostObject` fallback.

### Bucket: configuration families (`?key&id` is Get, `?key` alone on GET is List)

| Query key | GET with `id` | GET without `id` | PUT (`id`) | DELETE (`id`) | Origin |
| --- | --- | --- | --- | --- | --- |
| `analytics` | GetBucketAnalyticsConfiguration | ListBucketAnalyticsConfigurations | PutBucketAnalyticsConfiguration | DeleteBucketAnalyticsConfiguration | shunt |
| `intelligent-tiering` | GetBucketIntelligentTieringConfiguration | ListBucketIntelligentTieringConfigurations | PutBucketIntelligentTieringConfiguration | DeleteBucketIntelligentTieringConfiguration | shunt |
| `inventory` | GetBucketInventoryConfiguration | ListBucketInventoryConfigurations | PutBucketInventoryConfiguration | DeleteBucketInventoryConfiguration | shunt |
| `metrics` | GetBucketMetricsConfiguration | ListBucketMetricsConfigurations | PutBucketMetricsConfiguration | DeleteBucketMetricsConfiguration | shunt |

All rows in this section are class `BUCKET`.

### Object: reads

| Operation | Class | Method | Shape | Qualifier | Origin |
| --- | --- | --- | --- | --- | --- |
| GetObject | READ | GET | key | — (`versionId`, `partNumber`, `Range` are flags) | shunt |
| HeadObject | READ | HEAD | key | — (query ignored; flags recorded) | shunt |
| GetObjectAttributes | READ | GET | key | `attributes` | shunt |
| GetObjectAcl | READ | GET | key | `acl` | shunt |
| GetObjectTagging | READ | GET | key | `tagging` | shunt |
| GetObjectRetention | READ | GET | key | `retention` | shunt |
| GetObjectLegalHold | READ | GET | key | `legal-hold` | shunt |
| GetObjectTorrent | READ | GET | key | `torrent` | shunt |
| SelectObjectContent | READ | POST | key | `select` + `select-type=2` | shunt |
| GetObjectAnnotation | READ | GET | key | `annotation` + `annotationName` | added 2026-11-08 |
| ListObjectAnnotations | LIST | GET | key | `annotation` without `annotationName` | added 2026-11-08 |
| ListParts | LIST | GET | key | `uploadId` | shunt |

### Object: writes

| Operation | Class | Method | Shape | Qualifier | Origin |
| --- | --- | --- | --- | --- | --- |
| PutObject | WRITE | PUT | key | — | shunt |
| CopyObject | WRITE | PUT | key | `hdr:x-amz-copy-source`, no `uploadId` | shunt |
| CreateMultipartUpload | WRITE | POST | key | `uploads` | shunt |
| UploadPart | WRITE | PUT | key | `uploadId` + `partNumber` | shunt |
| UploadPartCopy | WRITE | PUT | key | `uploadId` + `partNumber` + `hdr:x-amz-copy-source` | shunt |
| CompleteMultipartUpload | WRITE | POST | key | `uploadId` | shunt |
| PutObjectAcl | WRITE | PUT | key | `acl` | shunt |
| PutObjectTagging | WRITE | PUT | key | `tagging` | shunt |
| PutObjectRetention | WRITE | PUT | key | `retention` | shunt |
| PutObjectLegalHold | WRITE | PUT | key | `legal-hold` | shunt |
| RestoreObject | WRITE | POST | key | `restore` | shunt |
| RenameObject | WRITE | PUT | key | `renameObject` (header `x-amz-rename-source` required by AWS; not used to classify) | added 2026-11-08 (directory buckets) |
| PutObjectAnnotation | WRITE | PUT | key | `annotation` | added 2026-11-08 |
| UpdateObjectEncryption | WRITE | PUT | key | `encryption` | added 2026-11-08 |

### Object: deletes

| Operation | Class | Method | Shape | Qualifier | Origin |
| --- | --- | --- | --- | --- | --- |
| DeleteObject | DELETE | DELETE | key | — (`versionId` is a flag) | shunt |
| AbortMultipartUpload | DELETE | DELETE | key | `uploadId` | shunt |
| DeleteObjectTagging | DELETE | DELETE | key | `tagging` | shunt |
| DeleteObjectAnnotation | DELETE | DELETE | key | `annotation` | added 2026-11-08 |

### Other

| Operation | Class | Method | Shape | Qualifier | Origin |
| --- | --- | --- | --- | --- | --- |
| Preflight | BUCKET | OPTIONS | any | — | shunt |
| Unknown | UNKNOWN | any | any | nothing above matched (method and shape recorded as flags) | shunt |

## Precedence traps the tests pin (lifted from shunt's `TestPrecedence`, extended)

- `uploadId` before `uploads` on object POST: `POST /k?uploads&uploadId=1` is `CreateMultipartUpload`.
- `uploadId` + `partNumber` + `x-amz-copy-source` is `UploadPartCopy`; without the header, `UploadPart`; the header alone, `CopyObject`; `uploadId` without `partNumber` on PUT is `PutObject`.
- `partNumber` or `versionId` on GET is still `GetObject`; on DELETE still `DeleteObject`; HEAD ignores the query.
- `list-type=2` exactly; `list-type=3` is v1.
- `analytics&id=x` is Get, `analytics` alone is List (same for the other three families).
- `policyStatus` is not `policy`; `object-lock` is bucket level.
- `select` needs `select-type=2`, otherwise `Unknown`.
- Object-level `uploads` or `versions` on GET is `Unknown` (a bucket listing misaddressed).
- New: `annotation` with `annotationName` is Get, without it List; `encryption` on a key is `UpdateObjectEncryption`, on a bucket `GetBucketEncryption`; `POST /?metadataTable` is Create, `GET /?metadataTable` is Get; `GET /?session` is `CreateSession`, not `ListObjects`.
- `GET /` with `x-id=ListDirectoryBuckets` is `ListBuckets` (folded; the test asserts the fold so it is a decision, not an accident).
