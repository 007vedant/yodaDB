# yodaDB

yodaDB is a learning-oriented, embedded relational database written in Go. Its
design combines strict types, copy-on-write storage, snapshot reads, optimistic
transactions, checksummed pages, and first-class change history.

The project is built in vertical milestones. [PLAN.md](./PLAN.md) is the
authoritative source for architecture, invariants, task dependencies, and
acceptance gates. [AGENTS.md](./AGENTS.md) contains the contribution rules that
keep implementation work aligned with that plan.

Status: **M4 persistent SQL tables and indexes**. The database now persists its
catalog, tables, primary keys, and secondary indexes through immutable B+ tree
roots. `INSERT`, `SELECT`, `UPDATE`, `DELETE`, unique indexes, range/index
planning, and deterministic `EXPLAIN` survive close and reopen.

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

## SQL shell

Run the CLI without a database filename for an in-memory database:

```sh
go run ./cmd/yodadb
```

Pass a filename to create or reopen a persistent database:

```sh
go run ./cmd/yodadb app.yoda
```

The SQL slice supports `CREATE TABLE`, `CREATE [UNIQUE] INDEX`, single-row
`INSERT`, `SELECT`, `UPDATE`, `DELETE`, and `EXPLAIN SELECT`:

```text
yoda> CREATE TABLE users (id INT64 PRIMARY KEY, name TEXT NOT NULL);
yoda> INSERT INTO users VALUES (1, 'Ada');
1 row(s) affected
yoda> SELECT name FROM users WHERE id = 1;
Ada
```

Without a filename the database disappears when the process exits. With a
filename, committed statements are available after restarting the process.

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
references, and page generations. The diagnostic command currently accepts the
root as a numeric page ID.

## Repository layout

Package ownership and dependency direction are documented in
[docs/architecture.md](./docs/architecture.md). Architectural decisions are
recorded under [`docs/adr`](./docs/adr/README.md).

## License

yodaDB is licensed under the Apache License 2.0. See [LICENSE](./LICENSE).
