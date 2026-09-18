# Bead br-GI-1-08: Consumer pipeline (sink → parse → session → account → cost → insert → analyze)

**Plan Reference**: `docs/planning/GI-1-claude-lens-v1.md` — §Cold-path pipeline, §Invariants 3/6, §Cross-source identity, tests 6, 12a

- **Bead ID**: br-GI-1-08
- **Priority**: P0 (critical)
- **Original Estimate**: 4h
- **Dependencies**: br-GI-1-03, br-GI-1-05, br-GI-1-06, br-GI-1-07
- **Blocks**: br-GI-1-09, br-GI-1-11, br-GI-1-16, br-GI-1-17

## Description

The cold-path bridge — one goroutine that drains the sink and writes rows. The plan's order is
**fixed**; later beads plug into named seams rather than rewriting the pipeline.

`Consumer.Run(ctx) error` loops over `sink.Drain(ctx)` and, per call, in order:

1. `parse.ExtractMeta(call.ReqBody, call.ReqHeaders)` (br-GI-1-05).
2. `parse.ExtractUsage(call.RespBody, call.RespHeaders.Get("Content-Type"))` (br-GI-1-05).
3. Build the `store.Event` from status/timings + `Meta` + `Usage` + `call.RequestID` +
   `call.CaptureComplete`.
4. **Resolve session** — an injected `SessionResolver` sets `ev.SessionID` **before** insert.
5. **Resolve account / `billing_mode`** — from the configured accounts (br-GI-1-01) matched with
   `call.AuthKind` (br-GI-1-03). `billing_mode` is derived from the **account**, not the credential
   alone: a mixed account is two accounts. Set `ev.Account` and `ev.BillingMode`.
6. **Compute cost** — the pre-insert cost step (br-GI-1-07) sets `CostUSD` + `ApiEquivalentCostUSD` +
   `CostSource` **before** insert. Subscription rows write `ApiEquivalentCostUSD` and leave `CostUSD`
   NULL; API rows write `CostUSD`; unpriced rows leave both NULL (invariant 5, br-GI-1-06).
7. `store.InsertEvent(ctx, ev)` → `id`. A colliding `request_id` **merges** in the store (br-GI-1-06).
8. Run the analyzer seam and attach by id — `store.UpsertWarnings` (keyed `(event_id, kind)`), **inside
   the insert's transaction**, because the session `warning_count` re-derivation reads `warnings`.
   In this bead the registered analyzer set is **empty**; br-GI-1-09/10 add implementations and
   br-GI-1-17 wires them. The interface does not change.
