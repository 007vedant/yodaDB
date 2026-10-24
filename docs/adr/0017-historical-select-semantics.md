# ADR 0017: Bind historical SELECT to an exact committed transaction

- Status: Accepted
- Date: 2026-10-24
- Owners: SQL and transactions

## Context

M6.4 exposes retained database history through SQL. A historical query must use
one internally consistent catalog, table-root, and index-root set; resolving
names against today's schema while reading old table roots would be incorrect.
The syntax also needs one unambiguous location in the initial SQL grammar.

## Decision

The supported form is:

```sql
SELECT ... FROM table AS OF TRANSACTION <uint64> [WHERE ...]
```

`EXPLAIN SELECT` accepts the same clause. The transaction ID is an unsigned
integer literal and identifies one exact retained committed manifest. Zero,
future IDs, and IDs outside retained history fail rather than silently choosing
another snapshot.

The database resolves the manifest before binding. It loads that manifest's
catalog and uses it for table/column resolution and planning, then executes with
the same manifest's table and index roots. This preserves atomic schema and data
visibility. In-memory databases reject the clause because they have no durable
manifest identity.

A `ReadTx` is already bound to one snapshot. It accepts an explicit clause only
when the requested ID equals its own `TxnID`; requesting another ID fails.
Historical DML and DDL are not supported.

## Consequences

Queries can reproduce both old values and old access plans. Objects created
after the selected transaction are unknown at bind time, and indexes created
later cannot influence the historical plan.

The clause currently accepts a literal rather than a parameter or timestamp.
Retention-specific diagnostics are refined in M6.5, but absence is always an
error and never a fallback to the nearest transaction.

## Alternatives considered

- Binding against the current catalog was rejected because stable object IDs do
  not make later schema definitions valid in an earlier snapshot.
- Selecting the nearest transaction at or before an ID was rejected because it
  hides retention gaps and makes reproducibility less explicit.
- A statement prefix was rejected in favor of keeping the snapshot qualifier
  adjacent to the table source it governs.
