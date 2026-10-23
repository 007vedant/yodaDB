# yodaDB

yodaDB is a learning-oriented, embedded relational database written in Go. Its
design combines strict types, copy-on-write storage, snapshot reads, optimistic
transactions, checksummed pages, and first-class change history.

The project is built in vertical milestones. [PLAN.md](./PLAN.md) is the
authoritative source for architecture, invariants, task dependencies, and
acceptance gates. [AGENTS.md](./AGENTS.md) contains the contribution rules that
keep implementation work aligned with that plan.

Status: **M3 copy-on-write B+ tree**. The M1 in-memory SQL slice remains
available, and low-level storage now includes immutable ordered trees, snapshot
roots, insertion, tombstone deletion, randomized model tests, and tree
inspection. SQL table persistence intentionally begins in M4.

## Requirements

- Go 1.24.5
- A POSIX-like shell and `make` for the convenience targets

## Commands

```sh
make check
make model-test-extended
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
Opening a `.yoda` file through the SQL API is introduced with persistent storage
in M4.

## Inspecting a page file

The CLI now uses Cobra for its command hierarchy. Given a low-level `.yoda` file
created through the M2 storage API or tests, inspect validated metadata and page
headers with:

```sh
go run ./cmd/yodadb inspect app.yoda
```

`inspect` does not execute SQL or modify the file. It reports the file identity,
format and page sizes, selected metapage generation, transaction/manifest
placeholders, page count, and each validated common page header.

To visualize a B+ tree whose root page ID is known:

```sh
go run ./cmd/yodadb pages app.yoda --tree 42
```

The output shows internal-page child ranges, leaf counts, tombstones, sibling
references, and page generations. M4 will resolve table and index names through
the persistent catalog; during M3 the root is supplied as a numeric page ID.

## Repository layout

Package ownership and dependency direction are documented in
[docs/architecture.md](./docs/architecture.md). Architectural decisions are
recorded under [`docs/adr`](./docs/adr/README.md).

## License

yodaDB is licensed under the Apache License 2.0. See [LICENSE](./LICENSE).
