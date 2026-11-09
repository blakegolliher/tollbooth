// Package limiter holds rate-limit rules, precompiled matchers, and the GCRA key store. One pass over the matching rules in config order consumes a token from each; the first denial throttles. See docs/DESIGN.md §5.
//
// Phase 0 placeholder: the package exists so the layout in docs/SPEC.md is real; code arrives
// in the phase that owns it (docs/STATUS.md).
package limiter
