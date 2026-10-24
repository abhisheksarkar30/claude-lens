[← INDEX](../INDEX.md)

# ADR 012: the provider is stamped on the request, not inferred from the model name

**Status:** Accepted

**Context:** GI#25 added a local model server (LM Studio) beside the cloud API, switchable from the
dashboard, with its calls priced as a projection and kept out of every real-cost sum
(`docs/planning/GI-25-local-llm-toggle-and-store-hardening.md`, §10). Billing mode decides which cost
column a figure lands in, so the question "was this call local?" has to be answered reliably.

**Decision:** The proxy reads the active provider once per request, resolves the upstream from it, and
stamps it on the capture (`CapturedCall.Provider`). The consumer derives `billing_mode='local'` and
`account='local'` from that stamp. The JSONL collector, which cannot see the upstream, classifies by
the configured `LocalModelPrefixes`, and on a merge the proxy side's identity wins.

**Rejected:**
- *Inferring local from the model name alone*: a local and a cloud model can legitimately share a name
  (Claude Code sends a Claude model id to LM Studio unchanged), so the split in invariant 5 would depend
  on a guess.
- *Reassigning the token-supplying side to the proxy row on every merge*: considered in the plan and
  dropped at implementation review (plan v9). It changes the existing completeness pick for a mismatch
  that needs a misconfigured prefix to occur, and the one order it would help loses nothing for good.

**Consequences:** One extra field threads from `captureState` to the row. A transcript row of a local call
is only recognised when `LocalModelPrefixes` is set, so with it unset a proxy-first merge of a local
call can end unpriced until the prefixes are configured and `clens reprice` runs. A request already in
flight keeps the provider it started on when the toggle moves.