9. **Re-derive the owning session's totals** — tokens/cost from `events`, `warning_count` from
   `warnings` — in the **same transaction as the insert/merge** (br-GI-1-06's `reconcileSession`).
   A merge can rewrite token columns, so this is a recomputation that wins over the incremental fold,
   never an increment.

**The seam interfaces** (this package defines them; `internal/analyze` satisfies them structurally,
so `consumer` never imports `analyze` — invariant 2's direction holds and br-GI-1-08 does not depend
on br-GI-1-09):

```go
type Analyzer interface {
    Analyze(meta parse.Meta, usage parse.Usage, ev *store.Event) []store.Warning
}
type SessionResolver interface {
    Resolve(meta parse.Meta, now time.Time) (sessionID string)
}
type SessionAggregator interface {
    RecordCall(ctx context.Context, sessionID string, ev *store.Event, warningCount int) error
}
```

`Resolver` is deliberately read-only: `warning_count` is final only after the analyzers have run, so
persisting the aggregates belongs on the other side of the insert and arrives through
`SetSessionAggregator`. A Consumer without an aggregator behaves exactly as it did before it was
installed.

**Error containment is the point of this bead** (invariant 6, fail open). Any per-call failure — bad
JSON, store error, analyzer panic — is logged to stderr once and the loop continues. A panic in an
analyzer is recovered with `defer recover()`, recorded as an `analyzer_panic` warning, and does not
kill the consumer. If the consumer dies, capture silently stops and the user has no idea — the worst,
because invisible.

**Batching**: group writes into a transaction flushed when either 50 calls or 250ms of quiet have
accumulated, whichever first. Per-call writes churn SQLite; unbounded batching makes the dashboard
stale.

**Shutdown**: on `ctx` cancel, drain what remains (bounded, ~2s) then flush and close. Losing the last
few calls on Ctrl-C is acceptable; hanging on exit is not.

**`internal/session`** (this bead): `session.New(st *store.Store, gapMinutes int) *Resolver`. It is
both halves of grouping — the pre-insert `Resolve` and the post-insert `RecordCall`. `Resolve` groups
by `prefix_hash` within an inactivity-gap window; an explicit `x-clens-session` header **overrides**
the prefix and wins (test 6). `prefix_hash` NULL-ness is load-bearing (br-GI-1-05): NULL = keyed by
header, `""` = body did not parse, and `prefix_hash = ?` therefore never matches a header-keyed
session. Session ids use `s_<unix-ms>_<8hex>`.

**`Consumer.Stats()`** exposes processed, failed, drained, dropped, and last-write-at for `doctor` and
`GET /api/sources`.

## Rationale

Every prior bead is a pure component; this is the only place they are wired, so it is the only place a
cross-component mismatch can hide. The batching and the seam set are the cheap middle: correct, and
not stale.

## Outcome Definition

- `go test ./internal/consumer/... ./internal/session/... -race` passes.
- A captured call produces exactly one `events` row with metadata, usage and costing populated.
- An unparseable body still produces a row, with zero usage and no error surfaced.
- A panicking analyzer does not stop the consumer and yields an `analyzer_panic` warning row.
- A store error on one call does not stop processing subsequent calls.
- 1000 submitted calls produce 1000 rows.
- A subscription call writes `api_equivalent_cost_usd` and leaves `cost_usd` NULL; an API call is the
  reverse.
- Cancel mid-stream flushes and returns within the shutdown bound.

## Test Specifications

- Integration Tests (`internal/consumer/consumer_test.go`, real sink + real temp SQLite):
  - **Happy path**: a fully-formed call → one row, all fields correct.
  - **Streaming call**: an SSE `RespBody` → correct token counts.
  - **Unparseable request body** → row exists, `ModelRequested` empty, no panic, no error.
  - **Upstream error call** (`Err != nil`) → row exists with an `upstream_error` finding.
  - **Panicking analyzer** → recovered, consumer survives, later calls processed, `analyzer_panic`
    warning naming the analyzer.
  - **Failing store** (error on the first call) → the consumer continues; the second call is written.
  - **Batching**: 200 rapid calls → rows appear and the write count is materially below 200.
  - **Flush on quiet**: one call readable within 500ms.
  - **Throughput**: 1000 calls → 1000 rows.
  - **Shutdown**: cancel with 100 queued → `Run` returns within the bound, store closed cleanly.
  - **Cost wiring**: with a price table installed, a priced row carries the right `cost_source`; an
    unknown model leaves both cost columns NULL (never `$0.00`).
  - **Billing model**: a subscription `auth_kind`/account writes `api_equivalent_cost_usd` and NULL
    `cost_usd`.
  - **Session re-derivation**: a merge path that rewrites tokens leaves the session totals equal to
    the recomputed sum.
  - `-race` clean with a concurrent producer.
- Unit Tests (`internal/session/session_test.go`):
  - **Test 6 — session keys**: same prefix within the gap window groups; beyond the window splits;
    an `x-clens-session` header override wins; header-on-one-call/absent-on-the-other does not group.
  - `RecordCall` folds a row into its session's totals; a NULL session id is a no-op.
- E2E: none.

## Files to Touch

- `internal/consumer/consumer.go`, `internal/consumer/consumer_test.go` (create)
- `internal/consumer/analyzer.go` (create — `Analyzer`, `SessionResolver`, `SessionAggregator`)
- `internal/session/session.go`, `internal/session/session_test.go` (create)
