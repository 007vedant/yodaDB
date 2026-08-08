# ADR 0004: Make full durability the default

- Status: Accepted
- Date: 2026-08-05
- Owners: transaction and storage

## Context

Callers must know what a successful commit promises. Performance options cannot
silently weaken that promise, and tests need to name the mode they exercise.

## Decision

The default is `DurabilityFull`. A commit succeeds only after new pages and its
manifest are synchronized, the selected metapage is written, and that metapage
is synchronized. Once acknowledged, the commit is intended to survive process
and power loss subject to the documented guarantees of the filesystem and
storage stack.

`DurabilityProcess` may be selected explicitly. It promises survival of process
failure but not power loss and may omit persistence barriers. Both modes retain
the same logical atomicity: reopening sees the complete old state or complete
new state, never a mixture.

I/O or synchronization failure makes the commit fail. Until recovery logic can
prove the handle's state, a failed publication leaves that handle unusable for
further writes.

## Consequences

Safe behavior is the default and weaker behavior is visible in configuration and
tests. Full durability has foreground latency. The exact syscall sequence and
supported filesystem matrix must be validated during M5.

## Alternatives considered

- Process-only durability by default was rejected as an unsafe surprise.
- A boolean sync option was rejected because it does not describe guarantees.
- A no-sync mode is deferred until it has precise semantics and a demonstrated
  use case.
