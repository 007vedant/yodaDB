# ADR 0025: Store full row images in an ordered chunked change tree

- Status: Accepted
- Date: 2026-10-25
- Owners: changefeed, transactions, and storage

## Context

M8 needs durable logical changes ordered by committing transaction and by the
original statement/row order within that transaction. The manifest already has
a `ChangeRoot`, but its target format and the before/after image policy were
deliberately undecided. A single update can contain two row images and may not
fit in one B+ tree leaf entry even when each table tuple fits independently.

## Decision

Each committed DML mutation produces one logical change:

- insert stores the new physical key and full `after` row image;
- update stores old and new physical keys plus full `before` and `after` images;
- delete stores the old physical key and full `before` row image.

Each image uses the existing version-1 tuple encoding, including the table
schema version under which the mutation executed. Stable table IDs and physical
keys are recorded; table/index page rewrites and SQL text are not.

The manifest's `ChangeRoot` points to an immutable B+ tree. Tree keys are 16
bytes in big-endian order: `TxnID uint64`, zero-based transaction sequence
`uint32`, and zero-based chunk number `uint32`. This ordering is transaction,
then statement/row sequence, then chunk. Sequences must be contiguous within a
transaction, and chunks must be contiguous within a record.

The reassembled record value has this version-1 layout:

| Offset | Size | Field |
|---:|---:|---|
| 0 | 4 | magic `CHNG` |
| 4 | 2 | format version (`1`) |
| 6 | 1 | kind (`1` insert, `2` update, `3` delete) |
| 7 | 1 | image flags (`before=1`, `after=2`) |
| 8 | 8 | stable table ID, little-endian |
| 16 | 4 | old-key length, little-endian |
| 20 | 4 | new-key length, little-endian |
| 24 | 4 | before-image length, little-endian |
| 28 | 4 | after-image length, little-endian |
| 32 | variable | old key, new key, before tuple, after tuple |

Encoded records are divided into chunks of at most 4000 bytes before insertion
into the tree. Chunking avoids imposing a stricter row-size limit and uses only
the existing B+ tree page format. Decoding caps a reassembled record at 16 MiB,
validates ordering, lengths, flags, tuple encodings, and schema versions, and
reports malformed durable bytes as corruption.

Changes are built from the transaction's exact mutation log only after
validation/rebase chooses its final `TxnID` and stable catalog IDs. Change-tree
pages are appended before manifest publication; therefore the data roots,
catalog root, and `ChangeRoot` become visible atomically. Failed commits leave
only unreachable copy-on-write pages.

Retention initially follows manifest history. M8.1 does not physically prune
older change entries; M8.2 defines cursor/retention behavior and M9 compaction
reclaims unreachable history.

## Consequences

Consumers can reconstruct table state and interpret primary-key changes without
consulting overwritten pages. Full update images cost additional space but keep
changefeed semantics inspectable and independent of later row contents.
Chunking adds grouping validation to readers but supports the existing tuple
size limits without a new overflow-page type.

The record and key layouts are now persisted format contracts. Future fields
require a new record version or reserved-compatible extension.

## Alternatives considered

- After-images only were rejected because deletes and audit consumers need the
  prior row, and primary-key updates need an explicit old identity.
- Before-images only were rejected because state reconstruction would require
  rereading table history.
- One tree entry per logical change was rejected because an update containing
  two legal tuples may exceed one leaf page.
- Recording physical page changes was rejected because copy-on-write rewrites
  are implementation details and do not represent user-visible mutations.
