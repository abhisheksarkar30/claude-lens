# GI-1 — claude-lens v1

<!-- version=5 status=converged -->

Ticket: [issue #1](https://github.com/abhisheksarkar30/claude-lens/issues/1) · Branch: `GI-1-claude-lens-v1` (off `main`; no `develop` exists yet) · Module: `github.com/abhisheksarkar30/claude-lens`

`claude-lens` is a local, loopback-only observability and usage/cost accounting tool for Claude. One
Go binary (`clens`), one SQLite file, no hosted service. It answers, for **every** way a person can
pay for Claude — Free, Pro, Max 5x, Max 20x, Team, Enterprise, and pay-as-you-go API keys — *how
much am I using, on what, what did it cost, and how close am I to my limit*. It carries over the
full feature set proven in `deepseek-lens` and extends it from one traffic source to four.

---

## What changes

A new Go module at `D:\github\claude-lens`. Nothing pre-exists; every file below is created.

```
claude-lens/
├── go.mod, README.md, .gitignore, LICENSE
├── cmd/clens/main.go                    subcommand dispatch
├── internal/
│   ├── config/                          flags + file + env, defaults, validation, accounts
│   ├── secret/                          ~/.clens/secrets.toml (POSIX 0600 / Windows ACL) — admin key, sessionKey
│   ├── sink/                            bounded channel, drop counter — the non-blocking guarantee
│   ├── proxy/                           reverse proxy, tee, header redaction, fail-open
│   ├── decode/                          Content-Encoding removal (gzip/deflate/br/zstd)
│   ├── parse/                           incremental SSE + non-stream JSON; meta + usage extraction
│   ├── store/                           SQLite schema, single ingest writer, reads, purge
│   ├── catalog/                         model catalogue (live GET /v1/models + shipped fallback)
│   ├── pricing/                         price table, per-class cost arithmetic, drift
│   ├── session/                         conversation grouping
│   ├── analyze/                         rule engine (cache, thinking, response, billing kinds)
│   ├── quota/                           rolling-window burn, plan limits, snapshot calibration
│   ├── consumer/                        proxy ingest pipeline (sink → parse → session → cost → store)
│   ├── jsonlogs/                        Claude Code .jsonl incremental tailer → events
│   ├── snapshot/                        claude.ai quota poller → quota_snapshots
│   ├── adminrep/                        Admin API usage/cost reports → admin_*_days
│   ├── reconcile/                       computed-vs-billed drift; source cross-checks
│   ├── ingest/                          orchestration of the three non-proxy collectors + scheduler
│   ├── replay/                          body-edit + re-issue logic
│   ├── api/                             dashboard JSON API + SSE broker
│   ├── cli/                             subcommand implementations
│   └── web/                             go:embed'd index.html, app.js, style.css
└── docs/, .beads/, .githooks/, .github/workflows/
```

---

## Why — mapping to requirements

| Requirement (issue #1) | Delivered by |
|---|---|
| One binary, one SQLite file, loopback-only | `cmd/clens`, `internal/store`, `internal/config.Validate` |
| Four sources, each failing independently | `proxy`+`consumer`, `jsonlogs`, `snapshot`, `adminrep` — independent writers, per-source health in `GET /api/sources` |
| A turn seen twice is stored once | `events.request_id` UNIQUE + `source_refs` (see *Cross-source identity*); the dedup key's equivalence to Claude Code's `requestId` is an assumption with a verification step, not yet evidence |
| Subscription and API figures never merged | Enforced by the schema, not by discipline: a subscription row leaves `events.cost_usd` NULL and carries `api_equivalent_cost_usd` instead, so `SUM(cost_usd)` cannot merge the two billing models with or without a JOIN; every aggregate surface additionally groups by `billing_mode` (see *Billing model* and invariant 5) |
| Computed cost reconciled against billed | `internal/reconcile` + `cost_drift` warning + `clens reconcile` |
| No invented numbers | Empty-by-default price rows and plan limits; `unpriced` / `unconfigured` are first-class labelled states |
| Credentials never in the DB, logs, or UI | `internal/secret` (outside the DB; POSIX modes on Unix, an explicit Windows ACL on Windows), `internal/proxy/redact`, startup `RedactCheck`, dashboard never reads `secret` |
| Full deepseek-lens feature set | *Carried over* table below |

### Carried over from deepseek-lens

Every v1 feature lands with a recorded disposition — including the ones deliberately dropped, and the reason. The table's contract is that no feature is *silently* dropped; where a feature is absent it is because a row below says so.

| Feature | Disposition |
|---|---|
| Streaming reverse proxy, tee, fail-open | **kept** — upstream default `https://api.anthropic.com` |
| TTFB no-buffering hard gate + `TestNoBufferingSSE` | **kept verbatim** — still the hard gate |
| Bounded non-blocking sink + drop counter | **kept verbatim** |
| Response/request body capture with cap + policy | **kept** — same `full`/`truncated`/`off`, same 256KB default |
| `Content-Encoding` decode (gzip/deflate/br/zstd) | **kept** — Claude Code sends `Accept-Encoding: gzip, deflate, br, zstd` |
| Incremental SSE parse + non-stream JSON | **kept** — plus Claude-specific extras (`stop_reason`, `stop_details`, `usage.*`) |
| Batch-grouped consumer, per-call panic containment | **kept** — pipeline order unchanged |
| Session grouping (prefix hash + gap, `x-lens-session`) | **kept** — header renamed `x-clens-session` |
| Price table, `lens prices --set/--unset/--edit`, live reload | **kept** — table ships populated with documented rates, plus `provisional` rows for models the bundle lists but does not price (see *Cost engine*) |
| Per-call cost with provenance (`cost_source`) | **kept** — extended to 6 token classes + speed/tier |
| Analyzer rule engine + kinds + README-consistency test | **kept** — rule catalogue replaced with Claude-relevant rules |
| Replay (`--replay` opt-in, Origin/Host guard, cost gate, `--set`/`--diff`/`--dump`) | **kept verbatim** |
| Purge / retention / `--vacuum` / scheduled purge | **kept verbatim** |
| Pagination (`?limit`/`?offset`, `X-Total-Count`/`X-Limit`/`X-Offset`) | **kept verbatim** |
| Stats by period (`hour\|day\|week\|month`, half-open `[since, until)`) | **kept verbatim** |
| SSE live push + broker + `PublishingStore` decorator | **kept verbatim** |
| Doctor with PASS/WARN/FAIL + live stats + `provider_hooks` | **kept** — `provider_hooks` generalizes to `client_config` |
| `--allow-remote` footgun + loopback validation | **kept verbatim** |
| Origin/Host allowlist on write routes (3) | **kept** — now 6 routes, same shared guard |
| Redaction of `x-api-key`/`Authorization`/`Cookie` + `RedactCheck` | **kept** — extended to `sessionKey` and `sk-ant-admin…` |
| Unpriced-as-a-label discipline | **kept** — becomes the general "no invented numbers" rule |
| `ponytail:` inline comments for deliberate ceilings | **kept** — convention unchanged |
| Embedded vanilla dashboard, no framework, no build step | **kept** — charts hand-rolled (see *Decision log*) |
| `branch-guard.yml`, `main-guard.yml`, `.githooks/{commit-msg,pre-commit}` | **kept** — `GI#<n>` prefix unchanged |
| 12 CLI subcommands | **kept** — 18 total (see *CLI*) |
| Peak-pricing subsystem (2× window, `pricing.IsPeak`, `peak_pricing` warning) | **dropped** — DeepSeek bills 2× inside a UTC peak window; Claude has no peak window, so there is nothing to substitute and no warning to raise. Every priced call uses a single documented rate. |
| `--model-map` config + `model_remapped` finding | **dropped / adapted** — the flag existed to supply a map DeepSeek's names required and the tool could not observe. For Claude the alias→resolved mapping is client-side (`ANTHROPIC_MODEL`) and *is* observed, in the captured request's `model_requested` versus the response's `model_resolved`, so a user-configured map has nothing left to add. The `model_remapped` finding folds into `model_mapping_drift` (kept in T2), which now fires on that observation rather than on a configured map. |

---

## The four sources

Each answers a different question, and none subsumes another. This is the whole design.

| # | Source | Transport | Granularity | What it is authoritative for | What it cannot tell you |
|---|---|---|---|---|---|
| **A** | **Live proxy capture** | tee on the hot path | per call, full request+response bodies | *what was actually sent* — cache breakpoints, tools, thinking config, auth kind; and therefore every cache/prefix rule | only from install time forward |
| **B** | **Claude Code JSONL** | incremental **recursive** walk of `~/.claude/projects/**/*.jsonl` — top-level session transcripts **and** `…/<sessionId>/subagents/agent-*.jsonl` sidechains (823 subagent files vs ~205 top-level on the authoring machine) | per API request (deduped) | history that predates install, and sessions never routed through the proxy | no request bodies, Claude Code only |
| **C** | **claude.ai quota snapshots** | cookie-authenticated poll of claude.ai's internal usage endpoint | one row per window per poll | **quota consumption %** — the only quota truth for a subscription | no tokens, no dollars, undocumented |
| **D** | **Admin usage & cost reports** | Admin API key, raw HTTP, daily | per day × model × workspace | **actually-billed dollars**, exactly | API organizations only; too coarse to attribute a call |

### Why the proxy is not redundant

The tech plan (v2) has three sources and no proxy. The proxy's unique contribution is the request
body, but the honest statement of what that buys is narrower than "these rules are undecidable
without it". Classify the three flagship rules by what they actually need:

| Rule | Detection | Attribution / localisation |
|---|---|---|
| `cache_prefix_invalidation` | **B can do this** — fires on a token-ratio signal (`cache_creation_input_tokens` ≈ full conversation size while `input_tokens` stays small), computable from source B's `usage` alone | **only A** — naming the byte that broke the prefix (a timestamp, a non-deterministic serializer, a reordered tool list) requires the captured body |
| `cache_invalidated_by_tools` | **B can do this** — the *effect* (whole-cache invalidation from position 0) shows in the usage numbers | **only A** — the *cause* is the `tools` array or top-level `system`, which only the body carries |
| `cache_prefix_below_minimum` | **only A** — no token stream can reveal that a `cache_control` marker was present but under the minimum, because the symptom is `cache_creation_input_tokens: 0` with no marker visible anywhere in `usage` | A (same capture) |

So exactly **one** rule (`cache_prefix_below_minimum`) genuinely needs the body to *fire*; the other
two fire from source B and use the body to *explain*. The proxy earns its place on three grounds that
survive that correction: it **attributes and localises** (turns "your cache keeps breaking" into
"this byte breaks it"), it captures calls from **any** Anthropic-shaped client — Cline, the raw SDK,
a script — where source B sees Claude Code and nothing else, and it is the only source that can never
be stale relative to a Claude Code version. The `cache_prefix_below_minimum` rule alone is not
sufficient justification for a hot-path component; the localisation and the non-Claude-Code coverage
are. **If a future implementer finds neither of those worth the hot-path cost, dropping source A is a
legitimate outcome** — source B+D would still answer every question except localisation and non-CC
clients. That is recorded here rather than defended.

### Why four and not fewer

Sources A and B are complements in time; C and D are complements in kind. B backfills what A
misses before install; A attributes what B cannot see. C is the only quota signal for a
subscription; D is the only authoritative money signal for an API account. Dropping any one leaves
a question unanswerable for one of the seven billing shapes. They stay independent by construction:
separate packages, separate writers, separate `ingest_state` rows, and a per-source health surface
so a broken collector is visible instead of silent.

---

## Billing model — all seven shapes

Two billing models that must never be summed into one number, and seven plans across them. The
separation is carried in the schema: a `subscription` row writes **`api_equivalent_cost_usd`** and
leaves **`cost_usd` NULL**; an `api` row writes `cost_usd`. The two figures therefore live in
different columns, so no `SUM(cost_usd)` — with or without a JOIN — can merge them. Showing the
subscription figure under a label that says "API-equivalent, hypothetical" is then a rendering
choice over a column that already carries the label.

| Plan | `billing_mode` | Cost shown | Quota shown |
|---|---|---|---|
| Free | `subscription` | API-equivalent value in `api_equivalent_cost_usd` (labelled hypothetical) | snapshot % + calibrated burn |
| Pro | `subscription` | API-equivalent value in `api_equivalent_cost_usd` | snapshot % + rolling 5h/7d burn |
| Max 5x | `subscription` | API-equivalent value in `api_equivalent_cost_usd` | snapshot % + rolling 5h/7d burn |
| Max 20x | `subscription` | API-equivalent value in `api_equivalent_cost_usd` | snapshot % + rolling 5h/7d burn |
| Team | `subscription` | API-equivalent value in `api_equivalent_cost_usd` | snapshot % + shared-seat context |
| Enterprise | `subscription` | API-equivalent value in `api_equivalent_cost_usd` | snapshot % + configured spend limit |
| Pay-as-you-go | `api` | **real billed dollars** (source D) and computed dollars in `cost_usd` (sources A/B) | Admin rate-limit reports |

### Auth-kind classification (how the tool knows which model a call belongs to)

The proxy classifies every captured call from the *shape* of its credential, and stores the
classification — never the credential:

| Observed credential | `auth_kind` | Meaning |
|---|---|---|
| `Authorization: Bearer` carrying an OAuth token | `oauth` | subscription (Claude Code on Pro/Max) |
| `x-api-key` carrying a standard key | `api_key` | pay-as-you-go |
| `x-api-key` carrying an admin key | `admin` | admin credential used against the data plane (a misconfiguration) |
| cloud-provider signature headers | `cloud` | Bedrock/Vertex/Foundry — **out of scope in v1**, classified and stored, not priced |
| none / unrecognised | `unknown` | recorded as such, never guessed |

The three first-party credential prefixes are `sk-ant-oat` (OAuth), `sk-ant-api` (standard), and
`sk-ant-admin` (admin). Only the prefix classification is stored; the value is redacted before the
row is written and is never logged or rendered.

`billing_mode` is then derived from the configured account, not from the credential alone: a mixed
account (Pro subscription **and** a separate API key) is two accounts, and the dashboard renders
them side by side and **never** sums them.

### No invented numbers

`deepseek-lens` established the rule: an invented rate is worse than no rate, because `$0.00` reads
as "this was free" while `unpriced` tells you to go configure something. Two states inherit it, and
they are first-class — stored, queried, and rendered, not collapsed to zero:

- **`unpriced`** — a model with no rate row. `clens stats` reports the unpriced count *alongside*
  the total, never folded in.
- **`unconfigured`** — a plan with no limit row. The dashboard shows the snapshot percentage and
  your own token burn, and states that no limit is configured. It never renders a percentage of a
  limit nobody supplied, and never guesses that "Max 20x" means a particular number of messages.

Limits become known one of two ways: the user configures them, or `internal/quota` *learns* them
from your own history — when a snapshot window reaches 100%, the burn recorded at that moment is an
empirical observation of the limit, and is offered for confirmation rather than applied silently.

---

## Architecture

One process, two `http.Server`s on separate listeners, plus collector goroutines.

```mermaid
flowchart LR
  Client["Client<br/>Claude Code / Cline / SDK"] -->|"ANTHROPIC_BASE_URL"| Proxy["internal/proxy<br/>hot path, tees bytes"]
  Proxy -->|"forward unmodified"| Up["api.anthropic.com"]
  Proxy -->|"Submit (non-blocking)"| Sink["internal/sink"]
  Sink --> Consumer["internal/consumer<br/>cold path"]
  Consumer --> Store[("SQLite<br/>~/.clens/lens.db")]
  JL["~/.claude/projects/**/*.jsonl"] --> Tailer["internal/jsonlogs"]
  CA["claude.ai usage endpoint"] --> Snap["internal/snapshot"]
  AA["Admin usage/cost API"] --> Adm["internal/adminrep"]
  Tailer --> Store
  Snap --> Store
  Adm --> Store
  Store --> API["internal/api<br/>JSON + SSE"]
  API --> Web["Browser dashboard<br/>internal/web"]
  CLI["internal/cli<br/>clens …"] --> Store
  CLI -->|"replay / writes"| API
```

### Invariants (each is a rule a change must not break)

1. **The hot path never buffers the stream to count tokens.** A buffered stream still returns
   correct bytes — just late — so only the TTFB test catches it. Still the hard gate.
2. **`internal/proxy` depends only on `sink`, `config`, and `secret`-free primitives.** If it ever
   imports `analyze`, `store`, or `pricing`, the design has eroded.
3. **`internal/events` rows have exactly one writer *package* per source, serialized by the store's
   single write connection.** The proxy's consumer goroutine is the only writer for
   `source='proxy'`; `jsonlogs` is the only writer for `source='jsonl'`. The two writer packages are
   serialized by the store's one write connection, not by the schema. Purge is a third writer
   (in-process sharing the store's connection; `clens purge` in a separate process, serialized by WAL
   + `busy_timeout(5000)`). **The cross-source merge is a deliberate departure from the carried-over
   store's append-only `requests` table**: it introduces the first `UPDATE` on `events` (the merge
   path below), where a row the `jsonlogs` writer created may be updated by the proxy writer. It is
   the store's job to make that one serialized statement, and it is called out so nobody reads
   "one writer" as "append-only". **The merge also re-derives the affected session's totals, not
   merely increments them** — because it can rewrite a row's token columns, the owning session's
   incrementally maintained totals would otherwise drift, so the merge `UPDATE`, the analyzer warning
   **upsert**, and the session re-derivation run in the **same transaction**, and the re-derivation
   wins over the incremental fold for that session: the token/cost totals are reconstructed from
   `events`, and `warning_count` from `warnings` — never from `events` and never by an increment,
   because the findings live in their own table, written by the analyzer step. **The warning ledger
   carries the same drift risk the token ledger did**, and the same fix: `warnings` is keyed
   `UNIQUE(event_id, kind)` and the analyzer attach is an upsert, so re-running the seam on a merged
   row updates its finding of that kind instead of appending a duplicate. This is the one place the
   two statements are reconciled, rather than asserted separately.
4. **`input_tokens` is the uncached remainder only.** Total prompt size is
   `input_tokens + cache_write_5m_tokens + cache_write_1h_tokens + cache_read_tokens`. A row that
   reports `input_tokens` alone as the prompt size is wrong, and `store` maintains the derived total
   so no caller can get it wrong. When a log carries only the flat `cache_creation_input_tokens` (a
   shape not observed on the authoring machine, handled belt-and-braces — see *Cost engine*), the
   flat count is stored in `cache_write_5m_tokens` so the derived total still counts it. This is a
   tested invariant, not a convention.
5. **A cost figure's billing model is carried with it, in the column it lives in.** A
   `subscription` figure is written to `api_equivalent_cost_usd` and `cost_usd` is left NULL; an
   `api` figure is written to `cost_usd`. A total is therefore only ever a total of one billing
   model, because the two figures are not in the same column. Invariant: no query may label the
   `api_equivalent_cost_usd` sum as a cost *paid*, and every aggregate surface (`/api/stats`,
   `clens stats`, the Overview tab) additionally groups or filters by `billing_mode`. This is
   enforced by a schema invariant test, not by a convention.
6. **Fail open.** A broken observer never breaks the user's coding session. Extended to the
   collectors: a broken *collector* never prevents the others from writing, and never crashes the
   process.
7. **Credentials never reach the database.** Not in headers (redacted before the tee), not in
   bodies (policy + cap), not in `secret` (which lives in a file outside the DB — protected by POSIX
   modes on Unix and an explicit Windows ACL on Windows, see *Security posture* — and is never read
   by the API or web layers).

### Cold-path pipeline (order is fixed; feature beads plug into named seams)

`ExtractMeta`/`ExtractUsage` → resolve session → resolve account/`auth_kind` → compute cost →
`InsertEvent` → run analyzers (**upsert** each finding by `(event_id, kind)`) → **re-derive the
owning session's totals — tokens/cost from `events`, `warning_count` from `warnings` — in the same
transaction as the merge `UPDATE`** → session fold.

Session resolution, account resolution, and costing populate columns on the row so they run
**before** insert. Warning analyzers attach by row id so they run **after** the insert and **inside
its transaction** — they must, because the `warning_count` re-derivation below reads `warnings` and
has to see the merged row's final finding set. **The re-derivation runs in the same transaction as
the merge `UPDATE`**: a merge can *rewrite* a row's token (and therefore cost) columns, so the owning
session's incrementally maintained totals cannot be a plain add — when a merge touches a session's
rows, that session's totals are recomputed from `events` in that transaction, and the recomputation
(not the increment) is the value that stands. **The merge ledger has the same shape on the warning
side, and the plan names the warning semantics it was silent on:** a call observed by both sources
runs the analyzer seam twice on one `event_id`, so the attach is an **upsert keyed by
`(event_id, kind)`**, not an append — the second run updates the row's finding of that kind rather
than adding a second copy. `UNIQUE(event_id, kind)` (schema below) is the mechanism, not a
replace-on-merge `DELETE` — chosen because the pipeline runs the analyzer seam after **every** insert
and has no "this was a merge" branch to hang a delete on, and because a delete-and-reattach would
drop a kind the first source's run raised if the merged row does not re-raise it, losing the union
the merge exists to produce. `warning_count` is then `COUNT` over that deduped ledger and **cannot be
inflated by the double run**. It is **re-derived from `warnings` on every write that touches the
session, never added by the fold**: it is not derivable from `events` (the findings live in their own
table, written by the analyzer step), which is exactly how an incremental counter drifts. The
`Analyzer` seam is unchanged from deepseek-lens:

```go
Analyze(meta parse.Meta, usage parse.Usage, ev *store.Event) []store.Warning
```

### Cross-source identity (the dedup rule)

The same call can arrive from A and from B. The plan's dedup key is Anthropic's `request-id`
**response header**, on the *assumption* that it is byte-equal to the `requestId` Claude Code writes
into its log line. That equivalence is **not yet established** — the plan asserts it and has no
captured pair to show for it, and one real JSONL line (`"requestId":"req_011Ceh2nFYY1p63usDZFpJ4q"`)
confirms only the JSONL half of the equation. It is therefore an **assumption with a verification
step**, not a fact:

- **Verification (a prerequisite step, recorded in this plan before the cross-source merge is relied
  on).** Before implementation of the merge, capture one live response's `request-id` header through
  the proxy and the matching Claude Code JSONL line for the same call, and assert they are equal.
  Record the result here. If equal, the header is the key and the assumption is discharged with
  evidence. If not equal, **the fallback key below becomes primary** and the JSONL `requestId` is
  used only within JSONL.
- **Fallback key (primary if the header does not match).** A value the plan can verify locally:
  `(model, session_id, started_at ±1s, input/cache-write/cache-read/output token quadruple)`, with
  `request-id` used *when present and matching* to strengthen it.

`events.request_id` is `NOT NULL UNIQUE`, synthesized deterministically when a source has no usable
id, so the key is always present:

- **Response-derived id preferred.** When the proxy sees a response, the key is derived from that
  response's `request-id` (which is distinct per attempt — a retried call carries a new one), never
  from the request body.
- **Body hash is a last-resort identifier only**, used solely for a call that never produced a
  response (a transport failure with no `request-id`). Even then it must not collapse attempts: the
  synthetic key carries a per-attempt disambiguator — `proxy:<sha256(body)>:<started_at_ns>:<attempt>`
  — so **two byte-identical bodies on two attempts produce two rows, not one**. Collapsing them would
  destroy exactly the `rate_limited` / `overloaded` signal those attempts exist to record.
- `jsonl:<sessionId>:<uuid>` for a JSONL line with no `requestId`.
- An insert that collides **merges** rather than duplicates: `source_refs` gains the new source,
  `first_source` is preserved, and token counts are **not** re-added. Sources are complementary for
  the same row — A contributes the bodies and the request-side material, B the JSONL-only metadata
  and, when its capture is the complete one, the token counts — so the merge is a union of columns,
  not a sum of numbers. **Which source is *complete* is the precedence rule, not which source
  arrived first.** A is not always the source that carries the numbers: with body capture
  `off`/`truncated` (the carried-over policy, 256 KB cap) or an unfinished SSE stream
  (`stream_incomplete`), A's row has no or partial `usage` while B's JSONL `usage` is authoritative.
  So on the token columns the **complete capture wins** — A when `capture_complete`, otherwise B.
  This means a merge can **rewrite** a row's token (and therefore cost) columns, not merely add to
  them, so the owning session's totals are **re-derived** from `events` in the same transaction as
  that `UPDATE` (the cold-path order above and invariant 3). The merge does **not** rewrite
  `session_id` — it is the row's grouping identity, not a token column — so the row keeps the session
  it was first written under, and exactly that one session is the re-derivation target. The merge
  **does** re-run the analyzer seam, and its findings join the row by **upsert keyed by
  `(event_id, kind)`** (`UNIQUE(event_id, kind)`, schema above), so the second run contributes the
  union of the two sources' findings with no duplicate kinds and no inflated `warning_count` — the
  warning-ledger instance of the same rewrite-not-add rule that governs the token columns.
- `source_mismatch` fires **only when two *complete* sources disagree** on the token counts — a real
  parser bug — and **never** when one source simply has nothing to contribute (an absent or partial
  A against a full B is a `0 vs N` non-disagreement, not a mismatch). The columns A structurally
  cannot supply — `client_version`, `project`, `git_branch`, `is_sidechain`, `cli_entrypoint` — are
  preserved from whichever writer supplied them, so B is not a no-op participant in the merge.

This is why "a turn observed by more than one source is stored once" holds — *given* the key
equivalence above, which the verification step is there to test rather than assume.

---

## Storage schema (SQLite)

`CREATE TABLE IF NOT EXISTS` only, time as Unix nanoseconds, WAL, `foreign_keys(ON)` — all as in
deepseek-lens. No migration framework in v1; the schema is created whole.

| Table | Purpose | Key columns |
|---|---|---|
| `events` | one row per observed turn, from A or B | `request_id` UNIQUE, `source`, `source_refs`, `first_source`, `started_at`, `ended_at`, `auth_kind`, `account`, `billing_mode`, `model_requested`/`model_resolved`, `input_tokens`, `output_tokens`, `cache_write_5m_tokens`, `cache_write_1h_tokens`, `cache_read_tokens`, `thinking_tokens`, `total_prompt_tokens`, `service_tier`, `speed`, `effort`, `inference_geo`, `stop_reason`, `stop_category`, `is_sidechain`, `session_id`, `project`, `git_branch`, `client_version`, `cli_entrypoint`, **`cost_usd` (NULL when `billing_mode='subscription'` OR when the row is `cost_source='unpriced'` — an unpriced API row is NOT `$0.00`)**, **`api_equivalent_cost_usd` (populated only when `billing_mode='subscription'`; NULL when the row is unpriced)**, `cost_source`, `prefix_hash`, `replay_of`, `replay_edits`, `capture_complete` |
| `events` (proxy-only, nullable for B) | the request/response material only A has | `method`, `path`, `status`, `req_headers`, `resp_headers`, `req_body`, `resp_body` |
| `sessions` | agentic-run grouping with incrementally maintained totals (re-derived in the same transaction whenever a cross-source merge rewrites one of its rows — token/cost totals from `events`, `warning_count` from `warnings`, never by increment; see invariant 3) | `id` (`s_<unix-ms>_<8hex>`), `prefix_hash`, `first_seen`/`last_seen`, `request_count`, token totals, `priced_count`/`unpriced_count`, `model_set`, `warning_count` (a `COUNT` over `warnings`, not over `events`), **`total_cost_usd` (sums API-billed rows only; NULL when the session has none, or when every API row it has is `unpriced`)**, **`total_api_equivalent_cost_usd` (sums subscription rows only; NULL when the session has none)** |
| `warnings` | **at most one analyzer finding of each kind per event row**, enforced by the schema — not asserted | `event_id` FK → `events(id)` ON DELETE CASCADE, `kind`, `severity`, `detail`, `path`, `created_at`, **UNIQUE(`event_id`, `kind`)** |
| `admin_usage_days` | source D, usage report | **UNIQUE(`day_start`, `model`, `workspace_id`)**, `day_start`, `window_start`/`window_end` (the fetched page's bounds), `model`, `workspace_id`, `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_write_tokens`, `raw`, `fetched_at` — collected by UPSERT |
| `admin_cost_days` | source D, cost report (**separate table — different key**) | **UNIQUE(`day_start`, `model`, `description`, `currency`)**, `day_start`, `window_start`/`window_end`, `model`, `description`, `amount_usd`, `currency`, `raw`, `fetched_at` — collected by UPSERT |
| `admin_rate_limits` | source D, org + workspace rate-limit reports | `scope`, `workspace_id`, `model`, `group_type`, `limit`, `fetched_at` |
| `quota_snapshots` | source C, one row per window per poll | `observed_at`, `account`, `window`, `utilization_pct`, `resets_at`, `status`, `raw` |
| `prices` | rate table mirrored for auditability | `model`, `input_rate`, `output_rate`, `cache_write_5m_rate`, `cache_write_1h_rate`, `cache_read_rate`, `fast_input_rate`, `fast_output_rate`, `batch_multiplier`, `effective_from`, `source` |
| `model_catalog` | `GET /v1/models` result + shipped fallback | `model_id` PK, `display_name`, `max_input_tokens`, `max_output_tokens`, `capabilities`, `fetched_at`, `source` |
| `ingest_state` | per-collector cursor + last outcome | `key` PK (`jsonl:<path>` byte-offset cursor; `admin:usage` / `admin:cost` = last-fetched `day_start` + the fetched `[window_start, window_end)` so a re-run re-requests only the gap; `snapshot:<account>` = last poll), `value`, `status`, `error`, `updated_at` |

**Why `admin_usage_days` and `admin_cost_days` are separate tables.** They come from two endpoints
with different groupings and different bucket widths. Merging them would force one to be
denormalised into the other's key. Note this is a *different* distinction from the one the
requirement names: the split keeps per-call computed data apart from per-day billed data. The
subscription-vs-API separation is enforced elsewhere and now genuinely by the schema — a
subscription `events` row has `cost_usd` NULL and its hypothetical figure in
`api_equivalent_cost_usd`, so `events.cost_usd` only ever holds API-account figures.

**Idempotent by key, not by luck.** Each admin table carries a UNIQUE constraint on its natural key
(`admin_usage_days`: `day_start, model, workspace_id`; `admin_cost_days`: `day_start, model,
description, currency`) and the collector **UPSERTs** rather than appends. Because the Admin
endpoints return a multi-day window per call (7 daily buckets by default), a `clens refresh`, a
`clens ingest --rebuild`, or a scheduler retry after a partial failure re-fetches days already
stored; the constraint makes that a no-op instead of a silent inflation of the "actually-billed
dollars" figure that is the plan's only ground truth. Each row also stores the fetched page's
`window_start`/`window_end`, and the `admin:*` `ingest_state` value records the last-fetched
`day_start`, so a re-run re-requests only the gap it does not hold. If the admin tables are made
idempotent by key alone, `ingest_state` for the admin collectors may be dropped — the constraint is
the control; the cursor is an optimisation.

**The `warnings` ledger is idempotent by `(event_id, kind)`, for the same reason.** The analyzer
seam runs after *every* insert — a fresh row and a cross-source merge alike — so a call observed by
both sources runs the rules **twice on one `event_id`**, and deepseek-lens's safety here rested on
its `requests` table being append-only (`internal/store/schema.sql:72-81` has no `UNIQUE`, and
`InsertWarnings` is a bare `INSERT`, `internal/store/store.go:238-246`) — a precondition claude-lens
removes with the first `UPDATE` on `events`. `UNIQUE(event_id, kind)` with the upsert attach makes
the second run an update, not a second row, so the two sources contribute the union of their
complementary findings and `sessions.warning_count` (re-derived from this table) cannot be inflated
by the merge. **By construction the seam emits at most one finding of a given `kind` per event row** —
each rule owns its kind, and the one deepseek-lens rule that emitted several findings of one kind for
a request (`unsupported_content_block`, `internal/analyze/rules.go:160-168`) is DeepSeek-specific and
is not in the carried-over catalogue — so `(event_id, kind)` is the natural key rather than a lossy
one. The constraint is what makes that claim **testable** instead of a property a reader has to
re-derive from the analyzer code.

**Why `quota_snapshots` is one row per window** rather than the tech plan's fixed
`session_pct`/`weekly_pct` columns: the endpoint returns different window sets on different plans
(5-hour, 7-day, and per-model 7-day windows). A fixed pair of columns would silently drop whichever
windows the user's plan actually has.

**A session total is split the same way an `events` row is.** A session is an agentic-run grouping,
not a billing-mode grouping — one session can hold both `api`-billed and `subscription` rows (a mixed
account) — so `sessions` carries the same two-column split `events` does: `total_cost_usd` sums only
API-billed rows and `total_api_equivalent_cost_usd` sums only subscription rows, each **NULL** — never
`$0.00` — when the session has no row of that model. A session whose API rows are all `unpriced`
therefore reads **NULL** on `total_cost_usd` too, not `$0.00`; `unpriced_count` (shown alongside)
disambiguates that from a session with no API row at all. `clens sessions` and `GET /api/sessions`
therefore render **two labelled figures**, with `unpriced_count` shown alongside, and no read path
emits a session total that mixes billing models or reports `$0.00` for a subscription or unpriced
session. This is the per-session instance of invariant 5, and it is why F1 read "every aggregate
surface": the session total was one of them.

### Reconciliation (the labelled comparison)

A vs D and B vs D are compared with an explicit, labelled JOIN on `(day, model)`. This JOIN is a
*side-by-side comparison of two labelled figures*, not a sum, and it cannot silently merge a
subscription figure with a billed one because the `events` side is restricted to API rows by
construction (`events.cost_usd` is NULL for subscription rows):

- `clens reconcile` and `GET /api/reconcile` report, per day and model, **computed** (`SUM(events.cost_usd)`
  — which is empty for subscription rows, by the schema rule above) against **billed**
  (`admin_cost_days.amount_usd`). The two are shown in separate columns, never added.
- Divergence beyond a configurable threshold raises `cost_drift`. This closes deepseek-lens's stated
  open risk *"pricing table needs manual upkeep"* **for API accounts only** — the billed figure exists
  only where an Admin cost report exists. For subscription accounts there is no billed counterpart, so
  `cost_drift` can never fire; the guard there is `model_catalog` refresh plus the `unpriced` /
  `approximate` labels, and an optional staleness signal for a shipped rate unverified for more than N
  days. The plan does not claim `cost_drift` answers the upkeep risk globally.
- A vs B is compared by `request_id` overlap and raises `source_mismatch` on token disagreement.

---

## Cost engine

`pricing.Compute(model, usage, speed, serviceTier, at) → (usd, costSource)`.

Six priced token classes plus two modifiers, because Claude bills them differently:

| Class | Rate | Source |
|---|---|---|
| `input` | base input rate | `usage.input_tokens` (uncached remainder) |
| `output` | base output rate | `usage.output_tokens` |
| `cache_write_5m` | **1.25×** input rate | `usage.cache_creation.ephemeral_5m_input_tokens` |
| `cache_write_1h` | **2×** input rate | `usage.cache_creation.ephemeral_1h_input_tokens` |
| `cache_read` | **0.1×** input rate (**0.025×** on Fable 5.1) | `usage.cache_read_input_tokens` |
| `thinking` | priced as output | `usage.output_tokens_details.thinking_tokens` (a subset of output — never added again) |

| Modifier | Effect |
|---|---|
| `speed: "fast"` | Opus 5 / Opus 4.8 use their own rates ($10/$50 per MTok), not the standard Opus rates |
| `service_tier: "batch"` | ×0.5 on every class |
| `service_tier: "priority"` | **no rate change applied, and none asserted.** The skill bundle states no per-token Priority rate — neither a premium nor "standard rates" — so the plan claims neither; the only documented Priority facts it carries are that Priority Tier is **unsupported** on Fable 5.1 / Mythos 5.1 (`shared/models.md:73`) and that Priority costs are not in the cost report (`shared/cost-optimization.md:39`). What Priority costs is left to the live usage endpoint's `service_tier` dimension, not guessed here. |

Rounding is applied **per class, then summed** — never on the total after the modifiers are applied
— because the per-class version is the one that matches an invoice line. (deepseek-lens learned this
the hard way, but its `TestComputePeakRoundsSumNotTotal` pinned the **2× peak multiplier**, and Claude
has no peak window (F9 dropped it). The carried-over test's subject therefore becomes the modifier
that *does* exist here: **batch billing**, `service_tier: "batch"` ×0.5. It is ported as
`TestComputeBatchRoundsPerClass` — the ×0.5 is applied per class *before* rounding, and rounding the
discounted total instead would not match the invoice.)

**Fallback when the TTL split is absent (belt-and-braces).** The cost engine reads the TTL from
`usage.cache_creation.ephemeral_5m_input_tokens` / `ephemeral_1h_input_tokens`. Every JSONL file on
the authoring machine that carries the flat `cache_creation_input_tokens` also carries that split
(1012 of 1012), so a flat-only shape **has not been observed**. It is handled anyway, because source
B exists to backfill pre-install history and a shape that has not been seen is not a shape that
cannot occur: if the `ephemeral_*` split is missing, the whole flat
`cache_creation_input_tokens` is recorded into `cache_write_5m_tokens` (so invariant 4's derived
total still counts it), and the row is marked `cost_source = approximate` with a
`cache_ttl_unknown` note — because the true rate could be 1.25× (5-minute) or 2× (1-hour), and the
tool says which it does not know rather than picking one silently. Test 9/12 carries a fixture for
both the split and the flat shape.

### Shipped price table

Unlike deepseek-lens's deliberately-empty table, the v1 table **ships populated** — with a rate the
skill bundle documents, or, for a model the first-party model list carries but the bundle does not
price, a `provisional` figure with a named verification step. Using a documented rate is strictly
better than `unpriced`; a *provisional* rate is a claim with a known gap, and is labelled as one:

| Model | Input $/MTok | Output $/MTok | Cache read | Cache write 5m | Cache write 1h | Rate source |
|---|---|---|---|---|---|---|
| `claude-fable-5-1` | 10.00 | 50.00 | 0.25 (0.025×) | 1.25× | 2× | `shared/models.md:73` |
| `claude-fable-5`, `claude-mythos-5` | 10.00 | 50.00 | 1.00 (0.1×) | 1.25× | 2× | `shared/models.md:74` (cache read $1/MTok) |
| `claude-mythos-5-1` | 10.00 | 50.00 | 0.25 (0.025×) | 1.25× | 2× | `shared/models.md:75` — same per-token pricing as Fable 5.1 |
| `claude-opus-5`, `claude-opus-4-8` | 5.00 | 25.00 | 0.1× | 1.25× | 2× | `shared/models.md:76` (Opus 4.8's rate, stated there) |
| `claude-opus-4-7`, `claude-opus-4-6` | 5.00 | 25.00 | 0.1× | 1.25× | 2× | **`provisional`** — no rate in the bundle (see below) |
| `claude-sonnet-5` | 2.00 | 10.00 | 0.1× | 1.25× | 2× | `shared/model-migration.md:1291` |
| `claude-sonnet-4-6` | 3.00 | 15.00 | 0.1× | 1.25× | 2× | `shared/model-migration.md:1291` |
| `claude-haiku-4-5` | 1.00 | 5.00 | 0.1× | 1.25× | 2× | **`provisional`** — no rate in the bundle (see below) |
| any other model | `unpriced` | — | — | — | — | — |

**Cache-read rates are per-model, not per-tier.** `claude-fable-5` and `claude-mythos-5` are the
*predécesseurs* of the 5.1 generation: they share `10.00`/`50.00` input/output, but their cache
reads are **$1/MTok (0.1×)** (`claude-api/shared/models.md:74`), four times `claude-fable-5-1`'s
**$0.25/MTok (0.025×)** (`shared/models.md:73`). Conflating the two under-prices every predecessor
cache read by 4×, so they are separate rows; `claude-mythos-5` is an active model and would
otherwise have priced as `unpriced`.

`claude-mythos-5-1` is a **cited** row, not a provisional one: `shared/models.md:75` grants it the
"same capabilities, limits, per-token pricing" as Fable 5.1, so its per-token rates are documented by
reference to Fable 5.1's row above.

**Three models across two rows are `provisional`, not asserted.** `claude-opus-4-7`,
`claude-opus-4-6`, and `claude-haiku-4-5` appear in the first-party model list but carry **no
per-token rate anywhere in the skill bundle** (searched: zero matches in `shared/*.md`). Their
`$5/$25` and `$1/$5` figures are the historical first-party rates, not a documented v1 fact, so the
row is marked `provisional` with a `source` note. Before such a row is treated as authoritative, the
rate **must be checked against the Pricing URL in `shared/live-sources.md`** — the rule this table
must honour, quoted from `shared/cost-optimization.md:233`: *"Per-model prices: always the Pricing URL
in shared/live-sources.md, never remembered rates."* A provisional rate is a claim with a known gap,
not a number the plan asserts.

`claude-opus-5` and `claude-opus-4-8` are **not** provisional: `shared/models.md:76` states Opus 4.8's
rate ($5/$25 per MTok) directly, naming it a drop-in upgrade — Opus 5 shares it, so the same line
covers both rows. The three provisional models stay *priced* rather than `unpriced` because a
labelled provisional figure is more useful than none, and `cost_drift` (API accounts) plus the
`provisional` label surface any divergence.

Every row carries `effective_from` and `source = 'shipped'` (a `provisional` row additionally carries
that note in its `source`, so the gap is queryable, not just prose). A user edit sets `source = 'user'`
and wins; `cost_drift` reports when a shipped or user rate disagrees with reality. Fast-mode rates are
shipped for Opus 5/4.8 only. Anything not in this table is `unpriced`, never guessed — including
Bedrock/Vertex/Foundry partner pricing, which is documented as *different* and is explicitly out of
scope for v1.

The `model_catalog` table, refreshed from `GET /v1/models`, supplies context window and output cap
per model, so ceiling checks come from live data rather than a hand-maintained table.

---

## Quota engine

`internal/quota` computes, from `events`, the burn inside each rolling window — and compares it to
the snapshot.

- **Windows are rolling, not calendar.** A 5-hour window and a 7-day window, evaluated at any
  instant as `[now-5h, now)` and `[now-7d, now)`, per account and per model family. This is a
  windowed query over `events`, and it works identically for both billing modes.
- **Snapshot cross-check.** Each `quota_snapshots` row is compared with the computed burn at that
  instant. `quota.calibration` reports tokens-per-percent, learned from your own history — which is
  the only honest way to translate between "tokens" and "percent of plan" when Anthropic does not
  publish the conversion.
- **Projection.** With limits configured, `quota_window_approaching` fires when the current burn
  rate projects to exhaust the window before it resets. With limits unconfigured, the same
  computation runs and reports `unconfigured` instead of a percentage.
- **Fail-soft.** An unreachable or unparseable snapshot marks that window `unavailable` in
  `quota_snapshots.status` and in `GET /api/sources`. Tokens keep being recorded; the quota view
  says what it does not know.

---

## Analyzer rule catalogue

The flagship of deepseek-lens was "what Anthropic-shaped settings DeepSeek silently ignored". Claude
is the native endpoint, so the divergence class is different — but the *shape* of the finding is
identical: **something that costs you money, raises no error, and you would never notice.** Cache
behaviour is the canonical case, and `thinking.display` defaulting to `omitted` is a literal silent
change.

Every rule below is grounded in documented API behaviour or in the verified JSONL schema; none is
speculative. Tiering criterion: **T1** is a rule whose absence makes a headline claim of this plan
untrue; **T2** is real and cheap but not load-bearing for v1.

### T1 — v1

| Kind | Sev | Fires when |
|---|---|---|
| `cache_prefix_invalidation` | warn | `cache_creation_input_tokens` ≈ full conversation size on every turn of a session, while `input_tokens` stays small — a silent invalidator is rewriting the prefix upstream of the breakpoint, so every turn pays full price. **The detection is a token-ratio signal source B can compute on its own; the proxy's captured body upgrades it from "your cache keeps breaking" to "this byte broke it" (localisation).** |
| `cache_prefix_below_minimum` | warn | A `cache_control` breakpoint is present but the prefix is under the model's minimum cacheable length (512 on Opus 5/Fable 5/5.1/Mythos; 1024 on Opus 4.8/Sonnet 5/4.6/4.5; 2048 on Opus 4.7; 4096 on Opus 4.6/4.5/Haiku 4.5) — so `cache_creation_input_tokens` is 0 and the marker did nothing. The minimum is **not monotonic across generations**, which is why it is a table, not a rule of thumb. |
| `cache_breakpoints_exceeded` | error | More than 4 `cache_control` breakpoints in one request (a 400). |
| `cache_write_never_read` | warn | Write tokens recorded with no subsequent read of that prefix within the TTL — the write premium was paid for nothing. |
| `cache_ttl_mismatch` | info | `ephemeral_1h` written where the start-to-start gap between reads stayed under 5 minutes (five-minute TTL was strictly cheaper), or a 1-hour write with no read in the 5–60 minute band that is the only window where the doubled write pays off. |
| `cache_expired_between_turns` | info | A prefix that was cached is re-written instead of read, with a start-to-start gap longer than the TTL it was written with. |
| `cache_invalidated_by_tools` | warn | The `tools` array or top-level `system` changed between consecutive calls in a session, invalidating the entire cache from position 0. The *effect* (whole-cache invalidation from position 0) is visible in source B's usage numbers; the *cause* — which array changed — requires the proxy's captured bodies. |
| `cache_concurrent_write_race` | info | N overlapping calls with identical prefixes: an entry is only readable after the first response *begins* streaming, so all N paid full price. |
| `thinking_budget_rejected` | error | `thinking.budget_tokens` on a model that now returns 400 (Fable 5/5.1, Opus 5/4.8/4.7, Sonnet 5). The direct analogue of deepseek's `budget_tokens_ignored`, except here it fails loudly. |
| `thinking_display_omitted` | info | Thinking was on and billed, but `thinking.display` defaulted to `omitted` on the newest models (a silent change from the 4.6 generation, where it was `summarized`) — you are paying for reasoning you are not receiving. |
| `max_tokens_truncation` | warn | `stop_reason: max_tokens` — the turn was cut off mid-thought and will be retried at more cost. |
| `refusal` | warn | `stop_reason: refusal`. `stop_details.category` is read **only** when `stop_reason == "refusal"`, as the API requires. |
| `stream_incomplete` | error | The SSE stream ended without `message_stop`. |
| `rate_limited` | warn | HTTP 429, with `retry-after` and any `anthropic-ratelimit-*` remaining/reset values recorded. |
| `overloaded` | error | HTTP 529. |
| `upstream_error` | error | Error object inside a 200 body, or a transport failure. Carried over verbatim. |
| `auth_kind_anomaly` | warn | An `admin` credential used against the Messages API, or a subscription account observed sending an `api_key` credential — the call is billed to a different model than the account it was attributed to. |
| `quota_window_approaching` | warn | Projected to exhaust a configured subscription window before it resets. |
| `cost_drift` | warn | Computed cost for a `(day, model)` diverges from the Admin cost report beyond threshold — the price table is stale. |
| `api_equivalent_cost` | info | On a subscription row: what this call would have cost at API rates. Labelled hypothetical, never added to a billed total. |
| `source_mismatch` | error | The same `request_id` arrived from two sources with disagreeing token counts. |
| `analyzer_panic` | error | A rule panicked. Carried over. |

### T2 — v1.1 (real, not load-bearing)

`prefill_rejected`, `forced_tool_choice_rejected` (Fable 5.1/Mythos 5.1), `priority_tier_unsupported`
(**only** the documented exclusions: Priority Tier is not supported on Fable 5.1 or Mythos 5.1 —
`shared/model-migration.md:1670`; the plan previously also listed Opus 5 and Sonnet 5, which the
authority does **not** state, so those are removed from the rule, not asserted),
`fast_mode_unsupported` (documented part: Opus 5 / Opus 4.8 only, first-party API only,
`shared/platform-availability.md:50`; the *"not with Batch or Priority"* clause is **not** documented
in any readable skill file and is marked `T2-unverified` alongside this rule rather than stated as
fact), `inference_geo_unsupported_model`, `effort_unsupported_model`,
`thinking_budget_deprecated` (still works on Opus 4.6/Sonnet 4.6), `model_retired`,
`model_mapping_drift` (fires on the observed `model_requested` vs `model_resolved` split introduced
by client-side `ANTHROPIC_MODEL`-style remaps — the configured `--model-map` is dropped, see
*Carried over*), `price_table_stale`, `batch_discount_applied`.

The T2 rules that cannot be confirmed from the skill bundle (its `SKILL.md` is injected, not a file
on disk) are labelled `T2-unverified` in the rule catalogue so a reader does not treat them as
documented. They are T2 (v1.1) and do not gate v1.

The README's warning-kind table stays mechanically checked against `internal/analyze/kinds.go` by
`readme_test.go`, exactly as in deepseek-lens. Rule spellings are therefore settled in **one** place:
the below-minimum rule is named `cache_prefix_below_minimum` everywhere — the T1 table, `kinds.go`,
the README kind table, and the tests — because a variant spelling (`cache_breakpoint_below_minimum`
was used in an earlier draft) is a build failure under that test.

---

## CLI

18 subcommands: deepseek-lens's 12 (unchanged names and flags) plus 6.

| Command | What it does |
|---|---|
| `clens serve` | Proxy + dashboard + consumer + the three collectors + the scheduler, in one process |
| `clens doctor` | Resolved config, PASS/WARN/FAIL checks, per-source health, port-collision check |
| `clens ls` / `show` / `tail` | Call log, one call's detail, live follow |
| `clens warnings` | Grouped by kind, `--detail` for one row per occurrence |
| `clens sessions` | Sessions with turns, tokens, the **two labelled cost figures** (API-billed / API-equivalent — never one mixed total, never `$0.00` for a subscription or unpriced session), warning counts |
| `clens stats` | Window totals **split by `billing_mode`** (never one merged total), per-model split, unpriced count; `--by model\|day\|session\|project` (`project` added for the tech plan's *Breakdown by project (Code)*), `--period`, granularity as today |
| `clens prices` | Effective table; `--set`, `--unset`, `--edit` |
| `clens replay <id>` | Re-issue a captured call; `--set`, `--diff`, `--dump`, `--yes` |
| `clens purge` | Delete by age or the unpriced predicate; `--dry-run`, `--yes`, `--vacuum` |
| `clens export` | Every stored event as JSON lines; `--csv` (the tech plan's export) |
| **`clens ingest`** | Backfill from the JSONL sources once, incrementally. `--rebuild` re-reads from byte 0 |
| **`clens refresh`** | Run every non-proxy collector once (JSONL tail + snapshots + admin pull) — the cron/Task Scheduler entry point |
| **`clens quota`** | Rolling 5h/7d burn vs configured limits, last snapshot, calibration |
| **`clens accounts`** | Configured accounts, `auth_kind` distribution, plan and limit state |
| **`clens models`** | Model catalogue, and which models are `unpriced` |
| **`clens reconcile`** | Computed vs billed, per day and model, with drift |

---

## API surface and dashboard

All 15 deepseek-lens routes are kept (including the three pagination headers, the unpaginated
`GET /api/warnings/summary`, the SSE `/api/stream`, and `/api/health`). **One kept contract is
deliberately restated rather than silently changed:** `GET /api/sessions` no longer returns a single
per-session `total_cost_usd` (the deepseek-lens `store.Session.total_cost_usd`); the per-session cost
is split into `total_cost_usd` (API-billed) plus `total_api_equivalent_cost_usd` (subscription), each
NULL when that side is empty, so the route can never return a figure that mixes billing models or
`$0.00` for a subscription session (see *Storage schema*). **Eight routes are added (five GET, three
POST).**

| Method | Path | Guard | Purpose |
|---|---|---|---|
| GET | `/api/sources` | none | Per-source health: last success, last error, rows written, cursor position, status |
| GET | `/api/quota` | none | Windows, computed burn, configured limits, last snapshot, calibration |
| GET | `/api/accounts` | none | Configured accounts and their billing model / plan state |
| GET | `/api/models` | none | Catalogue plus per-model pricing coverage |
| GET | `/api/reconcile` | none | Computed vs billed by day and model, with drift |
| POST | `/api/ingest` | Origin/Host | Trigger a collector run (non-destructive → always on) |
| POST | `/api/accounts` | Origin/Host | Set an account's plan and limits (no credentials) |
| POST | `/api/secrets` | Origin/Host | Set/replace the claude.ai `sessionKey` or Admin key (the plan's re-auth flow) |

The write-route guard count goes from three to six, all sharing the existing parameterized
`replayOriginReject` Origin/Host allowlist.

**Every aggregate route splits on `billing_mode`.** `/api/stats`, `/api/reconcile`, and the Overview
tab group or filter by `billing_mode`, and a mixed fixture returns two labelled totals rather than
one sum (tested — see *Test strategy*). The schema already guarantees `events.cost_usd` cannot hold a
subscription figure, but the count/token aggregates are a second place the two models could be
blended, so they carry the split explicitly.

**Write routes reach `secret` / `config` / `ingest` through injected function-value seams, not
imports.** `internal/api` and `internal/web` never import `internal/secret`, so `POST /api/secrets`
cannot call `secret.Save` directly. Instead the composition root (`cli/serve`) injects the writes,
the same shape deepseek-lens uses for `SetPricing` → `pricing.Save`
(`internal/api/api.go:102-106`, wired at `internal/cli/serve.go:115`):

- `api.SetCredentialWriter(fn func(name, value string) error)` ← `secret.Save`
- `api.SetAccountWriter(fn func(...) error)` ← config/accounts save
- `api.SetIngestTrigger(fn func(ctx) error)` ← `ingest.RunOnce`

An unwired seam returns `503`, exactly as `POST /api/prices` does without `SetPricing`. The
import-direction guarantee (`api`/`web` never import `secret`) is therefore stated in terms of these
function values, not in terms of the handler body.

**Dashboard tabs:** Overview · Calls · Sessions · Warnings · Stats · **Sources** · **Quota** ·
**Reconcile** · **Models** · Settings.

The **Sources** tab is what makes the tech plan's core promise — "if one collector breaks, the
others keep working" — trustworthy rather than aspirational: a dead collector is a visible red row,
not a quietly short chart.

---

## Security posture

Everything in deepseek-lens's posture carries over: loopback-only binding with
`--allow-remote` as a documented footgun, redaction of `x-api-key`/`Authorization`/`Cookie` before
the tee, full-body storage under a documented cap, no dashboard auth with dashboard auth stated as
a prerequisite for any non-local deployment, the Origin/Host allowlist on every write route, and
the `RedactCheck` startup self-test.

**One deliberate departure, and it is the most important security decision in this plan:**
deepseek-lens could say "credentials are not stored" because it never needed one. Source C and
source D **require** credentials that clens must hold — the claude.ai `sessionKey` cookie and the
Admin API key. That is a real expansion of the blast radius and it is handled explicitly:

- They live in `~/.clens/secrets.toml` — **outside** the database, so a copied or backed-up
  `lens.db` contains no credential. Protection is **platform-specific and stated as such**:
  - **POSIX**: the file is `0600` and its directory `0700`, applied with `os.Chmod`.
  - **Windows** (the target platform): the plan does **not** rely on permission bits, because Go's
    `os.Chmod` / `os.OpenFile` `perm` argument only toggles the read-only bit on Windows — a `0600`
    there is a **no-op**, not a control. Access is restricted by an explicit **ACL** applied with
    `icacls` through `os/exec`. A control that does not run is worse than no control, so the
    mechanism is spelled out:
    - **The principal is resolved in Go and passed as an argument — never a shell string.** Go's
      `os/exec` invokes no shell and expands no `${…}`, so a `"${USERNAME}"` argument would reach
      `icacls` literal and fail with *"No mapping between account names and security IDs was done"*.
      `secret.Save` resolves the account first (`os/user.Current()`, or the account SID;
      `os.Getenv("USERNAME")` as a fallback) and passes the args as a slice, so the account resolves
      or the command exits non-zero.
    - **A newly created file *does* inherit its parent directory's ACEs — that is what inheritance
      means — and `/inheritance:r` is what removes them.** The file does **not** "start with no ACEs
      to inherit": it inherits `%USERPROFILE%`'s `SYSTEM` / `Administrators` / user ACEs at creation,
      whether it is created directly in `~/.clens` or in a subdirectory `internal/secret` creates,
      because that subdirectory *itself* inherits from `%USERPROFILE%`. So the ACL step is
      `/inheritance:r` (drop the inherited ACEs, leaving a protected DACL) followed by
      `/grant:r <user>:F` (install the single explicit owner ACE) — `/inheritance:r` is
      load-bearing here, not redundant.
    - **The complete ACL is applied to a temp file and verified *before* it is renamed into
      place.** `secret.Save` writes a temp file in the **same directory** as `secrets.toml`, applies
      `exec.Command("icacls", tmpPath, "/inheritance:r", "/grant:r", user+":F")`, reads the DACL back
      (`icacls tmpPath`) and asserts the resulting principal set is exactly the intended one, and
      only then `os.Rename`s it over `secrets.toml`. **This ordering is what makes fail-closed *true*
      rather than merely asserted:** `icacls` applies its arguments in sequence, so had it run
      directly on the live file, `/inheritance:r` would already be committed when a later `/grant:r`
      failed to resolve the account — leaving an **empty DACL that denies everyone** and stranding an
      existing `secrets.toml` unreadable, which the "does not overwrite an existing credential"
      promise does not cover (its *permissions* would be clobbered before the grant is even
      attempted). The temp-then-rename path removes that window: a failure at any step deletes the
      temp and leaves the live file byte- and ACL-identical to before, and the rename is the
      **single atomic commit point** — the credential and its verified ACL land together or not at
      all. The rename replaces the live file's ACLs wholesale, which is also why no `/reset` pass is
      needed (a freshly created temp file has only *inherited* ACEs, and `/inheritance:r` drops
      those).
    - **The one safe direct-on-target case is the file that does not yet exist.** When there is no
      `secrets.toml` to strand, `secret.Save` may create it in place and apply the same ACL directly:
      there is no prior credential and no prior permissions to lose. Every path where the file
      already exists goes through temp-then-rename; the direct path is called out here precisely
      because it is the only case where a mid-sequence failure has nothing to destroy.
    - **The exit status is checked, and the failure is fail-**closed**.** A non-zero `icacls` exit —
      or a read-back DACL that does not match the intended principal set — means the ACL was not
      applied: `secret.Save` **refuses to write the credential** (and, when a file already exists,
      does not overwrite it), the caller reports the failure, and `clens doctor` shows a **FAIL** in
      plain words stating that the credential file is **not protected** — rather than leaving an
      org-wide Admin key on disk under the permissive `%USERPROFILE%` ACL while the plan claims
      otherwise. A control that fails open is worse than no control, because `doctor` then reports
      protection that is not there.
    - The `golang.org/x/sys/windows` ACL API is the fallback if shelling out is unavailable (the one
      path that would add a dependency; `icacls` is stdlib, so the "exactly one non-stdlib
      dependency" claim survives the default choice).
  - **Stated plainly: on Windows, permission bits alone are not a control.** This file holds an
    org-wide Admin key and a full-account `sessionKey`, so the ACL is the control that matters, and
    the plan no longer claims a mode it does not set. The testable consequences are two: after a
    successful save, the DACL on `secrets.toml` contains **exactly** the one intended principal (the
    same read-back the mechanism performs); and if the ACL cannot be applied — or the read-back does
    not match — the credential is **not** stored, **not** used, and reported unprotected by `doctor`,
    with any pre-existing file left untouched rather than silently downgraded or permission-clobbered.
- `internal/secret` is the only package that reads or writes the file. `internal/api` and
  `internal/web` never import it — `POST /api/secrets` writes through the injected
  `SetCredentialWriter` seam, which the composition root wires to `secret.Save` (see *API surface*) —
  so the API can report *whether* a credential is present and when it last worked, and cannot return
  its value.
- `clens doctor` reports the **actual** protection level it observes (the POSIX mode, or the Windows
  ACL entries on the file) rather than asserting a mode that was never applied. If the ACL step
  failed, `doctor` shows a **FAIL** naming the file as **unprotected**, and no credential is used —
  the fail-closed path above, not a silent downgrade.
- Both are added to the redaction set, so an accidentally captured `sessionKey` header or
  `sk-ant-admin…` key is redacted before insert, and `RedactCheck` scans stored headers for both
  patterns at startup.
- Neither is ever logged. Collector errors report a status and a reason, never the credential.
- The Admin key is **organization-wide read**. `clens doctor` states this in plain words when one
  is configured, and the CLI refuses to store one without `--yes`.
- The token endpoint surface is the same as deepseek-lens's: none. There is no signing, no session,
  no OAuth flow — a bearer token in a protected file (POSIX modes, or a Windows ACL), used only
  against `claude.ai` and
  `api.anthropic.com` respectively, and nothing else. `--allow-remote` remains the one way to
  expose the dashboard, and it remains a footgun.

Out of scope in v1, stated rather than implied: Bedrock/Vertex/Foundry pricing and auth, multi-user
access, any hosted deployment, and body-content secret scanning (deepseek-lens's own open
question, unchanged here).

---

## Infrastructure

- **Governance**, identical to deepseek-lens: `.githooks/commit-msg` rejects any commit not
  starting with `GI#<n>`; `.githooks/pre-commit` delegates to the machine-wide secret scan and
  refuses every commit until it is installed; `branch-guard.yml` and `main-guard.yml` enforce the
  PR flow. This repo has no `develop` yet, so v1 lands on `main` via a `GI-1-…` branch, and
  `develop` is introduced when a second story needs it. **This is a deviation from deepseek-lens's
  branch policy and is recorded in the decision log.**
- **`.gitignore`**: `*.db`, `*.db-wal`, `*.db-shm`, `config.toml`, `accounts.toml`,
  `secrets.toml`, `/**/*review*/`, and the built binary.
- **CI**: the two workflow guards only — no test CI, matching deepseek-lens. `go build ./...`,
  `go test ./...`, and `go vet ./...` are the developer's own gate before every PR.
- **Dependencies**: Go 1.24 stdlib plus `modernc.org/sqlite` (pure Go, no cgo). The collectors use
  raw `net/http` rather than an SDK because the Admin usage/cost report endpoints are **explicitly
  not covered by any Anthropic SDK** — they are documented as curl-only. This is recorded so a
  future reader does not "fix" it into an SDK dependency. The Windows credential ACL uses `icacls`
  through stdlib `os/exec`, so `modernc.org/sqlite` remains the **only** non-stdlib dependency; the
  `x/sys/windows` ACL fallback is the single alternative that would add a second, and is noted as
  such rather than as the default.
- **No network egress** beyond `api.anthropic.com`, `claude.ai`, and the configured upstream.

---

## Test strategy

Ordered by how much each protects the product. The first seven are carried over from deepseek-lens
unchanged, because they guard the same invariants.

1. **TTFB no-buffering** (proxy) — fake upstream slow-streams SSE; assert the client sees the first
   event before upstream sends its last. *The hard gate.*
2. **Byte identity** (proxy) — client and upstream bodies unchanged end to end, streaming and not.
3. **Split-chunk SSE** (parse) — an event split across two chunk boundaries parses correctly.
4. **Non-blocking sink** — with the consumer stopped, N writes complete without blocking and the
   drop counter reaches N.
5. **Store round-trip** — write, read back, WAL confirmed, redaction verified, FK cascade verified.
6. **Session keys** — same prefix within window groups; beyond window splits; header override wins.
7. **Cost arithmetic** — per-class rounding, not on the total, exercised through the batch modifier
   (`service_tier: "batch"` ×0.5) — the ported `TestComputeBatchRoundsPerClass`. The peak-pricing test
   it replaces had no subject here once F9 dropped the peak window.
8. **JSONL dedup** — the regression test for a real, measured defect. On the authoring machine,
   **35 of 47 `requestId`s in a real Claude Code log carry two assistant lines with byte-identical
   `usage` objects**, because one line is written per content block. A naive sum inflates token
   counts by roughly 1.75×. The test asserts a fixture with duplicated lines yields the tokens of
   the distinct requests, not the sum of the lines.
9. **JSONL tolerance** — the 13 non-assistant line types observed in real logs (`attachment`,
   `file-history-snapshot`, `atis-latch`, `file-history-delta`, `ai-title`, `last-prompt`,
   `queue-operation`, `mode`, `pr-link`, …) are skipped; an unknown `type` and a malformed line are
   counted into `ingest_state` and do not stop the tail. A fixture in each observed token shape —
   the `ephemeral_*` split and the flat `cache_creation_input_tokens` — parses to the same prompt
   total, the flat shape flagged `approximate`.
10. **JSONL resume** — byte-offset cursor survives append, truncation, and rotation, and re-reading
    from an offset does not duplicate rows (via `request_id`).
11. **Cross-source dedup (fixture, split from the proof).** Two parts, so the test never asserts the
    thing it is meant to verify:
    (a) **Fixture-level merge test** — a fixture sourced from a real captured proxy/JSONL pair where
    one exists (otherwise built with an id *known* to be equal on both sides) asserts that an insert
    from each source yields one row, `first_source` preserved, `source_refs` listing both, tokens
    **not** summed, and a disagreement between two *complete* sources raising `source_mismatch`. A
    second fixture where A is a **truncated** capture (`capture_complete = false`, empty or partial
    `usage`) and B is complete asserts that **B's tokens become the row's tokens** and that **no**
    `source_mismatch` fires — one source having nothing to contribute is not a disagreement. **The
    same merge is asserted on the warning ledger** (F4.1): the fixture raises at least one finding
    kind from each source, and the test asserts that after both inserts each kind appears **exactly
    once** on the row (`COUNT(*) = COUNT(DISTINCT kind)` per `event_id`, and the `UNIQUE(event_id,
    kind)` upsert is what makes it so) and that the session's `warning_count` **equals the distinct
    kind count** — not the sum of the two analyzer runs. A test that only checked the row count would
    pass on the buggy v4, which is why the assertion is on the kinds and the session count. This
    exercises the *merge*, not the id equivalence.
    (b) **Live id-equivalence verification** — a prerequisite step (not a fixture), capturing one live
    response's `request-id` header and the matching Claude Code JSONL line and asserting equality, its
    result recorded in this plan. Until it runs, the equivalence is an assumption and the fallback key
    is primary (see *Cross-source identity*).
12. **Prompt-total and billing-mode invariants** — (a) `total_prompt_tokens == input + cache_write_5m
    + cache_write_1h + cache_read` on every row, and the classic "input_tokens looks small so the call
    was cheap" misreading is asserted against; (b) a subscription row has **`cost_usd IS NULL`
    unconditionally**, and `api_equivalent_cost_usd IS NOT NULL` **unless** the row is `cost_source =
    'unpriced'` — an unpriced subscription call legitimately carries NULL on *both* columns, which is
    the labelled state the "no invented numbers" rule requires, not an invariant violation — and an
    api row is the reverse **unless** it is `cost_source = 'unpriced'`, where `cost_usd` is NULL and
    `api_equivalent_cost_usd` stays NULL (an unpriced API call is the same labelled state, never
    `$0.00`); a mixed fixture passed through `/api/stats` yields two labelled totals,
    never one merged `SUM(cost_usd)`, and the same mixed fixture read through `/api/sessions` yields
    the two labelled **session** totals (F2.2) — never one mixed figure and never `$0.00` for the
    subscription side.
13. **Cache-invalidation detector** — a synthetic healthy loop (reads growing, writes small) raises
    nothing; a synthetic invalidator loop (writes ≈ full conversation every turn) raises
    `cache_prefix_invalidation`.
14. **Cache minimum table** — a marker below the model's minimum raises
    `cache_prefix_below_minimum`, and the non-monotonic cases (a 3K prefix caches on Opus 5 but not
    on Opus 4.6) are asserted explicitly.
15. **Reconciliation** — computed vs billed drift fires at the threshold and not below it.
16. **Snapshot tolerance** — a renamed, missing, or extra window field on the unofficial endpoint
    records `parse_error` or an extra `window` row and does not crash; a 401 records `unauthorized`.
17. **Collector isolation** — with snapshots failing, JSONL and proxy ingestion continue and
    `GET /api/sources` shows the failure. This is the tech plan's headline promise, tested.
18. **Secret containment** — `internal/api` and `internal/web` never surface a credential value; a
    `sessionKey` in a captured header is redacted; `RedactCheck` catches both new patterns.
19. **Redaction integration** (carried over) — including the multi-valued-header bypass that was
    deepseek-lens's v1 blocker (`Header.Get` returns only the first value), with both values set.
20. **Admin collector idempotence** — running the usage and cost collectors twice over the same
    7-day window yields the same row counts and the same totals, not double (UNIQUE natural key +
    UPSERT); an overlapping re-fetch updates in place, leaving no duplicate `(day, model)` rows.
21. **Retry keeps two rows** — two proxy calls with byte-identical bodies but distinct response
    `request-id`s (a 429/529 retried) produce two `events` rows and two warnings, never one merged
    row — the `rate_limited`/`overloaded` signal is preserved.
22. **Subagent discovery** — a `…/<sessionId>/subagents/agent-*.jsonl` file is discovered by the
    recursive walk, ingested, and attributed to its parent session with `is_sidechain = true`; a
    top-level transcript gets `is_sidechain = false`.

---

## Risk areas

| Risk | Mitigation |
|---|---|
| **Double-counting a turn seen by two sources** | `request_id` UNIQUE + merge-not-add semantics, tested against the measured 35/47 duplicate case, plus `source_mismatch` on disagreement; the header↔`requestId` equivalence is verified live before the merge is relied on, with a fallback key if it fails. The **warning** ledger carries the same guarantee: `warnings` is `UNIQUE(event_id, kind)`, the analyzer attach is an upsert, and `warning_count` is re-derived from `warnings` — so a merge that re-runs the seam cannot raise a finding kind twice or inflate the count (test 11a) |
| **Summing subscription and API figures** | Enforced by column, not convention: subscription rows have `cost_usd` NULL and carry `api_equivalent_cost_usd`; `billing_mode` on every row; every aggregate surface groups by it (invariant 5, tested) |
| **Inventing a plan limit or a price** | `unconfigured` / `unpriced` as first-class labelled states; shipped rates cited to a skill file, or `provisional` with a named verification step — never silently remembered; limits learned from your own snapshots and confirmed, never assumed |
| **Trusting the local price table** | `cost_drift` reconciles computed against Anthropic's actually-billed figure — but **only for API accounts**, where a billed figure exists. For subscription accounts (six of the seven shapes) there is no billed counterpart, and the guard is `model_catalog` refresh plus the `unpriced`/`approximate` labels, with an optional staleness signal |
| **claude.ai's endpoint is undocumented and can change or break** | Tolerant, schema-agnostic window extraction; `status` recorded per snapshot; the other three sources unaffected; the Sources tab makes it visible (answers the tech plan's open question) |
| **The cookie expires** | Explicit re-auth via `clens accounts` / `POST /api/secrets` **and** a visible degraded state — never a silent failure (answers the tech plan's open question) |
| **The Admin key is org-wide** | Protected by the platform control that actually applies — POSIX `0600` on Unix, an explicit Windows ACL on Windows (never the `0600` token Go ignores on Windows) — outside the DB, redacted, never logged, never served. On Windows the ACL (`/inheritance:r` + `/grant:r`) is applied to a same-directory temp file, the DACL read back and verified, and only then renamed over `secrets.toml`, so a mid-sequence failure leaves the live file untouched; the ACL step **fails closed** (ACL not applied, or read-back mismatch → credential not written, not used, `doctor` FAILs), and `doctor` reports the real protection level and the key's scope; storing one requires `--yes` |
| **JSONL format changes between Claude Code versions** | Tolerant parser, unknown types counted not fatal, `client_version` recorded per row so a format break is attributable to a version |
| **Buffering the stream to count tokens** | Invariant 1 + the TTFB hard gate |
| **Hot-path stall on a full observer queue** | Bounded sink + drop counter, unchanged |
| **A collector crashing the process** | Each collector recovers its own panics; per-source error state; fail-open extended to the collectors |
| **Model rates change** | `effective_from` per row, `cost_drift` detects staleness, `model_catalog` refreshes context windows live |
| **Port collision with an existing deepseek-lens install** | `clens` defaults to 8797/8798 (not lens's 8787/8788); `doctor` fails a check if the port is occupied |
| **`ANTHROPIC_BASE_URL` pinned in `~/.claude/settings.json`** | `doctor`'s `client_config` check reads the file and reports the effective upstream and model mapping, because that env block overrides the shell |
| **Scope creep into a full client linter** | Every rule attaches a fact to a captured row; none reads or edits the user's code |

---

## Self-review

**As a senior engineer.** The design's spine is unchanged from deepseek-lens, which is the point —
the hot/cold split, the bounded sink, the one-way dependency graph, and the fixed pipeline order
were validated by a shipped tool and there is no reason to re-litigate them. The one structural
addition is the second and third ingest path. Two schema decisions carry it, and both are now
statements a test can check rather than claims to take on faith: (1) `admin_usage_days` and
`admin_cost_days` stay separate tables with UNIQUE natural keys and UPSERT inserts, so per-day
billed data cannot be silently folded into per-call data *and* cannot be double-counted by a
re-run; (2) the subscription-vs-API separation the requirement actually names is enforced in
`events` itself — a subscription row leaves `cost_usd` NULL and writes `api_equivalent_cost_usd`, so
`SUM(cost_usd)` cannot merge the two billing models whether or not a JOIN is involved. An earlier
draft claimed the two-table split made the merge "structurally impossible" while the plan's own
`/api/reconcile` performed a JOIN; that overstatement is replaced by the column-level rule, which is
the one the schema can actually enforce. I would normally push back on four
sources in v1 — the honest simplification is to drop source C entirely, since it is undocumented
and A+B+D already answer most questions. I kept it because quota is *the* question a subscription
user has and nothing else answers it, but it is the first thing to cut if v1 needs to shrink. The
absence of a migration framework is accepted on the same terms deepseek-lens accepted it:
rejected the moment a shipped column changes.

**As a QA engineer.** The failure modes that matter here are silent, which is why the test list
leads with timing and state rather than final output. Three specific ones I would stake the release
on: the JSONL duplicate-usage trap is *measured*, not hypothetical (35 of 47 requests on real data),
and a tool that inflates its own numbers by 75% is worse than no tool; the total-prompt invariant
catches the most common misreading of Claude's usage object (`input_tokens` is the uncached
remainder, not the prompt); and collector isolation is the plan's headline promise, so it is tested
rather than asserted. Accepted gaps, stated: no test for Anthropic changing a documented rule (the
rules are table-driven where they can be), no multi-machine sync (explicitly out of scope), and no
browser-automation coverage of the dashboard — verification is against the assets the running
server returns plus a manual eyeball, exactly as in deepseek-lens, whose `app.js` has real logic and
no harness.

**As a security engineer.** The asset is the same as deepseek-lens's — not a credential, but the
*content*: every prompt, every file the agent read, concentrated in one file — so loopback binding
and pre-insert redaction remain load-bearing defaults. What is genuinely new here is that this tool
now **stores credentials it must hold**, which deepseek-lens never did. The sessionKey is a
full-account credential for claude.ai and the Admin key is org-wide read; both are therefore kept
outside the database in a file whose protection is **platform-specific and actually applied** —
POSIX modes on Unix, an explicit Windows ACL on Windows, never the `0600` mode token that Go ignores
on the target platform. They are readable only by `internal/secret`, absent from the API and web
layers by import direction (write routes go through an injected seam), added to the redaction set,
and their actual protection level is reported by `doctor` in plain words. The re-auth route is a
write route on the Origin/Host allowlist and nothing more. Accepted risks, stated: `--allow-remote`
remains a footgun; the dashboard has no auth, which is acceptable only while loopback-bound and is a
prerequisite to fix before any hosted deployment; replay is billable and stays off by default behind
`--replay`; and no secret scanning is applied to body *content*, which remains deepseek-lens's open
question rather than a solved one here.

---

## Bead sequence

Core first (01–10) builds the proxy-to-row pipeline and its analyzers: scaffold, sink, proxy,
decode, extraction, store, pricing, the consumer that wires them, then the analyzer rules. The
collectors and accounting features follow (11–15), then the surfaces — the dashboard API (16), the
CLI (17), and the routes/tabs on top (18) — then documentation and acceptance (19).

| Bead | What it delivers | Category | Depends on |
|---|---|---|---|
| br-GI-1-01 | Repo scaffold, module init, config, accounts, `secret`, `doctor` | chore | — |
| br-GI-1-02 | Non-blocking capture sink | feat | 01 |
| br-GI-1-03 | Transparent proxy core, upstream `api.anthropic.com`, redaction | feat | 02 |
| br-GI-1-04 | `Content-Encoding` decode + SSE/JSON response parsing | feat | 01 |
| br-GI-1-05 | Request metadata + Claude usage extraction (6 token classes, speed, tier, stop_reason) | feat | 04 |
| br-GI-1-06 | SQLite store, schema, single ingest writer, purge | feat | 01 |
| br-GI-1-07 | Pricing engine (6 classes, speed/tier modifiers, shipped table) + `catalog` | feat | 05, 06 |
| br-GI-1-08 | Consumer pipeline (sink → parse → session → account → **cost** → insert → analyze) | feat | 03, 05, 06, **07** |
| br-GI-1-09 | Analyzer engine + T1 cache rules | feat | 07, 08 |
| br-GI-1-10 | Analyzer T1 response/thinking/billing rules | feat | 09 |
| br-GI-1-11 | `jsonlogs` recursive tailer, `request_id` dedup, merge semantics, `ingest_state` (the live header↔`requestId` verification in *Cross-source identity* is a prerequisite step for its merge semantics) | feat | 06 |
| br-GI-1-12 | `snapshot` poller + `quota` engine + calibration | feat | 06, 07 |
| br-GI-1-13 | `adminrep` collector + `reconcile` + `cost_drift` | feat | 06, 07 |
| br-GI-1-14 | `ingest` orchestration, scheduler, collector isolation + per-source health | feat | 11, 12, 13 |
| br-GI-1-15 | CLI additions (`ingest`, `refresh`, `quota`, `accounts`, `models`, `reconcile`) | feat | 11, 12, 13 |
| br-GI-1-16 | Dashboard: JSON API, SSE broker, embedded web assets | feat | 06 |
| br-GI-1-17 | CLI subcommand dispatch + the 12 carried-over commands (incl. `serve` wiring) | feat | 03, 07, 08, 14, 16 |
| br-GI-1-18 | API routes, Sources/Quota/Reconcile/Models tabs, hand-rolled charts | feat | 14, 15, 16 |
| br-GI-1-19 | README, context docs, governance workflow, end-to-end acceptance | docs | 17, 18 |

Dependency graph (re-derived so **no bead depends on one scheduled later** — every edge points to a
lower number): the pipeline spine is `01 → 02 → 03 → 08`, `01 → 06`, and `06 → 07 → 08` — the
consumer (08) needs the proxy (03), the store (06), **and the pricing engine (07)**, because its
`cost` step is what 07 builds. Analyzer rules follow the engine: `07,08 → 09 → 10`. Three collector
branches hang off the store: `06 → 11`, `06,07 → 12`, `06,07 → 13`, converging on `14` and `15`
(orchestration and the CLI additions). The dashboard API (16) reads the store. The CLI dispatch bead
(17), including `serve`, is the composition root: it wires the proxy (03), pricing (07), consumer
(08), collectors (14) and dashboard API (16) that already exist, which is why it sits after them. Bead
18 lays the new routes and tabs over `14/15/16`; 19 closes on `17,18`.

---

## Decision log

| Decision | Choice | Alternative rejected |
|---|---|---|
| Stack | Go 1.24, single binary | Python/FastAPI (the tech plan's stack) — would mean reimplementing 13 proven packages and rebuilding the hot path in a slower runtime; deepseek-lens's own decision log already rejected Python for the hot path |
| Source count | Four (proxy + JSONL + snapshot + admin) | Three (the tech plan's set) — would lose the proxy's body-based *localisation* (the two token-ratio cache rules still fire from JSONL) and its non-Claude-Code client coverage |
| Storage shape | Fine-grained `events` and coarse `admin_*_days` as separate tables; within `events`, subscription rows write `api_equivalent_cost_usd` and leave `cost_usd` NULL | One unified table — would let a daily billed total be summed with a per-call computed total; and a single `cost_usd` column would let a subscription figure and an API figure be summed directly, which is the one arithmetic error this tool must not make |
| Cross-source identity | `request_id` UNIQUE, merge-not-add; the header↔`requestId` equivalence is an assumption **verified live** before the merge is relied on, with a fallback key if it fails | Row per (source, turn) with a dedup view — leaves the inflation bug reachable by any query that forgets the view |
| Quota snapshots | One row per `(window)` | Fixed `session_pct`/`weekly_pct` columns (the tech plan's schema) — would silently drop the per-model windows some plans return |
| Plan limits | Empty by default; learned from your own snapshots and confirmed | Shipped per-plan limit table — inventing a number is the failure mode deepseek-lens already named |
| Price table | Ships populated with documented (cited) first-party rates, plus `provisional` rows where the bundle lists a model but prices it nowhere — verification step named | Empty by default (deepseek-lens's choice) — that choice was forced by having no authoritative source, which is no longer true |
| `cost_usd` provenance | Computed locally, verified against the Admin cost report | Trusting the local table — `cost_drift` detects staleness **for API accounts**; subscription accounts have no billed counterpart and are guarded by `model_catalog` refresh + `unpriced`/`approximate` labels instead |
| Write-route seams | Function values injected at the composition root (`SetCredentialWriter` → `secret.Save`, `SetAccountWriter`, `SetIngestTrigger`), `503` when unwired | Direct import of `secret`/`config`/`ingest` by `internal/api` — breaks the stated import-direction guarantee that keeps credentials out of the API and web layers |
| Dashboard charts | Hand-rolled inline SVG, no CDN, no build step | Chart.js from a CDN (the tech plan's choice) — adds a third-party network load and an offline failure mode to a loopback tool that holds every prompt |
| Credential storage | `~/.clens/secrets.toml`, outside the DB, readable only by `internal/secret` — POSIX `0600`/`0700` on Unix; on Windows an explicit `icacls` ACL (`/inheritance:r` + `/grant:r`, principal resolved in Go and passed as an `exec` arg — never a shell string) applied to a same-directory temp file whose DACL is read back and verified before an atomic rename over the live file, so it **fails closed** without ever clobbering an existing file's permissions (`x/sys/windows` fallback) | In the DB (a copied backup leaks it) / in the environment only (unusable from a scheduled task) / relying on `0600` on Windows, where Go's `perm` argument is a no-op / a shell-string `"${USERNAME}"` that `os/exec` never expands, so the ACL silently never runs / applying `icacls` on the live file with the destructive `/inheritance:r` before `/grant:r`, which strands an existing `secrets.toml` with a zero-ACE DACL if the grant fails |
| Bedrock/Vertex/Foundry | Out of scope; classified and stored as `auth_kind=cloud`, not priced | Priced — partner rates differ from first-party and are not verifiable from here |
| Branch policy | v1 lands on `main` via `GI-1-…`; `develop` introduced when a second story needs it | Creating an empty `develop` immediately — a branch with no purpose yet |
| Migrations | None in v1 | A framework from day one |
| Multi-machine sync | Out of scope in v1 | — |

---

## Change history

### v1 — initial plan (2026-09-18)

Derived from the Claude Usage Tracker tech plan (v2) and the full `deepseek-lens` feature set, with
grounding verified against the live API reference (model ids, rates, cache economics, minimum
cacheable prefix lengths, Admin API surface) and against real Claude Code JSONL on the authoring
machine (Claude Code 2.1.272) — which is where the duplicate-`usage`-per-`requestId` defect that
drives test 8 was measured, and where the absence of any `costUSD` field confirmed that local
pricing is the only cost source for source B.

### v2 — round-1 review triage (2026-09-18)

Applied every finding and nit from `review/round-1/critique.md`. The changes that make the plan
*less* confident are deliberate, and the corrected claims read as testable rather than asserted:

- **F1 (BLOCKER)** — the subscription-vs-API separation is now enforced by the schema, not by
  discipline: subscription rows write `api_equivalent_cost_usd` and leave `cost_usd` NULL, so
  `SUM(cost_usd)` cannot merge the two models with or without a JOIN; `/api/reconcile` is restated as
  a labelled side-by-side comparison, and the false "structurally impossible" claim is gone. Added a
  billing-mode invariant test (test 12b).
- **F2** — the `request-id`↔`requestId` equivalence is now an assumption with a live verification
  step (recorded in the plan) and a fallback key; test 11 is split so the fixture tests the *merge*
  and a separate step verifies the id equivalence, breaking the circularity.
- **F3** — the four-source argument is restated honestly: one rule (`cache_prefix_below_minimum`)
  needs the body to *fire*; the other two fire from source B and use the body to *localise*; the
  proxy's real case is localisation plus non-Claude-Code client coverage, with dropping it named as a
  legitimate outcome.
- **F4** — price table split: `claude-fable-5-1` at 0.025×, `claude-fable-5` and `claude-mythos-5`
  (added) at 0.1× ($1/MTok), `claude-mythos-5-1` marked provisional for its self-contradicting
  cache-read rate.
- **F5** — admin tables get UNIQUE natural keys + UPSERT, store `window_start`/`window_end`, and an
  explicit admin `ingest_state` semantic; idempotence test added (test 20).
- **F6** — the credential file's control is now the platform that applies it: POSIX modes on Unix, an
  explicit Windows ACL (`icacls`, stdlib) on Windows, with the plan stating that permission bits alone
  are not a control on Windows; `doctor` reports the real protection level; dependency claim held.
- **F7** — a belt-and-braces fallback for an absent TTL split, worded as unobserved (not the refuted
  "older logs lack it" claim), with a fixture.
- **F8** — recursive `**/*.jsonl` discovery, `is_sidechain` populated from the `subagents/` path, test
  added (test 22).
- **F9** — carry-over table records peak pricing (dropped — DeepSeek-specific) and `--model-map`
  (dropped/adapted — the map is observed client-side), and the "nothing is dropped" heading is
  corrected to "no feature is silently dropped".
- **F10** — the whole bead DAG is re-derived; every edge now points to a lower number (pricing 07
  precedes consumer 08; CLI `serve` 17 sits after its dependencies), and the prose spine matches the
  table's new numbering.
- **F11** — "eight routes are added (five GET, three POST)".
- **F12** — the `secret` write seam is named (`SetCredentialWriter` → `secret.Save`, with the sibling
  `SetAccountWriter`/`SetIngestTrigger`), wired at the composition root, `503` when unwired.
- **F13** — `cost_drift` is scoped to API accounts; the subscription-side guard is `model_catalog`
  refresh + `unpriced`/`approximate` labels.
- **F14** — no body hash as a primary key; the response id is preferred and a body-hash key carries a
  per-attempt disambiguator so identical retries stay two rows; test 21 added.
- **F15** — `priority_tier_unsupported` narrowed to the documented Fable 5.1/Mythos 5.1 exclusion;
  the fast-mode Batch/Priority clause is marked `T2-unverified` (not in any readable skill file).
- **N1** — `cache_prefix_below_minimum` settled as the one spelling, with the README↔`kinds.go`
  check noted; **N2** — `project` added to `clens stats --by`; **N3** — invariant 3 restated as
  "one writer package per source, serialized by the store's single write connection", with the new
  merge `UPDATE` path called out.

### v3 — round-2 review triage (2026-09-18)

Applied every round-2 finding (F2.1–F2.6, N2.1) under the conductor's binding overrides. As in v2,
the fixes that make the plan *less* confident are deliberate; each corrected claim now names what a
future implementer can **test**.

- **F2.1 (MAJOR)** — the Windows credential ACL mechanism is now specified and **fails closed**: the
  principal is resolved in Go and passed as an `exec` argument (never the unexpanded shell string
  `"${USERNAME}"`, which `os/exec` does not expand), the file is created by `internal/secret` and the
  ACL step leads with `/reset` before `/grant:r` so pre-existing *explicit* ACEs are stripped (not
  just disinherited by `/inheritance:r`), and a non-zero `icacls` exit causes `secret.Save` to
  **refuse to write the credential** and `doctor` to show a FAIL naming the file unprotected — never
  a silent downgrade to an unprotected file. Security posture, the `doctor` bullet, the Risk table,
  and the Decision-log credential row all updated.
- **F2.2 (MAJOR)** — the per-session cost total is now split like `events`: `sessions` gains
  `total_cost_usd` (API-billed only) and `total_api_equivalent_cost_usd` (subscription only), each
  NULL when that side is empty; `clens sessions` / `GET /api/sessions` render two labelled figures;
  the `GET /api/sessions` contract change is recorded in the API-surface section; test 12b gains the
  mixed-fixture session assertion.
- **F2.3 (MINOR)** — the rounding paragraph and test 7 no longer cite the peak test (its 2× subject
  was dropped with the peak window in F9); the carried-over test's subject is now the live batch
  modifier (×0.5), ported as `TestComputeBatchRoundsPerClass`.
- **F2.4 (MINOR)** — the merge precedence is explicit: the **complete** capture wins the token
  columns (A when `capture_complete`, else B); `source_mismatch` fires **only** when two *complete*
  sources disagree, never when one source has nothing to contribute; B-owned metadata columns are
  preserved from whichever writer supplied them. Test 11(a) gains a truncated-A / complete-B fixture.
- **F2.5 (MINOR)** — test 12b no longer asserts `api_equivalent_cost_usd IS NOT NULL` universally:
  a subscription row is `cost_usd IS NULL` unconditionally, and `api_equivalent_cost_usd IS NOT NULL`
  unless the row is `cost_source = 'unpriced'` (an unpriced subscription call legitimately carries
  NULL on both — the labelled state, not a violation).
- **F2.6 (MINOR)** — the shipped price table is cited per row: `models.md:73/74/75/76`,
  `model-migration.md:1291`. `claude-mythos-5-1` is **promoted from provisional to cited**
  (`models.md:75` — the provisional label was on the wrong row); `claude-opus-4-7`, `claude-opus-4-6`
  and `claude-haiku-4-5` are marked **`provisional`** (zero rate matches in the bundle) with the
  verification step named — check the Pricing URL in `shared/live-sources.md`, per
  `cost-optimization.md:233`.
- **N2.1 (NIT)** — the modifier table no longer asserts "Priority Tier has no per-token premium"; it
  now states only what the bundle documents (Priority unsupported on Fable 5.1 / Mythos 5.1,
  `models.md:73`; Priority costs absent from the cost report, `cost-optimization.md:39`) and applies
  no rate change.
- **F13 (confirm-only)** — no edit. `cost_drift` is scoped to API accounts as a **stated limitation**
  ("for API accounts only", "`cost_drift` can never fire" for subscriptions), not a hedge; confirmed,
  not restated.

### v4 — round-3 review triage (2026-09-18)

Applied all four round-3 findings (F3.1–F3.4) under the conductor's binding overrides. As in v2/v3,
the fixes that make the plan *less* confident are deliberate; each corrected claim now names what a
future implementer can **test**.

- **F3.1 (MINOR)** — the `events.cost_usd` NULL condition is now *both* cases, mirroring the
  subscription carve-out F2.5 applied on the other side: NULL when `billing_mode='subscription'`
  **or** when the row is `cost_source='unpriced'` — an unpriced API row is NOT `$0.00`. Test 12(b) no
  longer asserts `cost_usd IS NOT NULL` for an api row unconditionally: an unpriced API row carries
  NULL on **both** cost columns, the same labelled state, disambiguated by `unpriced_count`. The
  `sessions` paragraph and row gain the matching note that an all-unpriced API session reads NULL on
  `total_cost_usd`.
- **F3.2 (MINOR)** — the Windows ACL step is **restructured, not reordered**. The false "starts with
  no ACEs to inherit" premise is corrected: a newly created file **does** inherit its parent
  directory's ACEs — that is what inheritance means — including at one directory of indirection, so
  `/inheritance:r` is load-bearing and **kept**. The destructive-before-constructive ordering is fixed
  structurally: the complete ACL (`/inheritance:r` + `/grant:r`; no `/reset`, since the rename
  replaces the live file's ACLs wholesale) is applied to a **temp file in the same directory**, the
  DACL read back and asserted to be exactly the intended principal set, and only then renamed into
  place. The plan now states **why** this makes fail-closed *true* rather than merely asserted:
  `icacls` applies its arguments in sequence, so a direct-on-target `/inheritance:r` followed by a
  failing `/grant:r` would leave a zero-ACE DACL denying everyone — stranding an existing
  `secrets.toml`, a state the "does not overwrite an existing credential" promise does not cover
  (its *permissions* would be clobbered). Temp-then-rename leaves the live file byte- and
  ACL-identical on any failure, with the rename as the single atomic commit point. The one safe
  direct-on-target case (the file does not yet exist, nothing to strand) is called out as such.
  Security posture, the Risk-table row, and the Decision-log credential row all updated.
- **F3.3 (MINOR)** — the merge's token rewrite is reconciled with the incrementally maintained session
  totals. The cold-path order gains an explicit **re-derive the owning session's totals** step, run
  **in the same transaction as the merge `UPDATE`**; invariant 3 now says the merge **re-derives** the
  session (the re-derivation wins over the incremental fold), not merely that sessions are maintained;
  the merge bullet notes a merge **rewrites** token/cost columns and that `session_id` is **not**
  rewritten — the row keeps the session it was first written under, so exactly one session is the
  re-derivation target.
- **F3.4 (NIT)** — the surviving "B contributes nothing new" phrase (which contradicted the precedence
  paragraph ten lines below it) is replaced with the correct statement: A contributes the bodies and
  the request-side material; B contributes the JSONL-only metadata and, when its capture is the
  complete one, the token counts.

### v5 — round-4 review triage (2026-09-18)

Applied the one round-4 finding (F4.1) under the conductor's binding overrides. As in v2–v4, the fix
that makes the plan *less* confident is deliberate; the corrected claim now names what a future
implementer can **test**.

- **F4.1 (MINOR)** — the warning ledger now carries the same rewrite-not-add rule F3.3 applied to the
  token/cost ledger. The analyzer seam runs after *every* insert, so a call observed by both sources
  runs it **twice on one `event_id`** — and the plan was silent on what the merge does to the
  warnings, leaving a duplicate-finding/duplicate-`warning_count` path that is user-visible on
  `GET /api/warnings/summary` and `clens sessions`. Fixed by **making the invariant enforced, not
  asserted**: `warnings` gains **`UNIQUE(event_id, kind)`** and the attach becomes an **upsert** keyed
  on it, so the second run updates the row's finding of that kind rather than appending a copy and the
  two sources contribute the union of their complementary findings. **The phrasing was the defect and
  is corrected**: the schema row no longer reads "one analyzer finding per row" (a claim nothing held
  once the merge path could re-run the seam) but "at most one analyzer finding of each kind per event
  row", enforced by the constraint. **`warning_count` is re-derived from `warnings`** — not from
  `events`, and never by increment, since it is not derivable from `events` — **in the same
  transaction as the merge**, alongside the F3.3 cost re-derivation; the cold-path order, invariant 3,
  the `sessions` schema row, and the `warnings` schema paragraph all say so. The plan states
  **explicitly that the seam is idempotent per `(event_id, kind)` by construction** (each rule owns its
  kind; the one deepseek-lens rule that emitted several findings of one kind per request,
  `unsupported_content_block`, is DeepSeek-specific and not carried over) and **keeps the constraint
  anyway**, because the constraint is what makes the claim testable rather than a property to
  re-derive by reading the analyzer code. **Test 11(a) is extended** to assert each finding kind
  appears **once** and the session's `warning_count` equals the distinct-kind count — an assertion a
  test that only checked the row count would miss, and one that would have failed on v4.
