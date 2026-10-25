# ADR 0019: Model optimistic conflicts with logical transaction footprints

- Status: Accepted
- Date: 2026-10-25
- Owners: transactions

## Context

M7 introduces concurrent writers. Page IDs and B+ tree roots cannot identify
conflicts: copy-on-write updates to independent rows may replace the same tree
root, while the same logical row can occupy different pages after a rebase.
Validation therefore needs identities that survive physical rewrites.

## Decision

Each write transaction records separate logical read and write sets. Resources
belong to three namespaces:

- a row is identified by stable table ID plus its canonical encoded table key;
- a unique constraint entry is identified by stable index ID plus its canonical
  encoded index key;
- a catalog resource is identified by object kind plus either a stable object
  ID or a name. Index names also carry their owning table ID because their
  namespace is table-local. Name resources detect concurrent creates and
  renames before a new stable ID is published.

Resource identities own immutable copies of their key material and are
comparable, so recording is idempotent and exact. Conflict validation for the
v1 snapshot-isolation target compares write sets only. A transaction conflicts
when its write set intersects writes committed since its base transaction.
Read/write overlap is recorded but does not abort either transaction.

This ADR defines only the in-memory logical model. M7.2 wires recording into
private transaction preparation; M7.3 defines validation and durable history
needed by the commit coordinator.

## Consequences

Physical page churn does not produce false conflicts, and disjoint writes in
one table can commit independently. Row, unique-key, and catalog namespaces do
not alias even when their numeric IDs and bytes match. Keeping reads makes the
transaction's dependencies inspectable and leaves room for stronger isolation
without claiming serializability today.

Snapshot isolation permits write skew. For example, two doctors can both read
that the other is on call, then update only their own row to off call. Their
read sets overlap the other's write, but their write sets are disjoint, so both
may commit and violate an application-level invariant. Applications needing
that invariant must arrange a shared written guard row or retry around an
explicit conflict.

## Alternatives considered

- Root or page identity was rejected because it couples logical validation to
  copy-on-write layout and conflicts unrelated updates.
- Treating every read/write overlap as a conflict was rejected because that is
  closer to serializable validation than the documented snapshot-isolation
  target and would abort more work.
- Using catalog names alone was rejected because renames would make existing
  objects lose stable identity; using IDs alone misses concurrent creation of
  the same name.
