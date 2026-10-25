# Benchmarking

M0 provides a small benchmark harness so later milestones have one repeatable
entry point. Run all current benchmarks with:

```sh
make bench
```

For a reviewable comparison, collect multiple samples from an otherwise idle
machine:

```sh
go test -run '^$' -bench . -benchmem -count 5 ./internal/bench
```

Benchmark reports must include the commit, benchmark names and workload sizes,
Go version, operating system and architecture, filesystem when storage is
involved, durability mode, database size, and relevant concurrency settings.
Latency-sensitive database benchmarks should report distributions or
percentiles rather than only throughput.

The foundation CRC-64 benchmark is evidence about local cost, not a performance
gate. New caches, pooling, unsafe code, or clever encodings require a benchmark
showing the problem and before/after results on a representative workload.

## Concurrent writers

Run the M7 contention workloads with:

```sh
go test -run '^$' -bench '^BenchmarkWriterContention$' -benchmem ./internal/bench
```

Both workloads use one persistent database handle, process durability, and the
default `testing.B.RunParallel` worker count (normally `GOMAXPROCS`). Each
benchmark iteration is one transaction attempt:

- `disjoint_keys` inserts a distinct primary key, exercising validation and
  stale-root rebase without intentional logical conflicts;
- `hot_key` updates one shared primary-key row, exercising write/write aborts
  under maximum logical contention.

In addition to the standard aggregate `ns/op` and allocation metrics, the
benchmarks report per-attempt `p50-ns/op`, `p95-ns/op`, and `p99-ns/op`, plus
`abort_%`, `commit_%`, and the observed `writers` count. Latency includes begin,
SQL execution, validation/rebase, and commit, but excludes initial schema and
seed-row setup. Abort rate counts only typed transaction conflicts; any other
error fails the benchmark.

Use `-cpu` to compare fixed concurrency levels and `-count` for reviewable
samples, for example:

```sh
go test -run '^$' -bench '^BenchmarkWriterContention$' -benchmem -benchtime=2s -count=5 -cpu=1,4,8 ./internal/bench
```
