[← INDEX](../INDEX.md)

# ADR 001: The billing split is carried by column, not by convention

**Status:** Accepted

**Context:** The tool reports cost for two billing models that mean fundamentally different things.
A `subscription` figure is *hypothetical* — what your tokens would have cost at API rates, useful
for comparison, but no money changed hands. An `api` figure is *real* — it is what you were billed.
Summing them produces a number that is neither, and it is a mistake a query makes silently.

The rejected design was one unified table with a single `cost_usd` column and a `billing_mode`
discriminator beside it. That is the conventional shape, and it is one careless `SUM(cost_usd)`
away from the error — with or without a JOIN, since the discriminator is just another column a
`WHERE` can forget.

**Decision:** The separation is structural. A `subscription` figure is written to
`api_equivalent_cost_usd` and `cost_usd` is left **NULL**; an `api` figure is written to `cost_usd`.
They are never in the same column, so no `SUM` over either column can merge them.

The rule extends past storage:

- `sessions` mirrors it with `total_cost_usd` and `total_api_equivalent_cost_usd`.
- The dashboard header renders **two labelled totals** — `api` and `sub` — never one sum.
- An unpriced row leaves both columns NULL and carries `cost_source='unpriced'`. It is never `$0.00`,
  which reads as "this was free".
- The quota side has the same shape: no configured limit renders `unconfigured`, never a percentage
  of an invented ceiling.

**Consequences:**

- Every cost-reading query must decide which model it is asking about, because there is no single
  column that holds both. This is the intended friction.
- Adding a third billing model means adding a column, not a discriminator value.
- `TestBillingModeInvariants` and `TestSessionCostSplit` pin the invariant, so a change that
  collapses the split fails a test rather than passing review.

See [../cost-and-quota.md](../cost-and-quota.md) and [../storage-schema.md](../storage-schema.md).
