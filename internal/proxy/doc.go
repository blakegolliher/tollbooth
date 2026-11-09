// Package proxy is the data path: the listener (plaintext or TLS with a hot-reloadable
// certificate, PROXY protocol v1/v2), the backend pool with active and passive health, and the
// passthrough handler lifted from shunt's passthrough pipeline. One handler, no mux, no
// middleware: identity, (Phase 2: limiter), outbound request built from the raw request-target
// with the inbound header map reused and nothing added, RoundTrip with a retry only before any
// request byte was sent, headers relayed as they arrive, body streamed through a pooled buffer
// with every chunk flushed, and a body cut short never delivered as complete (docs/DESIGN.md §4).
package proxy
