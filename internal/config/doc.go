// Package config is the YAML schema, loader, whole-file validation, and the derived values the
// data path needs (the identity configuration, the admin token). Unknown keys are errors with the
// key path named; defaults are applied before validation; a Config is immutable once returned and
// is published behind one atomic.Pointer by serve. Nothing here ever writes the operator's file
// (docs/CONFIG.md, docs/DESIGN.md §7.1).
package config
