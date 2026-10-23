# ADR 0013: Fix the version-1 commit-manifest format

- Status: Accepted
- Date: 2026-10-23
- Owners: storage and transactions

## Context

M4 needs one immutable record that identifies the catalog, table, and index
roots belonging to a logical commit. The record must decode without catalog or
SQL dependencies and must be deterministic so recovery can validate it before
opening higher layers.

## Decision

A manifest is stored as a `TypeManifest` common-page payload. The common page's
CRC-64 is the manifest checksum. The payload begins with this 64-byte header:

| Offset | Size | Field |
|---:|---:|---|
| 0 | 4 | magic `MNFT` |
| 4 | 2 | manifest version, `1` |
| 6 | 2 | flags, zero |
| 8 | 8 | transaction ID |
| 16 | 8 | parent transaction ID, zero for the first commit |
| 24 | 8 | signed Unix nanosecond commit timestamp |
| 32 | 8 | catalog B+ tree root, zero means empty |
| 40 | 8 | oldest retained transaction ID |
| 48 | 2 | table-root count |
| 50 | 2 | index-root count |
| 52 | 4 | reserved, zero |
| 56 | 8 | logical-change root, zero means absent |

Table-root entries follow, then index-root entries. Every entry is a stable
object ID `u64` followed by a root page ID `u64`. IDs are nonzero and strictly
increasing within each map. Root zero means an empty tree; nonzero roots must be
page 2 or later. The complete payload length is derived exactly from the two
counts, and unused trailing bytes are not permitted.

Transaction IDs are nonzero. A nonzero parent must be lower than its child, and
the retention boundary cannot exceed the manifest transaction ID. Timestamps
are informational and are not used for ordering.

## Consequences

The version-1 page-size limit permits at most 249 total table and index root
entries in one manifest. Larger databases will require an indirect root-map
tree and a new manifest version. Encoding sorts map keys, so Go map iteration
cannot change persisted bytes.

M4 may publish manifests using a basic single-process path to demonstrate
persistence. M5 remains responsible for proving the double-metapage commit
protocol under crashes and torn writes.

## Alternatives considered

- Encoding root maps as tuples was rejected because recovery would then depend
  on the tuple layer before it can identify database roots.
- JSON was rejected because canonical encoding, bounds, and corruption offsets
  would be less explicit.
- One manifest page per object was deferred until the inline map limit becomes
  a demonstrated constraint.
