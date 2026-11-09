# bench/

Phase 1 adds the null backend and the recording backend; Phase 2 adds the load generator
(ADR-0002). Results and profiles land under `results/<date>/` (docs/SPEC.md, Performance targets).
`hdrhistogram-go` is imported here and nowhere else (depguard).
