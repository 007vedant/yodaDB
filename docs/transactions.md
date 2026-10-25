# Transactions and concurrency

yodaDB provides snapshot isolation for persistent databases. A transaction
reads the committed snapshot captured by `DB.Begin`, sees its own writes, and
does not expose those writes through the parent `DB` until commit. Commit is
serialized, but separate transactions can prepare work concurrently.

## Conflicts

Optimistic validation tracks logical resources rather than physical pages:

- a table row is identified by table ID and canonical row key;
- a non-NULL unique-index value is identified by index ID and encoded key;
- catalog creates are identified by table names or table-scoped index names.

If a newer transaction wrote the same resource, commit returns a typed
`*db.ConflictError` that also matches `dberrors.ErrConflict`. Disjoint stale
transactions are replayed on the latest roots using the exact rows selected by
their original snapshot; SQL predicates are not re-evaluated during replay.

`DB.RetryTransaction` can rerun a complete callback after a typed commit
conflict. It never retries an individual statement or a callback error. Because
the callback may execute more than once, external effects such as sending a
message must be idempotent or delayed until the helper succeeds.

## Write skew

Snapshot isolation prevents dirty reads and lost updates, but it is not full
serializability. Write skew remains possible. For example, suppose rows for
doctors Ada and Grace both say `on_call = TRUE`, and the application requires
at least one doctor to stay on call:

1. Transaction A reads both rows and changes only Ada to `FALSE`.
2. Transaction B starts from the same snapshot, reads both rows, and changes
   only Grace to `FALSE`.
3. The writes touch different row keys, so both transactions can commit.
4. The final state has no doctor on call, despite each transaction observing a
   valid starting state.

Applications needing such a cross-row invariant should make both transactions
write a shared guard row (so validation detects a conflict) or enforce the
invariant in a future serializable layer. Retrying alone does not prevent write
skew when the writes remain disjoint.

## Deterministic validation

The concurrency suite enumerates all 20 order-preserving interleavings of two
writers' `BEGIN`, write, and commit steps. After every step it compares public
committed reads with a small reference model. Cases cover disjoint rows,
same-row updates, duplicate unique keys, and duplicate catalog names. This
checks both conflict outcomes and the absence of dirty intermediate state
without depending on goroutine timing.
