# ADR 0020: Prepare concurrent writers in private execution contexts

- Status: Accepted
- Date: 2026-10-25
- Owners: transactions and public API

## Context

ADR 0015 stores one active writer directly on `DB` and holds the database mutex
through every statement. M7.2 must let independent write transactions prepare
at the same time without changing committed visibility or prematurely claiming
that stale transactions can safely commit.

## Decision

Each native `Tx` owns a private execution context containing its base manifest,
working copy-on-write roots, and cloned catalog. Transaction statements hold a
shared writer-lifecycle lock, separate from the database commit/state mutex,
and a per-transaction mutex. Separate handles can therefore parse, scan,
evaluate, and append tentative pages concurrently with one another and with
committed operations; the shared manifest/pager layer remains responsible for
thread-safe physical reads and appends.

The parent `DB` tracks live handles so close can abort all of them. Their base
transactions are pinned against history retention. Reads through the parent DB
continue to see only committed roots while private transactions are active.

Publication remains serialized by the parent database mutex. Until M7.3 adds
logical validation, a dirty transaction may commit only if its base is still
the latest committed transaction. A stale writer aborts with `ErrConflict`,
even when its logical changes are disjoint. Read-only transactions may close
successfully from an older snapshot. SQL session-style `BEGIN` retains its
single-session representation but receives the same conservative stale-base
guard.

## Consequences

Multiple callers can prepare private changes without a global exclusive writer
lock, and uncommitted pages and catalog changes remain unreachable. This stage
intentionally improves preparation concurrency without yet improving stale
commit success: M7.3 and M7.4 will validate logical footprints and rebase safe
writes.

The writer-lifecycle read lock is held for each transaction statement. It does
not serialize peer writers or commit publication, but it prevents close from
racing an in-flight statement. Each transaction still serializes its own
methods.

## Alternatives considered

- Moving every executor function to accept an explicit transaction parameter
  was deferred because a private execution context preserves existing tested
  statement behavior with a smaller surface-area change.
- Allowing stale private roots to publish was rejected because it can discard
  intervening commits.
- Replaying disjoint stale writes here was rejected because validation and
  rebase are the explicit deliverables of M7.3 and M7.4.
