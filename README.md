# yodaDB

yodaDB is a learning-oriented, embedded relational database written in Go. Its
design combines strict types, copy-on-write storage, snapshot reads, optimistic
transactions, checksummed pages, and first-class change history.

The project is built in vertical milestones. [PLAN.md](./PLAN.md) is the
authoritative source for architecture, invariants, task dependencies, and
acceptance gates. [AGENTS.md](./AGENTS.md) contains the contribution rules that
keep implementation work aligned with that plan.

Status: **M0 engineering foundation**. Database behavior begins in M1; the
current CLI intentionally exposes only foundation-level help and version output.

## Requirements

- Go 1.24.5
- A POSIX-like shell and `make` for the convenience targets

## Commands

```sh
make check
go run ./cmd/yodadb --help
go run ./cmd/yodadb --version
make bench
```

`make check` verifies formatting, runs `go vet`, the unit tests, and the race
detector. CI runs the same gate.

## Repository layout

Package ownership and dependency direction are documented in
[docs/architecture.md](./docs/architecture.md). Architectural decisions are
recorded under [`docs/adr`](./docs/adr/README.md).

## License

yodaDB is licensed under the Apache License 2.0. See [LICENSE](./LICENSE).
