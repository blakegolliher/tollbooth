# ADR-0005: Performance targets at v0.1.0, the Go-versus-Rust call, and the bench cross-check

Status: **accepted** (Blake, 2026-11-10, answering Gate 5 questions 1 to 3): (1) option (b), tag
v0.1.0 with the four end-to-end rows published as unverified on the reference node and that run
made the gate for v0.1.1; (2) stay on Go, reopened only by a reference-node profile that puts the
runtime rather than the kernel at the top; (3) elbencho replaces warp as the external cross-check.
Proposed in Phase 5 on 2026-11-09.

## Context

The specification's performance targets (docs/SPEC.md, Performance targets) are defined on a
reference node: 16 vCPU, 64 GiB, 25 GbE or faster, with the proxy, the generator, and the null
backend on pinned CPU sets, 30 s warm-up, three runs, the median reported. Gate 5 requires every
target met or each miss covered by an ADR, "including the Go-versus-Rust call".

Every number tollbooth has so far comes from the 2 vCPU dev box, where the three processes share
two cores. What those runs established (docs/reports/gate-2.md §4, gate-3.md §4, bench/results/):

| Row | Target | Dev box |
| --- | --- | --- |
| Limiter decision, 100 rules, 1,000,000 keys | ≤ 10 µs p99 | **p99 4.7 µs**; 0 allocations; 335 ns per decision on the 4 vCPU CI runner |
| Showback increment | ≤ 2 µs p99 | **198 ns**, 0 allocations |
| Memory at 1,000,000 keys + 250,000 tuples | ≤ 1 GiB RSS | **226 B/key + 257 B/tuple ≈ 290 MiB** for the two stores at their caps |
| Config reload under load | ≤ 100 ms, 0 failed | **0.60 ms**, 0 failed |
| Startup to ready | ≤ 1 s | **65–78 ms** |
| Added latency, 4 KiB GET, plaintext | p50 ≤ 150 µs, p99 ≤ 1 ms | added p50 2.1 ms, p99 7.0 ms: the direct run alone was 808 µs p50, five times the budget, because the generator and the backend compete for the same two cores; **not a measurement of the proxy** |
| Small-op throughput, plaintext | ≥ 50,000 req/s | 2,194 req/s proxied vs 6,284 direct at concurrency 8 on two shared cores; same caveat |
| Throughput, TLS terminated; 32 × 64 MiB streaming at line rate | ≥ 30,000 req/s; ≥ 80% of NIC | not measured (no NIC to speak of on the dev box) |
| Showback on versus off | ≤ 2% | within run-to-run noise on shared cores (D-055) |

The profile of the proxy under the dev-box load (gate-2.md §4) is the one fact that carries to any
machine: 44% of the proxy's CPU is socket syscalls, 27% of it the per-chunk flush that transparency
requires (D-006), 16% the backend read loop; the limiter decision is 1.25%, the identity pass
0.75%, the handler's own work under 2%. The proxy is I/O bound. Its own code costs microseconds
per request and allocates seven times per forwarded request, all seven named (DESIGN §9).

## Decision to make

Three parts:

**(1) What v0.1.0 claims.** Options:

- (a) Tag v0.1.0 only after a reference-node run meets every row, or ADRs each miss with its number.
  Honest and what the gate text says. Blocks the tag on hardware this project has not had.
- (b) Tag v0.1.0 now, with the five rows that are hardware-independent met on the dev box and the
  four end-to-end rows marked **unverified**, and treat the reference-node run as the v0.1.1 gate.
  The CHANGELOG and `docs/OPERATIONS.md` say so in those words.
- (c) Run the end-to-end rows on whatever larger machine is available (a lab box, a cloud VM with
  8+ vCPUs) and record them as "indicative, not the reference node".

Recommendation: **(b)**, with (c) if a machine appears before the tag. The dev-box profile shows
the proxy's cost is the kernel's, and the hardware-independent rows pass with margin; a release
that says "unverified on the reference node" is more useful to the field than no release.

**(2) Go versus Rust.** The specification asks for the call at Gate 5 if the p99 targets are
missed. The targets have not been measured on the reference node, so the question is premature,
and the evidence points the other way: the proxy spends its CPU in `read`, `write`, and the
per-chunk flush, which a Rust proxy would also issue; the Go-specific costs (GC, allocation) are
seven small allocations per request and 0 on the decision and increment paths, measured, with
`GOMEMLIMIT` bounding the heap. A rewrite would buy nothing until a profile on the reference node
shows the runtime, not the kernel, at the top. Recommendation: **stay on Go**; revisit only with a
reference-node profile that says otherwise.

**(3) The bench cross-check (amends ADR-0002).** ADR-0002 chose the in-repo generator with MinIO
warp as the external cross-check, pinned at Phase 5. MinIO has archived its open-source projects
(D-022); the warp release assets still exist but will not be maintained. elbencho (GPL-3.0, run as
a process, already pinned and used in the compatibility matrix) drives S3 PUT/GET at configurable
concurrency and object size and reports throughput and latency percentiles. Recommendation:
**elbencho replaces warp** as the cross-check; `bench/crosscheck.sh` runs it against direct and
proxied endpoints with the same object size as the generator's raw mode and the gate report
prints both side by side. warp is dropped, not pinned.

## Consequences

- With (b): `CHANGELOG.md` and `docs/OPERATIONS.md` carry an "unverified on the reference node"
  section listing the four rows; the Gate 5 report's performance table shows them as such; the
  v0.1.1 gate is the reference-node run with profiles saved under `bench/results/`.
- `bench/crosscheck.sh` (Phase 5) replaces the planned `bench/warp/run.sh`; `docs/deps.md` and
  `test/integration/clients.sh` already carry elbencho.
- The Go decision stands until contradicted by a reference-node profile; the ADR is reopened then,
  not before.
