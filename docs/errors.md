# Error conventions

Publicly observable errors use categories from `dberrors`. Callers inspect a
category with `errors.Is(err, dberrors.ErrCorruption)` and recover structured
context with `errors.As`.

Subsystems retain their detailed error types. They wrap underlying causes rather
than flattening them, so operating-system errors, cancellation, and other useful
causes remain discoverable. An operation name is a short, stable subsystem verb
such as `pager.read`; it is context for people and logs, not a programmatic API.

Rules for new errors:

- Preserve causes with `%w` or `dberrors.Wrap`.
- Select a category based on the failed invariant, not the layer reporting it.
- Never encode control flow by matching error strings.
- Syntax errors add source spans in the SQL-owned detailed type.
- Corruption errors add file, page, page type, and slot/key context where known.
- Do not include raw corrupt slices or bound values in default messages.
- Cancellation remains discoverable as both `dberrors.ErrCancelled` and its
  underlying context error.
- Unsupported behavior returns `dberrors.ErrUnsupported`; it is never silently
  approximated.
