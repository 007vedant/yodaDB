# ADR 0022: Rebase disjoint writers by replaying exact row mutations

- Status: Accepted
- Date: 2026-10-25
- Owners: transactions and relational access

## Context

M7.3 can distinguish a genuine write/write conflict from a stale transaction
whose writes are disjoint from every intervening commit. The latter still
cannot publish its private roots directly: those roots descend from an older
manifest and would discard newer committed changes.

Rerunning the original SQL is also unsafe. For example, a stale transaction may
prepare `UPDATE jobs SET claimed = TRUE WHERE claimed = FALSE`; a newer commit
can insert another unclaimed job. Re-evaluating the predicate would claim a row
that was never visible to the original transaction.

## Decision

Successful DML statements append exact logical row mutations to their private
transaction state:

- insert records the table ID, physical row key, and inserted row;
- update records the table ID, old and new keys, and old and new rows;
- delete records the table ID, physical row key, and deleted row.

If validation finds no write conflict but the base is stale, the commit
coordinator starts from the latest committed manifest and replays those exact
mutations in their original order. It rebuilds table and secondary-index roots
with the new transaction ID, then publishes the rebased manifest. Before
updating or deleting, replay verifies that the latest row still equals the
recorded old row. Inserted and updated keys must equal their recorded keys.
Any mismatch fails conservatively with `ErrConflict`.

Mutation recording participates in statement rollback: a failed statement
restores both its logical footprint and mutation-log boundary. Original
tentative pages remain unreachable after a successful rebase and are future
compaction garbage.

Catalog mutations are not rebased in M7.4. A stale transaction containing DDL
still returns `ErrConflict`; M7.5 adds catalog-aware validation. Replay does use
the latest catalog, so a newly committed secondary index on an unchanged table
is maintained correctly. Unique-index failures during replay are conservatively
reported as conflicts until M7.5 tracks their logical keys directly.

## Consequences

Disjoint writers from one snapshot can both commit without lost updates. Rebase
preserves snapshot semantics because it applies only rows selected by the
original execution, not rows newly matching a predicate. Multi-statement
transactions retain mutation order and read-your-writes effects.

Replay initially runs under the serialized commit section. This favors
correctness and makes the published root set deterministic. Contention
benchmarks in M7.8 will show whether replay preparation should later move out of
that section with a second validation pass.

## Alternatives considered

- Publishing stale roots was rejected because it loses intervening commits.
- Re-executing SQL text or bound plans was rejected because predicates would be
  evaluated against rows outside the transaction's snapshot.
- Merging B+ tree pages was rejected because physical structure is not a stable
  logical identity and index maintenance would still need row semantics.
