# ADR 0012: Define the version-1 B+ tree leaf tombstone flag

- Status: Accepted
- Date: 2026-10-23
- Owners: storage

## Context

M3.5 introduces logical deletion without page redistribution or merging. Removing
a key from a copied leaf would make a parent's minimum-key separator stale when
the removed key is the first key in a right child. Retaining the key as a
tombstone keeps routing boundaries stable and lets old roots continue to expose
the earlier live value.

ADR 0009 reserved a `u16` flags field in each version-1 leaf slot and required it
to be zero for M2. M3 must define a persisted tombstone representation before
writing deleted entries.

## Decision

Leaf slot flag bit 0 (`0x0001`) means tombstone. All other flag bits remain
unknown and cause a corruption error. Reserved slot bytes remain zero.

A tombstone record contains the canonical key-length varint and key bytes, with
no value bytes. Decoders reject a tombstone record containing a value. The key
continues to participate in physical leaf ordering and internal separator
routing, but lookup and cursor APIs omit the entry.

Deletion copy-on-write replaces the target leaf entry with a tombstone, copies
the path to a new root, and performs no redistribution, merge, or root collapse.
Deleting a missing or already tombstoned key is a no-op. Inserting the same key
later replaces the tombstone with a live value in the new snapshot.

The leaf format remains version 1. ADR 0009 explicitly described zero flags as
the M2 rule and reserved the field for page-type meanings. Older readers safely
reject pages containing the newly defined bit; current readers continue to read
all older version-1 leaves.

## Consequences

Repeated delete/reinsert operations retain only one physical slot per key in a
given snapshot. Deletes can reduce leaf payload usage but do not reclaim slots
or reduce tree height. Later compaction, redistribution, or merging may remove
tombstones while preserving roots still reachable by retained snapshots.

The format has no deletion transaction ID. Snapshot visibility comes from the
immutable root selected by the transaction/manifest layer, not from interpreting
the tombstone in isolation.

## Alternatives considered

- Physically removing the slot was rejected because it complicates separator
  maintenance and is not the tombstone-first policy selected in `PLAN.md`.
- Encoding deletion as an empty value was rejected because empty values are
  valid user data.
- A leaf format-version bump was rejected because the slot flags field was
  reserved for compatible page-type extensions and old readers fail safely.
