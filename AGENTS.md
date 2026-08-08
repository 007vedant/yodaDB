# Coding-agent instructions

1. Read `PLAN.md` before changing code. It is the architectural source of truth.
2. Work on one milestone and one task ID at a time unless explicitly asked to do
   otherwise. Respect the dependency graph in the plan.
3. Do not silently change an invariant or on-disk format. Record an Architecture
   Decision Record under `docs/adr/` and update `PLAN.md` when a decision changes.
4. Every task must include proportionate tests. Run `go test ./...`, formatting,
   and the milestone gate before calling it complete.
5. Prefer small packages with explicit ownership. In particular, SQL packages
   must not perform file I/O, and storage packages must not depend on SQL syntax.
6. Never reuse a committed page in place. Committed pages are immutable except
   for the two explicitly documented metapages.
7. Favor correctness and inspectability over premature optimization. Benchmark
   before introducing caches, pooling, unsafe code, or clever encodings.
