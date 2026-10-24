# ADR 0016: Bind read transactions to immutable manifests

- Status: Accepted
- Date: 2026-10-24
- Owners: transactions and public API

## Context

M6 requires a reader to keep seeing one committed database state while later
writes publish new roots. Committed pages are immutable, but a read transaction
still needs an explicit identity, catalog version, and retention signal so that
future history reclamation cannot discard its reachable pages.

## Decision

`DB.BeginRead` creates a persistent, read-only `ReadTx` bound to the latest
committed manifest. The handle stores a defensive copy of that manifest and a
clone of its loaded catalog. Binding, planning, table access, and index access
all use those captured objects rather than the database's current state.

Each open handle pins its manifest transaction ID in a reference-counted map on
the database. Multiple handles may pin the same transaction independently.
Closing a handle releases one reference and is idempotent. Closing the parent
database invalidates its handles and releases all process-local pins. The
transaction ID is exposed by `ReadTx.TxnID`; zero denotes the empty database
before its first commit. Snapshot queries take the database's shared lock, so
multiple readers can execute concurrently. Writers and lifecycle changes take
the exclusive lock; they may proceed between a long-lived reader's statements
because a read handle does not hold the lock for its lifetime.

Read transactions are initially available only for persistent databases. Their
API exposes queries and close, not mutation methods. Immutable committed pages
mean a later writer can publish new roots without changing the pages reachable
from an older handle. M6.5 makes retention honor the pins.

## Consequences

A read transaction has repeatable schema and data visibility. A newly created
table or index is invisible to a handle that predates it, even after the DDL
commits. Handles must be closed to avoid unnecessarily retaining history once
reclamation exists.

The process-local pin map does not by itself protect snapshots across processes
or database close/reopen. Cross-process coordination remains outside the v1
scope, and named snapshots provide durable retention separately in M6.5.

## Alternatives considered

- Re-reading the latest manifest for every query was rejected because it gives
  statement-level visibility rather than a stable transaction snapshot.
- Pinning only table roots was rejected because catalog and index roots are part
  of the same atomic committed state.
- Holding a write-preventing lock for a handle's lifetime was rejected because
  immutable roots allow readers and a writer to coexist.
