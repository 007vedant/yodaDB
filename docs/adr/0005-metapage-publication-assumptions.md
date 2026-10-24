# ADR 0005: Validate double-metapage publication before relying on it

- Status: Accepted
- Date: 2026-08-05
- Owners: transaction and storage

## Context

The plan publishes commits through alternating metapages. Correctness depends on
what survives torn writes, reordering, and sync on supported filesystems.

## Decision

Pages 0 and 1 are the only committed pages that may be overwritten. Publication
writes the lower-generation or invalid metapage slot only after all referenced
pages and the manifest have been appended and synchronized. It then synchronizes
the metapage write before acknowledging success.

Each metapage is independently versioned and checksummed. Open does not select a
slot by generation alone: it selects the highest-generation valid slot whose
manifest and referenced roots pass the required structural validation. An
invalid newer slot falls back to the older valid slot. Unreachable tail pages
are garbage.

This is a design hypothesis until M5 fault-injection and real-filesystem tests
validate the required atomicity, ordering, and sync behavior on each supported
platform. If those assumptions cannot be defended, M5 introduces a small
checksummed commit journal through a superseding ADR.

M5.2 implements candidate validation and publication ordering: open validates
the referenced manifest plus the shallow structure of every catalog, table, and
index root before selecting a generation, and failed publication I/O makes the
handle reject further writes until reopen. This does not close the design
hypothesis; M5.3 and M5.4 still must exercise every persistence boundary.

## Consequences

Recovery has two independent publication candidates and never repairs data
silently during normal open. Platform support must state filesystem assumptions.
The project cannot claim durable commits before the M5 gate passes.

## Alternatives considered

- A single overwritten header was rejected because a torn write could destroy
  the only publication record.
- An immediate write-ahead journal was deferred until testing shows whether the
  simpler protocol is insufficient.
- Choosing only the largest generation was rejected because it could reference
  incomplete or corrupt data.
