# ADR 0008: Give FLOAT64 a deterministic total order

- Status: Accepted
- Date: 2026-08-09
- Owners: types and SQL

## Context

IEEE 754 NaN is not equal to itself and ordinary floating-point comparisons do
not provide a total order. Database indexes, uniqueness checks, sorting, and SQL
expressions must not disagree about the same stored values.

## Decision

All `FLOAT64` NaN bit patterns belong to one database value class. NaN compares
equal to NaN and sorts after every non-NaN float, including positive infinity.
Negative and positive zero compare equal. All other floats use numeric order.

This database equality is used consistently by SQL equality, unique constraints,
and future key encoding. Persisted tuple encoding may retain the original float
bits, but a future order-preserving key codec must canonicalize NaN and zero in a
way that implements this decision.

## Consequences

`FLOAT64` has a deterministic total order and unique indexes can enforce one
unambiguous rule. The semantics differ from raw Go and IEEE comparisons where
`NaN != NaN`, so expression evaluation must use `types.Value` operations rather
than native operators.

## Alternatives considered

- IEEE unordered NaN semantics were rejected because they do not provide the
  total order needed by indexes.
- Ordering NaNs by payload bits was rejected because payload identity is not a
  useful SQL-level distinction and would complicate canonical key encoding.
