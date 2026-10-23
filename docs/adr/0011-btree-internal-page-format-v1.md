# ADR 0011: Fix the version-1 B+ tree internal-page format

- Status: Accepted
- Date: 2026-10-23
- Owners: storage

## Context

M3.2 adds read-only traversal through arbitrary-height B+ trees. Internal pages
must store variable-width separator keys and child page IDs in a canonical,
corruption-detectable layout that fits the M2 common page format.

## Decision

A version-1 internal page uses the full 4060-byte common-page payload. Its
16-byte header is:

| Offset | Size | Field |
|---:|---:|---|
| 0 | 1 | internal format version, `1` |
| 1 | 1 | flags, zero |
| 2 | 2 | separator count |
| 4 | 2 | free-space start |
| 6 | 2 | free-space end |
| 8 | 8 | first/leftmost child page ID |

Sixteen-byte slots grow forward from offset 16. Each slot contains key offset
`u16`, key length `u16`, flags `u16`, reserved `u16`, and right-child page ID
`u64`. Flags and reserved bytes are zero in version 1. Separator key records are
packed backward from the payload end, with no gaps or overlaps. All free bytes
are zero. Child page IDs must refer to page 2 or later.

Separator keys are strictly increasing. A separator is the minimum key reachable
from its right child. Traversal chooses the first child for keys smaller than the
first separator; equality and larger values choose the corresponding right
child. Empty separator keys are invalid.

## Consequences

Internal and leaf pages use matching forward-slot/backward-record layouts.
Readers can binary-search separators without interpreting key types because M3.1
keys are already order-preserving byte strings. Structural corruption, invalid
children, and unordered separators are rejected before traversal continues.

The format spends 16 bytes per separator. Prefix compression and sibling links
for internal pages are deferred until measurements justify the complexity.

## Alternatives considered

- Fixed-width separator keys were rejected because composite keys are variable
  length.
- Storing one child in every slot without a header child was rejected because a
  node with N separators necessarily has N+1 children.
- Prefix compression was deferred because it complicates validation, splitting,
  and diagnostic output.
