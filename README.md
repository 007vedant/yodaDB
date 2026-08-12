# yodaDB

yodaDB is a learning-oriented, embedded relational database written in Go. Its
design combines strict types, copy-on-write storage, snapshot reads, optimistic
transactions, checksummed pages, and first-class change history.

The project is built in vertical milestones. [PLAN.md](./PLAN.md) is the
authoritative source for architecture, invariants, task dependencies, and
acceptance gates. [AGENTS.md](./AGENTS.md) contains the contribution rules that
keep implementation work aligned with that plan.

Status: **M1 in-memory relational vertical slice**. The lexer, parser, binder,
strict scalar types, in-memory catalog, iterator executor, native API, and SQL
shell are implemented. Persistence intentionally begins in a later milestone.

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

## In-memory SQL shell

Run the CLI without a database filename:

```sh
go run ./cmd/yodadb
```

The M1 vertical slice supports `CREATE TABLE`, single-row `INSERT`, and
single-table `SELECT` with projections and Boolean/comparison `WHERE`
expressions:

```text
yoda> CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL);
yoda> INSERT INTO users VALUES (1, 'Ada');
1 row(s) affected
yoda> SELECT name FROM users WHERE id = 1;
Ada
```

The database exists only in memory and disappears when the process exits.
Opening a `.yoda` file is introduced with persistent storage in M4.

## Repository layout

Package ownership and dependency direction are documented in
[docs/architecture.md](./docs/architecture.md). Architectural decisions are
recorded under [`docs/adr`](./docs/adr/README.md).

## License

yodaDB is licensed under the Apache License 2.0. See [LICENSE](./LICENSE).
