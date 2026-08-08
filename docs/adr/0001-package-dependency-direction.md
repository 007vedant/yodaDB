# ADR 0001: Enforce downward package dependency direction

- Status: Accepted
- Date: 2026-08-05
- Owners: M0 integration owner

## Context

yodaDB spans SQL, transaction, and storage layers. Cyclic dependencies or SQL
knowledge leaking into storage would make its correctness boundaries difficult
to test and inspect.

## Decision

Dependencies flow from adapters and the public API through the SQL pipeline,
transaction and access-method layers, then B+ tree, pager, page codec, and VFS.
SQL packages do not perform file I/O. Storage packages do not import SQL syntax
packages. OS file operations are owned by `storage/vfs`.

Cross-layer interfaces live at the consuming boundary. Packages must have a
specific domain owner; a generic `common` package is not permitted.

The complete M0 package map is in `docs/architecture.md`.

## Consequences

Adapters may translate errors and values but cannot bypass the core API. Tests
can replace low-level dependencies at narrow interfaces. Some small types may
need deliberate placement to avoid upward imports.

## Alternatives considered

- A single engine package was rejected because it obscures ownership.
- A shared common package was rejected because it tends to accumulate unrelated
  helpers and permits accidental coupling.
