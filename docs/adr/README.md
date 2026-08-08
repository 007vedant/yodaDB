# Architecture decision records

Architecture decision records (ADRs) capture decisions that constrain yodaDB's
public behavior, package architecture, or persisted format. `PLAN.md` remains the
architectural source of truth; accepted ADRs refine decisions that the plan
explicitly leaves open.

## Process

1. Copy [0000-template.md](./0000-template.md) to the next zero-padded number.
2. Use a short, decision-oriented title and one of: Proposed, Accepted,
   Superseded, or Rejected.
3. Describe the context, decision, consequences, and alternatives.
4. Persisted-format or invariant changes must update `PLAN.md` in the same
   change and include compatibility fixtures when a format already exists.
5. Superseding an ADR leaves the old record in place and links both records.

ADRs are reviewed before implementation of the decision they govern. They are
not retroactive justifications for an already-merged format change.
