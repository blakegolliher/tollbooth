// Package s3id computes one immutable RequestIdentity per request from headers and the request
// line only, before a single body byte is read: access key (SigV4 and SigV2, header and
// presigned), bucket and key in both addressing styles, the list prefix (ADR-0001), source
// address (peer, PROXY protocol, trusted X-Forwarded-For), and the S3 operation with its class.
//
// The classifier is shunt's table (internal/s3 at 9b36d09), lifted and extended by the 18
// operations the API reference lists that shunt lacked; docs/OPERATIONS_TABLE.md is generated
// from it by TestOperationsTableIsCurrent. The common path allocates nothing: the request-target
// is split, not parsed; the query is scanned into a bitmask, not a map; every string in the
// identity is a substring of the request unless it needed percent-decoding (docs/DESIGN.md §4.2).
package s3id
