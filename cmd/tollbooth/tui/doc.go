// Package tui is the terminal UI (tollbooth tui). It is the only package that may import
// charmbracelet (ADR-0003, enforced by depguard in .golangci.yml). It is read-only and off the
// data path: it polls the admin API through internal/admin's client.
//
// Phase 0 placeholder.
package tui
