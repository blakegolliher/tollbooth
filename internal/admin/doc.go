// Package admin holds the admin HTTP server (health, readiness, metrics, and from Phase 2 the
// /admin/* routes), the Prometheus registry with every per-request handle pre-resolved so an
// observation allocates nothing (docs/DESIGN.md §9.3), and, from Phase 4, the client the CLI and
// TUI share. It imports no proxy, limiter, or showback package: those hand it functions and
// counters, so the dependency graph stays acyclic (docs/DESIGN.md §2).
package admin
