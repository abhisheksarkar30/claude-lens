# Storage schema

One SQLite file, `~/.clens/lens.db`, created whole from
`internal/store/schema.sql` — no migration framework in v1 (`CREATE TABLE IF
NOT EXISTS` only). Times are Unix nanoseconds.

The schema is the enforcement point for two invariants, which is why it is
worth reading before changing a column: see
[architecture.md](architecture.md) and
[cost-and-quota.md](cost-and-quota.md).

## The nine tables, in three groups

**Traffic** — what a call was:

| Table | Holds |
|---|---|
| `events` | one row per captured call: identity, tokens, cost, and the proxy-only request/response columns |
| `sessions` | the per-session fold: summed tokens, `priced_count`/`unpriced_count`, `warning_count` |
| `warnings` | one row per (event, kind) — `UNIQUE(event_id, kind)`, `ON DELETE CASCADE` |

**Collector output** — what a source reported:

| Table | Source | Key |
|---|---|---|
| `admin_usage_days` | `admin` | `UNIQUE(day_start, model, workspace_id)` |
| `admin_cost_days` | `admin` | `UNIQUE(day_start, model, description, currency)` |
| `admin_rate_limits` | `admin` | append-only, `fetched_at`-stamped |
| `quota_snapshots` | `snapshot` | append-only observations of `utilization_pct` |

**Reference and state**:

| Table | Holds |
|---|---|
| `prices` | `PRIMARY KEY (model, effective_from)` — a rate change is a new row, not an update |
| `model_catalog` | what the models endpoint advertised |
| `ingest_state` | one row per collector cursor, with `status` and `error` |

`ingest_state` is what `GET /api/sources` reads and what makes a failed
collector visible instead of a quietly short chart.

## Token columns

Total prompt size is **`input_tokens + cache_write_5m_tokens +
cache_write_1h_tokens + cache_read_tokens`** — `input_tokens` is the uncached
remainder only. `events.total_prompt_tokens` stores the sum so a query does
not have to re-derive it, and a test asserts the two agree. A row that reports
`input_tokens` alone as the prompt size is wrong.

## The billing split, in columns

Two billing models that must never be summed, carried by *which column the
figure lives in*:

| `billing_mode` | `cost_usd` | `api_equivalent_cost_usd` |
|---|---|---|
| `api` | the computed cost | NULL |
| `subscription` | NULL | the hypothetical API-equivalent value |

No `SUM(cost_usd)` — with or without a JOIN — can merge them, because they are
never in the same column. `sessions` mirrors the split with `total_cost_usd`
and `total_api_equivalent_cost_usd`.

`cost_source` labels how the figure was arrived at: `shipped` (from the
bundled table), `provisional` (bundled but unverified), `user` (an override),
`approximate:<reason>` (a rate applied with a known caveat), or `unpriced`
(no rate row — the cost columns stay NULL, never `0.0`). A schema invariant
test in `internal/store` pins all of it.

## One writer per source

`events` has one writer *package* per source, serialized by the store's single
write connection (`SetMaxOpenConns(1)`) rather than by the schema. The proxy
consumer owns `source='proxy'`; `internal/jsonlogs` owns `source='jsonl'`.

The cross-source merge (`internal/store/merge.go`) is the first `UPDATE` on
`events` and re-derives the owning session's totals in the same transaction.
It has to: a merge can rewrite a row's token columns, and an incremental fold
would drift away from the sum it is supposed to equal.

`request_id` is `UNIQUE` — that uniqueness is what makes the merge possible at
all, and what makes a second source arriving with disagreeing counts a
`source_mismatch` warning instead of a second row.
