// Package s3id holds identity extraction and the S3 operation classifier (shunt's op table, lifted). It yields one immutable RequestIdentity per request from headers and the request line, before any body byte is read. Zero allocations on the common path; see docs/DESIGN.md §4.
//
// Phase 0 placeholder: the package exists so the layout in docs/SPEC.md is real; code arrives
// in the phase that owns it (docs/STATUS.md).
package s3id
