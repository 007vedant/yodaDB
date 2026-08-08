# ADR 0002: Start with 4 KiB little-endian pages

- Status: Accepted
- Date: 2026-08-05
- Owners: storage

## Context

The page codec needs a portable initial page size and byte order. The format must
be able to reject unsupported variants without assuming Go struct layout.

## Decision

New v1 databases use 4096-byte pages. The format header records the page size so
a future format can support other powers of two. All fixed-width persisted
integers use little-endian byte order. Variable-width integers use an explicitly
versioned unsigned varint encoding where their structure calls for it.

Persisted structures are encoded field by field and never by serializing Go
struct memory. Page IDs are zero-based and byte offsets use checked arithmetic:
`pageID * pageSize`.

Exact offsets and size limits will be fixed in a page-format ADR before M2.3.

## Consequences

Files are portable across CPU architectures. Supporting another page size is an
open-time capability decision, not an inference. Tests must cover overflow in
page-offset calculations.

## Alternatives considered

- Native byte order and Go struct serialization were rejected as non-portable.
- A permanently implicit 4 KiB size was rejected because it prevents explicit
  format evolution.
- Larger initial pages were deferred until workloads demonstrate a need.
