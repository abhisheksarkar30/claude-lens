# Bead br-GI-1-10: Analyzer T1 response, thinking and billing rules

**Plan Reference**: `docs/planning/GI-1-claude-lens-v1.md` — §Analyzer rule catalogue (T1), §Billing model, tests 12b, 21

- **Bead ID**: br-GI-1-10
- **Priority**: P0 (critical)
- **Original Estimate**: 3h
- **Dependencies**: br-GI-1-09
- **Blocks**: br-GI-1-17

## Description

The remaining **per-event** T1 rules, added to br-GI-1-09's rule table in `internal/analyze`. No
pipeline change: this is table entries, their descriptions in `kinds.go`, and tests.

| Kind | Sev | Fires when |
|---|---|---|
| `thinking_budget_rejected` | error | `thinking.budget_tokens` on a model that now returns a 400 (Fable 5 / 5.1, Opus 5 / 4.8 / 4.7, Sonnet 5). The direct analogue of deepseek-lens's `budget_tokens_ignored`, except here it fails loudly. |
| `thinking_display_omitted` | info | Thinking was on and **billed**, but `thinking.display` defaulted to `omitted` on the newest models — a silent change from the 4.6 generation, where it was `summarized`. You are paying for reasoning you are not receiving. |
| `max_tokens_truncation` | warn | `stop_reason: max_tokens` — the turn was cut off mid-thought and will be retried at more cost. |
| `refusal` | warn | `stop_reason: refusal`. `stop_details.category` is read **only** when `stop_reason == "refusal"`, as the API requires (br-GI-1-05 enforces the read; this rule reports it). |
| `stream_incomplete` | error | The SSE stream ended without `message_stop` (br-GI-1-04 flags it). |
| `rate_limited` | warn | HTTP 429, with `retry-after` and any `anthropic-ratelimit-*` remaining/reset values recorded. |
| `overloaded` | error | HTTP 529. |
| `upstream_error` | error | An error object inside a 200 body, or a transport failure. Carried over verbatim — one kind for both triggers, because it means the same thing to the reader. |
| `auth_kind_anomaly` | warn | An `admin` credential used against the Messages API, **or** a subscription account observed sending an `api_key` credential — the call is billed to a different model than the account it was attributed to. |
| `api_equivalent_cost` | info | On a **subscription** row: what this call would have cost at API rates. Labelled hypothetical, **never** added to a billed total. Its `detail` carries the figure computed by br-GI-1-07. |

**Not in this bead** — these T1 kinds are emitted by other components and are declared in
br-GI-1-09's `kinds.go` but raised elsewhere, because they are not per-event:

- `analyzer_panic` — the consumer's recover path (br-GI-1-08).
- `source_mismatch` — the store merge path (br-GI-1-06/11).
- `quota_window_approaching` — the quota surface (br-GI-1-12).
- `cost_drift` — the reconcile surface (br-GI-1-13).

**Billing discipline this bead must not break.** `api_equivalent_cost` fires **only** on a
`subscription` row, and its detail must render the figure as hypothetical API-equivalent value, never
as dollars paid. No rule in this bead may write or imply a merged total across billing models
(invariant 5).

## Rationale

These are the findings that turn a raw row into an explanation: a truncated turn that will be retried,
a thinking budget that now 400s, reasoning paid for but not received, a credential used against the
wrong plane. Every one is grounded in documented API behaviour; none is speculative.

## Outcome Definition

- `go test ./internal/analyze/... -race` passes.
- Each rule has a positive and a negative test.
- `api_equivalent_cost` fires only on a subscription row and never contributes to `cost_usd`.
- `rate_limited` records `retry-after` and the `anthropic-ratelimit-*` values when present.
- `upstream_error` fires for both an error object in a 200 body and a transport failure, as one kind.
- A clean event raises nothing.
- Every new kind is declared in `kinds.go` with a description that renders in a markdown table cell.

## Test Specifications

- Unit Tests (`internal/analyze/rules_test.go`, additions):
  - `thinking.budget_tokens` on Opus 5 → `thinking_budget_rejected`; on Opus 4.6 (where it still works)
    → **no** warning.
  - Thinking on + billed + `display` omitted → `thinking_display_omitted`; `display: summarized` → none.
  - `stop_reason: max_tokens` → `max_tokens_truncation`; `end_turn` → none.
  - `stop_reason: refusal` with a category → `refusal` naming the category; a non-refusal with a
    `stop_details.category` present → no `refusal` warning.
  - Incomplete stream → `stream_incomplete`; a stream with `message_stop` → none.
  - Status 429 with `retry-after: 30` → `rate_limited` carrying 30 and any rate-limit headers.
  - Status 529 → `overloaded`.
  - Error object in a 200 body → `upstream_error`; transport failure → `upstream_error` (same kind).
  - `auth_kind=admin` against `/v1/messages` → `auth_kind_anomaly`; `auth_kind=api_key` on a
    subscription account → `auth_kind_anomaly`; a matching kind → none.
  - **Test 12b (rule half)**: a subscription row with a computed API-equivalent → `api_equivalent_cost`
    at info severity, and the row's `cost_usd` stays NULL; an API row → **no** `api_equivalent_cost`.
  - Multi-rule: a request that is both truncated and rate-limited → two distinct kinds, no duplicates.
- Integration Tests (br-GI-1-11 owns test 21):
  - Two calls with byte-identical bodies but distinct response `request-id`s (a 429 then a 200) →
    two rows, two findings; the `rate_limited` signal is preserved (test 21).
- E2E: none.

## Files to Touch

- `internal/analyze/rules.go` (modify — the rules above)
- `internal/analyze/kinds.go` (modify — descriptions for the new kinds)
- `internal/analyze/rules_test.go` (modify)
