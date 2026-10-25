# ADR 0024: Make transaction retry explicit and callback-scoped

- Status: Accepted
- Date: 2026-10-25
- Owners: transactions and public database API

## Context

Optimistic writers can fail at commit after their work has executed. Callers
need structured diagnostics to distinguish logical write conflicts from other
errors, and many callers want a bounded way to rerun a complete transaction.

Automatically retrying one SQL statement is unsafe. A statement can be part of
a larger transaction, and retrying only that statement against a newer
snapshot can change predicates and break the caller's intended unit of work.
Retrying user code can also repeat external side effects, so it must be an
explicit choice.

## Decision

Commit-time logical conflicts return `*db.ConflictError`. The error continues
to match `dberrors.ErrConflict` and exposes:

- whether validation history, overlapping writes, or rebase caused the abort;
- the transaction's base ID and the latest ID used for validation;
- exact conflicting logical resources when validation identified them.

`DB.RetryTransaction` is an opt-in helper. It begins a fresh native transaction,
runs a callback, and commits it. Only a typed commit conflict causes the whole
callback to run again. Callback errors, constraint failures, cancellation, I/O
errors, and untyped conflicts return immediately. `MaxAttempts` bounds total
executions; zero selects a documented default of three. The helper performs no
sleep or backoff, leaving scheduling policy to callers and future contention
measurements.

The callback must use the supplied transaction and must not commit or roll it
back itself. Because the callback may run more than once, callers remain
responsible for making external side effects idempotent or moving them after a
successful return.

No persisted format changes.

## Consequences

Applications can inspect conflicts with `errors.As`, retain compatibility with
`errors.Is(err, dberrors.ErrConflict)`, and choose either manual retry or a
small bounded helper. Retry never hides non-conflict failures and never changes
the atomic boundary from a whole transaction to an individual statement.

The helper deliberately has no exponential backoff or jitter. M7.8 contention
benchmarks can justify such policy later without changing conflict semantics.

## Alternatives considered

- Retrying every `ErrConflict` was rejected because that category also covers
  file locks and API-state conflicts that a fresh transaction cannot repair.
- Statement-level retry was rejected because it can violate transaction intent
  and re-evaluate predicates under a different snapshot.
- Unbounded retry was rejected because hot keys and cancellation bugs could
  otherwise loop forever.
- Built-in backoff was deferred until contention benchmarks establish an
  appropriate policy.
