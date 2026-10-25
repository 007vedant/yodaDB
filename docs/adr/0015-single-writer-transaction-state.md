# ADR 0015: Use private root sets for single-writer transactions

- Status: Superseded by ADR 0020
- Date: 2026-10-23
- Owners: transactions and public API

## Context

M4 publishes every DML statement immediately. M5.1 needs explicit transactions
in which several statements read their own writes and become visible together,
without weakening the immutable-page rule or prematurely implementing M7's
optimistic multi-writer validation.

## Decision

A persistent database handle permits one active write transaction. Beginning a
transaction copies the latest committed manifest into a private root set,
clones the committed catalog, and reserves `base TxnID + 1`. Every successful
DML or DDL statement appends copy-on-write pages with that transaction ID and
advances only the private roots and, for DDL, the private catalog. Binding,
planning, reads, and index maintenance in the transaction use those private
snapshots. A failed statement leaves the preceding private state active,
allowing the caller to correct the error or roll back.

Commit publishes the private catalog root and table/index roots together in one
manifest, then promotes the private catalog in memory. A read-only commit writes
no manifest. Rollback discards the private catalog and roots; already appended
pages are unreachable garbage. Commit or rollback closes the transaction
permanently.

The native `Tx` handle owns its transaction. Direct operations on the parent
`DB` are rejected while that handle is active. SQL `BEGIN` creates a session-
style transaction whose following `DB` statements run against the private
roots until SQL `COMMIT` or `ROLLBACK`.

Autocommit statements retain their M4 behavior. For a table without a declared
primary key, insertion scans the current private table root and allocates one
greater than its largest hidden positive `INT64` RowID. This is deliberately
simple and guarantees distinct keys for multiple inserts in one transaction.

## Consequences

Multiple DML and DDL statements have atomic logical visibility and read their
own schema and data changes while committed state remains unchanged until
publication. Constraint or type errors do not automatically abort the
transaction. Only one writer can be prepared at a time, and hidden-RowID
allocation is linear in table size; both limitations favor inspectable
correctness at this stage.

M5.2 through M5.5 must harden publication and recovery. M6 adds independent
snapshot readers, and M7 replaces the single active writer rule with optimistic
validation and rebase.

## Alternatives considered

- Buffering SQL text or logical statements for replay at commit was rejected
  because it complicates read-your-writes and delays constraint feedback.
- Publishing after every statement and hiding intermediate manifests was
  rejected because those states would already be recoverable after a crash.
- Concurrent private writers were deferred to M7, after snapshot pinning and
  crash-safe publication are proven.
