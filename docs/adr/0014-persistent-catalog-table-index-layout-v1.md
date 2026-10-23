# ADR 0014: Fix the version-1 catalog, table, and secondary-index layout

- Status: Accepted
- Date: 2026-10-23
- Owners: catalog and storage

## Context

M4 makes SQL schemas and rows durable. Catalog, table, and secondary-index
trees need deterministic keys and values so a manifest root can be reopened
without depending on Go map order or process-local identities.

## Decision

All three structures use the immutable B+ tree and the version-1 key and tuple
codecs fixed by ADRs 0009 and 0010.

Catalog entries use the composite key `(object-kind INT64, stable-id INT64)`.
Object kind `1` is a table and kind `2` is an index. Descriptor values are
version-1 tuples. A table descriptor stores its stable ID, name, schema version,
primary-key ordinal, and ordered column descriptors. An index descriptor stores
its stable ID, name, table ID, uniqueness flag, and ordered stable column IDs.

Table values are versioned row tuples. A table with a declared primary key uses
that scalar's canonical key encoding as its physical key. A table without one
uses a positive hidden `INT64` RowID. M4 allocates that RowID from the publishing
transaction ID because each SQL INSERT is one commit.

A secondary-index key begins with its indexed column values. A non-unique entry
appends the encoded physical table key as a `BYTES` component. A unique entry
omits that suffix when every indexed value is non-NULL, and stores the physical
table key as its value. If any indexed value is NULL, it uses the non-unique
form, matching SQL's allowance for multiple NULLs in a unique index.

## Consequences

Stable object and column IDs survive rename-capable future schema changes.
Manifest publication makes table and every affected index root visible
together; pages appended by a failed constraint check remain unreachable.
Changing any descriptor field order, object-kind value, table-key rule, or
secondary-index suffix rule requires a new format version and migration plan.

The transaction-derived hidden RowID is sufficient while M4 publishes each
INSERT separately. Multi-row writes introduced later must allocate distinct
RowIDs within a transaction without changing the encoded key type.

## Alternatives considered

- Catalog keys based on names were rejected because renames would change object
  identity and rewrite keys.
- Heap pages plus separate primary indexes were deferred because one table tree
  gives M4 a smaller, inspectable correctness surface.
- Treating NULL as an ordinary unique value was rejected because it would make
  a unique index allow only one NULL.
