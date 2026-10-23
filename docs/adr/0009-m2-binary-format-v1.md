# ADR 0009: Fix the M2 binary format version 1

- Status: Accepted
- Date: 2026-08-15
- Owners: storage and types

Leaf slot flags are extended for M3 tombstones by ADR 0012.

## Context

M2 writes yodaDB's first persisted bytes. The conceptual layouts in `PLAN.md`
and ADRs 0002, 0003, and 0005 must become exact before codecs and fixtures can
be implemented. Ambiguous padding, unused bytes, and decoder limits would make
corruption detection and future compatibility unreliable.

## Decision

All fixed-width integers are little-endian. All offsets below are byte offsets
from the beginning of their containing structure. Version 1 supports only
4096-byte pages.

### Common page

Every page is exactly 4096 bytes. Its 36-byte header is:

| Offset | Size | Field |
|---:|---:|---|
| 0 | 4 | magic, ASCII `YODA` |
| 4 | 2 | format version, `1` |
| 6 | 1 | page type |
| 7 | 1 | flags, zero unless defined by that page type |
| 8 | 8 | page ID |
| 16 | 8 | generation |
| 24 | 2 | payload length, at most 4060 |
| 26 | 2 | reserved, zero |
| 28 | 8 | CRC-64/ECMA checksum |
| 36 | variable | payload |
| remainder | variable | unused bytes, all zero |

The checksum covers bytes 0 through 35 with bytes 28 through 35 zeroed, followed
by exactly the declared payload. Unused bytes are excluded from the checksum but
must decode as zero. This ensures mutations anywhere in a canonical page are
either checksummed or rejected. Page-type values are: 1 Meta, 2 Manifest, 3
BTreeInternal, 4 BTreeLeaf, and 5 Catalog. Zero and unknown values are invalid in
format version 1.

### Tuple

A tuple is self-describing in M2 so malformed-input tests and diagnostics do not
need a catalog. Its fields are:

1. tuple version byte, `1`;
2. schema version as unsigned varint;
3. column count as unsigned varint;
4. null bitmap of `ceil(columnCount / 8)` bytes, least-significant bit first;
5. one type-tag byte per column;
6. one unsigned-varint cumulative payload end offset per column;
7. concatenated non-null column payloads.

NULL columns use the NULL type tag and repeat the previous end offset. BOOL uses
one byte, 0 or 1. INT64 and FLOAT64 use eight bytes; FLOAT64 stores IEEE 754 bits.
TEXT is valid UTF-8 bytes and BYTES is arbitrary bytes. End offsets must be
monotonic and the final offset must equal the payload length.

Default decoder limits are 1024 columns, 1 MiB per variable-width value, and
4 MiB per tuple. Callers may lower these limits. Values exceeding them return a
resource-limit error that matches `types.ErrValueTooLarge` where applicable.

### Slotted B+ tree leaf payload

The common-page payload begins with a 16-byte leaf header:

| Offset | Size | Field |
|---:|---:|---|
| 0 | 1 | leaf format version, `1` |
| 1 | 1 | flags, zero |
| 2 | 2 | slot count |
| 4 | 2 | free-space start |
| 6 | 2 | free-space end |
| 8 | 8 | right-sibling page ID, zero means none in M2 |

Eight-byte slots grow forward from offset 16. A slot contains record offset
`u16`, record length `u16`, flags `u16`, and reserved `u16`; flags and reserved
are zero in M2. Records grow backward from the payload end and contain key length
as an unsigned varint, key bytes, then value bytes. Slots define logical order.
All free bytes between `freeStart` and `freeEnd` are zero.

M2 builders may rearrange records and remove slots before encoding. Once wrapped
in a committed page, the page-immutability rule applies.

### Metapage payload

Pages 0 and 1 contain a 56-byte payload:

| Offset | Size | Field |
|---:|---:|---|
| 0 | 4 | metapage magic, ASCII `META` |
| 4 | 2 | metapage version, `1` |
| 6 | 2 | reserved, zero |
| 8 | 16 | random database file ID |
| 24 | 4 | page size, `4096` |
| 28 | 4 | reserved, zero |
| 32 | 8 | latest transaction ID, zero for M2 bootstrap |
| 40 | 8 | latest manifest page ID, zero for M2 bootstrap |
| 48 | 8 | oldest retained transaction ID, zero for M2 bootstrap |

The common page-header generation orders metapage candidates. A new empty file
writes generation 1 to page 0 and generation 0 to page 1, synchronizes both, and
selects page 0. M2 open validates both independently and may fall back to the
older valid copy. Manifest/root validation becomes mandatory when manifests are
introduced in M4/M5.

## Consequences

M2 codecs can reject non-canonical bytes and fixtures can detect format drift.
Self-describing tuples cost type tags and offset varints but remain easy to fuzz
and inspect. Zero-filled unused page bytes cost negligible encoding work and make
whole-page mutation tests deterministic.

Version 1 has no overflow pages. A tuple may satisfy tuple limits yet still be
too large for a leaf page; the leaf builder returns a resource-limit error.

## Alternatives considered

- Go struct serialization was rejected as padding- and architecture-dependent.
- Checksumming the entire page was rejected because payload length should remain
  authoritative; canonical zero validation gives equivalent mutation detection.
- Schema-dependent tuple decoding was deferred because it would couple the M2
  malformed-byte decoder to catalog availability.
- Fixed-width tuple offsets were rejected because varints keep small rows compact.
