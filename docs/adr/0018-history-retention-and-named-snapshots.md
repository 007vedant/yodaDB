# ADR 0018: Retain all history until an explicit monotonic cutoff

- Status: Accepted
- Date: 2026-10-24
- Owners: transactions, catalog, and public API

## Context

History needs a clear default, durable named pins, and an error that separates a
compacted-away transaction from a future or nonexistent ID. M9 performs physical
reclamation, but M6 must establish the logical boundary that compaction will
honor.

## Decision

Databases retain all committed history by default. A caller may explicitly call
`DB.RetainHistoryFrom(txnID)` to publish a new manifest whose
`OldestRetainedTxnID` is that exact retained transaction. The boundary advances
monotonically and cannot be reset or moved backward. Advancing the boundary is
a logical operation in M6; it makes older history inaccessible but does not yet
rewrite or shrink the file.

An active `ReadTx` pins its transaction in memory. A durable named snapshot pins
its target through a kind-3 catalog descriptor containing stable ID, exact
case-sensitive UTF-8 name, and transaction ID. `CreateSnapshot` and
`DropSnapshot` are autocommit catalog mutations; they are rejected while an
explicit write transaction is active. `OpenSnapshot` creates an ordinary pinned
`ReadTx` at the target. Dropping the name does not invalidate already open read
handles.

The retention boundary cannot advance past either an active read pin or a named
snapshot; the request returns a conflict until the handle closes or name is
dropped. A request below the durable boundary, including `AS OF` access, returns
the stable `history_not_retained` error category. Future, zero, and nonexistent
transaction IDs remain constraint errors.

Opening a database validates that every named snapshot targets an existing
manifest at or above the durable boundary. Violation is corruption because the
catalog and manifest retention state were supposed to publish atomically.

## Consequences

Retention is conservative and predictable: nothing disappears without an
explicit cutoff, and every live pin is honored. Applications can create durable
release or backup markers and reopen them later. Physical space is unchanged
until M9 compaction traces all roots at or above the boundary.

Named-snapshot creation and deletion each consume a transaction ID because they
are durable catalog changes. This is intentional: their lifecycle is itself
part of committed history.

## Alternatives considered

- A default last-N policy was deferred because choosing an implicit data-loss
  window before usage metrics exist would be surprising.
- Process-local named snapshots were rejected because names used for recovery,
  releases, or backups must survive reopen.
- Storing names in a sidecar was rejected because it would require a separate
  atomicity and durability protocol.
- Silently clamping a requested cutoff to the oldest pin was rejected because a
  conflict makes the retained state and required cleanup explicit.
