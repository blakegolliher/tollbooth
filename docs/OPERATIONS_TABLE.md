# S3 operations table

Reference: [Amazon S3 API operations](https://docs.aws.amazon.com/AmazonS3/latest/API/API_Operations_Amazon_Simple_Storage_Service.html), fetched **2026-11-08**, listing **116** operations for the Amazon S3 service (the page carries no version date; the fetch date is the version). S3 Control, Tables, Vectors, Outposts, and Files operations are out of scope.

Source table: shunt `internal/s3/op.go` and `classify.go` at commit `9b36d09` (95 entries: 93 named operations, `Preflight`, `Unknown`). This file is the Phase 0 check of that table against the reference. **In Phase 1 the Go table in `internal/s3id` becomes the single source of truth and a test regenerates this file from it, failing when the two drift** (docs/SPEC.md, S3 operation classifier). Until then it is hand-written.

## Result of the check

| Outcome | Count | Operations |
| --- | --- | --- |
| In shunt and in the reference, kept | 92 | everything not listed below |
| In the reference, missing from shunt, **added** | 18 | the metadata-table family (9), `GetBucketAbac`, `PutBucketAbac`, `CreateSession`, `RenameObject`, `GetObjectAnnotation`, `PutObjectAnnotation`, `DeleteObjectAnnotation`, `ListObjectAnnotations`, `UpdateObjectEncryption` |
| In the reference, same wire shape as a kept operation, **folded** | 4 | `GetBucketLifecycle` → `GetBucketLifecycleConfiguration`, `PutBucketLifecycle` → `PutBucketLifecycleConfiguration`, `GetBucketNotification` → `GetBucketNotificationConfiguration`, `PutBucketNotification` → `PutBucketNotificationConfiguration` (deprecated aliases; `GET /?lifecycle` and `GET /?notification` are byte-identical request lines) |
| In the reference, not distinguishable without `x-id`, **folded** | 1 | `ListDirectoryBuckets` → `ListBuckets` (`GET /` on the `s3express-control` endpoint; the only discriminator is the host or `x-id`, and `x-id` is never the sole source) |
| In the reference, unreachable through an S3-compatible backend, **not added** | 1 | `WriteGetObjectResponse` (`POST /WriteGetObjectResponse` to an S3 Object Lambda endpoint; through the proxy it parses as a path-style bucket named `WriteGetObjectResponse` and classifies as `PostObject`) |
| In shunt, not in the reference, kept | 2 | `PostObject` (browser form upload, documented by AWS outside the operations list) and `Preflight` (CORS `OPTIONS`, not an API operation) |
| Sentinel | 1 | `Unknown` |

Table size after the check: **113** entries (110 reference names + `PostObject` + `Preflight` + `Unknown`). 92 + 18 = 110; 116 − 4 folded aliases − 1 `ListDirectoryBuckets` − 1 `WriteGetObjectResponse` = 110.

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
