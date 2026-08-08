# ADR 0007: Treat the native API as unstable before v1

- Status: Accepted
- Date: 2026-08-05
- Owners: public API

## Context

The native Go API must evolve while storage and transaction semantics are being
learned. Claiming compatibility too early would either freeze mistakes or cause
surprising breaks.

## Decision

The module follows semantic versioning once releases are tagged. Before v1,
minor releases may make documented breaking API changes. Such changes require
release notes and migration guidance; they are never hidden in patch releases.

The native `db` API owns database semantics. The later `database/sql` driver is
an adapter and does not force limitations into the native API. Exported names
must have Go documentation, and observable error categories use the stable
`dberrors` contract introduced in M0.4.

Persisted-format compatibility is independent of Go API stability. Any format
change still requires versioning, fixtures, and an ADR even before v1.

## Consequences

The project can correct early API design while making breaks explicit. Users of
pre-v1 versions should pin versions. On-disk safety cannot be waived by the
pre-v1 label.

## Alternatives considered

- Immediate API stability was rejected before a complete vertical slice exists.
- Deferring all compatibility policy until v1 was rejected because early users
  still need predictable release practices.
