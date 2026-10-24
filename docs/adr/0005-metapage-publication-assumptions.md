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

M5.3 provides the deterministic crashable VFS needed for that validation. It
can fail or shorten selected operations, persist only a prefix of a successful
write, reorder which pending mutations a sync makes durable, and discard all
remaining unsynced state at a chosen operation boundary.

M5.4 applies that VFS after every operation in the publication protocol and at
targeted failed, short, torn, and reordered persistence points. Recovery always
selects the exact prior manifest and root or the exact new manifest and root;
it never exposes a mixture.

M5.4 treats a final incomplete page after a crash as unreachable garbage. Open
exposes only the file's complete-page prefix to the pager, while candidate
validation still determines whether either metapage names a wholly valid
committed state. This is necessary because a torn append can extend the file by
less than one page without changing either committed metapage.

M5.5 makes candidate fallback corruption-specific. A read failure is returned as
I/O instead of being treated as an invalid candidate, preventing a transient
device error from silently exposing an older generation. Every nonzero root and
each immediate child or sibling named by its root must precede the candidate's
manifest page; complete or partial bytes after that manifest are unreachable
garbage and cannot be pulled into the committed state.

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
