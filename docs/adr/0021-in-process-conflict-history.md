# ADR 0021: Validate live writers with in-process commit footprints

- Status: Accepted
- Date: 2026-10-25
- Owners: transactions and public API

## Context

Concurrent private writers can start from the same committed manifest. Before a
stale writer may be rebased, the commit path must determine whether any logical
resource it writes was also written by a transaction committed since its base.
Physical roots cannot answer that question without false conflicts.

## Decision

Each open database handle owns a commit coordinator initialized at the latest
durable transaction ID. Every successful publication records an immutable copy
of that transaction's logical footprint. Validation unions the footprints of
all commits after a writer's base and computes their exact write/write
intersection with the candidate footprint.

M7.3 records table rows by stable table ID and canonical encoded table key for
`INSERT`, `UPDATE`, and `DELETE`. Primary-key changes record both the old and new
row identities. Failed statements restore the footprint they had before the
statement. Unique-index keys and catalog resources join validation in M7.5.

Conflict history is in memory rather than stored in the database file. This is
sufficient because write handles cannot survive closing or reopening their
parent database. After open, a new coordinator baseline is the recovered latest
transaction; every transaction that can still commit starts at or after that
baseline.

A logical conflict aborts with `ErrConflict`. A stale transaction with no
logical conflict is identified separately but still returns `ErrConflict`
because replay onto current roots is M7.4. Publication remains serialized.

## Consequences

Same-row writers cannot both commit, including when another unrelated commit
occurs between them. Disjoint stale writers are no longer confused with genuine
logical conflicts, preparing the commit path for rebase. Coordinator state does
not change the on-disk format or recovery protocol.

If coordinator bookkeeping ever loses continuity after a durable publication,
the handle resets its baseline to that publication. Older live writers then
fail conservatively because their required conflict history is unavailable;
the durable commit is still reported as successful.

## Alternatives considered

- Persisting conflict footprints under the manifest change root was deferred;
  M8 owns durable logical change encoding, and live writers do not cross reopen.
- Comparing table roots was rejected because any write to the same B+ tree
  would conflict, even for disjoint row keys.
- Validating only against the latest commit was rejected because it can miss a
  conflicting earlier commit followed by an unrelated newer one.
