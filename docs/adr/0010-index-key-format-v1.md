# ADR 0010: Use canonical order-preserving composite index keys

- Status: Accepted
- Date: 2026-10-23
- Owners: types and storage

## Context

B+ trees compare keys as byte strings. M3 therefore needs one canonical encoding
whose lexicographic byte order agrees with the scalar ordering in `types.Value`.
The encoding must preserve composite-key boundaries, support prefix scans, and
give NaN and signed zero the semantics fixed by ADR 0008.

## Decision

Version-1 keys begin with byte `1` and then contain zero or more typed
components. They do not store a component count, so the encoding of a composite
prefix is also a byte prefix of every longer key beginning with those components.

Each component begins with its `types.Kind` byte. `NULL` has no payload and its
tag sorts before every non-NULL tag. Index definitions ensure corresponding
non-NULL components have the same type; byte ordering across different non-NULL
types is not SQL comparison semantics.

- `BOOL` stores one byte, `0` or `1`.
- `INT64` flips the sign bit and stores the result as big-endian `uint64`.
- `FLOAT64` canonicalizes both zero signs to positive zero. Non-NaN IEEE bits
  are complemented for negative values and have their sign bit flipped for
  non-negative values, then are stored big-endian. Every NaN uses the reserved
  ordered payload `ff ff ff ff ff ff ff ff`, after positive infinity.
- `TEXT` and `BYTES` retain their bytes, replace every `00` with `00 ff`, and
  end with `00 00`. `TEXT` must be valid UTF-8.

Decoding rejects unknown tags, truncated fixed-width payloads, invalid Boolean
bytes, non-canonical float encodings, invalid escapes, invalid UTF-8, and values
or keys exceeding configured limits. Malformed persisted keys return a
corruption-class error and never panic.

## Consequences

`bytes.Compare` can be used directly by B+ tree pages for equality, lookup,
sorting, and range traversal. Composite prefixes can define index scan ranges.
The format is independent of native integer byte order and Go struct layout.

The type tag costs one byte per component and zero-byte escaping can expand
variable-width values. Descending order and non-binary collations require a
future version or an explicitly different encoding.

## Alternatives considered

- Little-endian numeric payloads were rejected because lexical and numeric
  order disagree.
- Length-prefixed variable values were rejected because lexical order would be
  dominated by length instead of content.
- Storing a component count before the key was rejected because it breaks the
  useful byte-prefix relationship for composite-key prefixes.
