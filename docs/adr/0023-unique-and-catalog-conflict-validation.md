# ADR 0023: Validate unique keys and replay catalog creates

- Status: Accepted
- Date: 2026-10-25
- Owners: transactions, catalog, and relational access

## Context

Row-key validation alone does not protect every logical constraint. Two stale
transactions can insert different rows with the same unique secondary-index
key. Concurrent `CREATE TABLE` or `CREATE INDEX` statements can also receive
the same tentative numeric IDs even when their names are disjoint.

Rejecting every stale transaction containing DDL would be correct but would
unnecessarily serialize independent catalog changes.

## Decision

Transactions record each non-NULL unique secondary-index key as a logical write
resource scoped by the index's stable ID. The key uses the same canonical value
encoding as the index. Keys containing NULL are omitted because yodaDB permits
multiple NULL entries in a unique index. Primary-key conflicts remain row-key
conflicts because the primary key is the table's physical key.

`CREATE TABLE` records its global table-name resource. `CREATE INDEX` records
an index-name resource scoped to its owning table. When a conflict-free stale
transaction commits, its catalog creates are replayed against the latest
catalog before its exact row mutations. Replay allocates fresh table and index
IDs, remaps references from tentative IDs, rebuilds new indexes over the latest
rows, and then applies the recorded row mutations.

Resources based on tentative table or index IDs are not recorded for objects
created by the same transaction: concurrent transactions may legitimately
reuse those tentative numbers for unrelated objects. Their stable namespace is
protected by catalog names until replay assigns committed IDs. The currently
supported DDL is create-only; future alter/drop operations must additionally
record stable catalog-object resources.

This decision changes only in-memory validation and replay metadata. It does
not change the page or manifest format.

## Consequences

Two writers cannot both commit the same enforced unique key, while multiple
NULL keys and unrelated unique keys remain concurrent. Same-name catalog
creates conflict early, while disjoint table and index creates can rebase
without ID collisions. A replayed unique-index build or row mutation can still
fail conservatively with `ErrConflict` if the latest committed state violates
its constraint.

Catalog replay and index construction occur inside the serialized commit
section. This keeps ID assignment and published roots deterministic; M7.8 will
measure the resulting contention.

## Alternatives considered

- Validating only during physical index insertion was rejected because it
  cannot distinguish the logical conflict before replay and gives weaker
  diagnostics.
- Treating all NULL values as one unique key was rejected because it contradicts
  the database's documented multiple-NULL semantics.
- Rejecting all stale DDL was rejected because independent creates can be
  remapped safely.
- Using tentative numeric IDs as conflict identities was rejected because two
  private catalogs can assign the same number to unrelated objects.
