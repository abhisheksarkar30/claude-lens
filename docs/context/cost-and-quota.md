# Cost and quota model

Two rules generate most of the code in `internal/pricing`, `internal/quota`,
and `internal/reconcile`, and most of the tests:

1. **Two billing models are never summed.** Not in SQL, not in Go, not in the
   dashboard's totals.
2. **An invented number is worse than no number.** `$0.00` reads as "this was
   free"; `unpriced` tells you to go configure something.

The design is in `docs/planning/GI-1-claude-lens-v1.md` §Billing model. This
file is the map to the code that implements it.

## The seven shapes, and the column that carries them

| Plan | `billing_mode` | Cost shown | Quota shown |
|---|---|---|---|
| Free | `subscription` | API-equivalent value, labelled hypothetical | snapshot % + calibrated burn |
| Pro / Max 5x / Max 20x | `subscription` | API-equivalent value | snapshot % + rolling 5h/7d burn |
| Team | `subscription` | API-equivalent value | snapshot % + shared-seat context |
| Enterprise | `subscription` | API-equivalent value | snapshot % + configured spend limit |
| Pay-as-you-go | `api` | real billed dollars (source D) and computed dollars in `cost_usd` (sources A/B) | Admin rate-limit reports |

The separation is carried in the schema, not in a convention: a
`subscription` figure is written to `api_equivalent_cost_usd` and `cost_usd`
is left NULL; an `api` figure is written to `cost_usd`. Both live in
`internal/store`, enforced by an invariant test — see
[storage-schema.md](storage-schema.md).

`billing_mode` comes from the **configured account**, not from the credential
shape alone. A mixed account (a Pro subscription *and* a separate API key) is
two accounts, rendered side by side and never summed.

## Auth-kind classification

`internal/proxy` classifies a call from the *shape* of its credential and
stores only the classification — never the credential:

| Observed | `auth_kind` | Meaning |
|---|---|---|
| `Authorization: Bearer` with an OAuth token | `oauth` | subscription |
| `x-api-key` with a standard key | `api_key` | pay-as-you-go |
| `x-api-key` with an admin key | `admin` | admin credential on the data plane — a misconfiguration |
| cloud-provider signature headers | `cloud` | Bedrock/Vertex/Foundry — classified and stored, **not priced** in v1 |
| none / unrecognised | `unknown` | recorded as unknown, never guessed |

`analyze` fires `auth_kind_anomaly` when the pair disagrees with the account's
billing mode in either direction (`internal/analyze/rules.go`).

## Pricing

`internal/pricing/table.go` is the bundled rate table; each row carries a
`Source` of `shipped` (cited to a doc line in the comment) or `provisional`
(bundled but unverified). A user override is written to a separate file and
carries `user`.

`Cost(model, usage)` returns `(nil, "unpriced")` for a model with no rate —
never a numeric zero a caller could store as `$0.00`. The full vocabulary of
`cost_source` is `shipped | provisional | user | approximate:<reason> |
unpriced`; `approximate:` marks a rate applied where only a flat cache count
was available (e.g. `approximate:cache_ttl_unknown`), so the caveat survives
into the stored row instead of being rounded away.

## Quota

`internal/quota` computes the token burn inside a rolling window from events,
and cross-checks it against the polled snapshots from source C.

- **Burn** sums *every* token column over the half-open interval
  `[now-duration, now)` — input + output + both cache-write tiers + cache-read.
  Using `input_tokens` alone would under-count every cached call.
- **`CrossCheck`** pairs each snapshot with the burn computed at *that
  snapshot's own instant*, not at `now`, so an old snapshot is checked against
  the window as it stood then.
- **`Limits` is empty by default.** `Projection.Configured` is false with no
  limit, and the caller must render `unconfigured` — never read
  `UtilizationPct` as a real 0%.
- A limit becomes known one of two ways: the user configures it, or
  `internal/quota/calibration.go` *learns* it — a snapshot window that reached
  100% makes the burn recorded at that moment an empirical observation of the
  limit, offered as a **candidate** for confirmation, never applied silently.

`quota_window_approaching` fires when the current rate would exhaust the
window before it resets. The projection is a linear extrapolation of
tokens-per-second across the window (marked `ponytail:` in the source) — good
enough to answer "should the user worry", not a simulation of which requests
age out when.

## Reconciliation

`internal/reconcile` compares this tool's computed cost against the Admin
cost report, per `(day, model)`. Divergence past an absolute-dollar
threshold raises `cost_drift` — the signal that the price table is stale.

`source_mismatch` is the other half: the same `request_id` arriving from two
sources with disagreeing token counts. It is raised by the store's merge, not
by an analyze rule, and is one of the four kinds in `nonAnalyzeKinds`
(`internal/analyze/kinds.go`) — the list a README check uses to expect a kind
with no rule in that package. See the README's warning-kind table, which
`internal/analyze/readme_test.go` pins against `AllKinds()`.
