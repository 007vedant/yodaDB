# ADR 0006: Fold unquoted SQL identifiers to lowercase ASCII

- Status: Accepted
- Date: 2026-08-05
- Owners: SQL

## Context

Lexing, catalog lookup, duplicate detection, and diagnostics require one stable
rule for identifier equality. Locale-dependent behavior would make schemas
non-portable.

## Decision

Unquoted identifiers are case-insensitive over ASCII and canonicalized to
lowercase before binding and catalog storage. Quoted identifiers preserve their
exact UTF-8 spelling and compare by exact bytes. Keywords are recognized
case-insensitively only when unquoted.

The AST retains original source spans and spelling for diagnostics separately
from canonical lookup names. Non-ASCII code points in unquoted identifiers are
preserved and compared exactly; Unicode normalization and case folding are not
performed in v1.

## Consequences

`Users`, `USERS`, and `users` name the same unquoted object. `"Users"` is a
distinct exact name. Behavior is deterministic across locales, though users who
need non-ASCII case behavior must quote and match identifiers exactly.

## Alternatives considered

- Locale-aware folding was rejected as non-portable.
- Full Unicode case folding was deferred because normalization and versioning
  rules require a larger compatibility policy.
- Preserving unquoted case while comparing insensitively was rejected because it
  complicates stable catalog display and collision handling.
