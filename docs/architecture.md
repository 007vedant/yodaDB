# Package architecture

`PLAN.md` is the architectural source of truth. This document makes its package
dependency direction explicit for tooling and reviews.

```text
cmd/yodadb, driver
        |
        v
       db
        |
        v
sql/lexer -> sql/parser -> sql/binder -> sql/plan -> sql/exec
                              |                       |
                              v                       v
                           catalog <---------------> txn
                              |                       |
                              +----------+------------+
                                         v
                                      access methods
                                         |
                                         v
                                  storage/btree
                                         |
                                         v
                                  storage/pager
                                         |
                                         v
                                   storage/page
                                         |
                                         v
                                    storage/vfs
```

The access-method boundary will be introduced by its owning milestone; it is not
an empty package in the M0 scaffold.

## Ownership rules

- `db` owns the native public API. `driver` and `cmd/yodadb` are adapters.
- `sql/*` owns SQL text, syntax, semantic binding, plans, and execution. SQL
  packages must not perform file I/O.
- `types` owns logical scalar semantics and byte codecs, but no query syntax.
- `catalog` owns stable schema identities and versioned descriptors.
- `txn` owns transaction state, snapshots, validation, and commit coordination.
- `storage/*` owns persisted bytes and must not depend on SQL syntax.
- `changefeed` owns logical changes, not physical page mutations.
- `internal/testutil` owns test-only fixtures and fault-injection helpers.
- Cross-layer interfaces are declared at the consuming boundary. New generic
  utility packages require a concrete owner and use case.

Dependencies flow downward. Import cycles, upward imports from storage into SQL,
and direct OS file access outside `storage/vfs` are architectural violations.
