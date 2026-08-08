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
