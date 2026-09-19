[← INDEX](INDEX.md)

# Glossary

The codebase's own vocabulary. Where a term has a canonical list in code, that file is the source
of truth and this table links to it rather than copying it.

| Term | Meaning | First seen in |
|---|---|---|
| **source** | One of the four independent origins of traffic or usage data. Values: `proxy`, `jsonl`, `snapshot`, `admin`. `events.source` records which one wrote a row. | [README.md](../../README.md) §Why four sources |
| **proxy** (source A) | This tool's own capture of `/v1/messages`. Adds request/response bodies, exact parameters, and what was dropped. | `internal/consumer` |
| **jsonl** (source B) | Claude Code's own transcripts under `~/.claude/projects/**/*.jsonl`. Adds calls that never went through the proxy, plus session identity. | `internal/jsonlogs` |
| **snapshot** (source C) | The claude.ai usage endpoint. Adds the plan's own utilization percentage. | `internal/snapshot` |
| **admin** (source D) | The Admin API usage and cost reports. Adds the real billed dollars, per day and model. | `internal/adminrep` |
| **hot path** | The client's goroutine inside `internal/proxy`: tee bytes into the sink and return. Never parses, decompresses, or touches the database. | [architecture.md](architecture.md) |
| **cold path** | The consumer goroutine: drain the sink, decompress, parse, analyze, price, write. Everything expensive happens here. | `internal/consumer` |
| **sink** | The bounded buffer connecting hot and cold paths — the only thing the two are allowed to share. | `internal/sink` |
| **TTFB gate** | `TestNoBufferingSSE`: a fake upstream streaming SSE slowly, asserting the client sees its first event *before* upstream sends its last. The hard check that keeps the hot path unbuffered. | [internal/proxy/proxy_test.go:40](../../internal/proxy/proxy_test.go#L40) |
| **event** | One captured call — the unit of the `events` table. | [storage-schema.md](storage-schema.md) |
| **session** | One agentic run: a fold of the events that belong to it. Materialized in the `sessions` table. | `internal/session` |
| **`request_id`** | The cross-source identity key. `UNIQUE` on `events`; a second source arriving with disagreeing counts for the same id is a `source_mismatch` warning, not a second row. | [internal/store/merge.go](../../internal/store/merge.go) |
| **merge** | Folding a `jsonl` row onto the `proxy` row that describes the same call. The only `UPDATE` on `events`. | [internal/store/merge.go](../../internal/store/merge.go) |
| **`first_source`** | The source that created a row. **Never rewritten by a merge** — which is why the surviving row's session, not the incoming event's, is the re-derivation target. | [internal/store/merge.go:181](../../internal/store/merge.go#L181) |
| **warning kind** | A finding's canonical spelling, declared once in `kinds.go` and pinned against the README by a test. | [internal/analyze/kinds.go](../../internal/analyze/kinds.go) |
| **`nonAnalyzeKinds`** | The five kinds emitted by a package *other* than `analyze` (`analyzer_panic`, `source_mismatch`, `cost_drift`, `quota_window_approaching`, `peak_pricing`). Exists so a README check expects a kind with no rule in that package. | [internal/analyze/kinds.go](../../internal/analyze/kinds.go) |
| **`billing_mode`** | `subscription` or `api`. Comes from the **configured account**, not from the credential shape alone — and on a merge, from the capture that supplied the cost columns. | [cost-and-quota.md](cost-and-quota.md) |
| **peak window** | A model whose rate varies by time of day: peak = off-peak × `Multiplier` (DeepSeek: 2×), inside UTC hour spans on Mon–Fri, excluding the off-peak dates. `Rate.Peak == nil` means flat-priced, which is every Anthropic row. | [cost-and-quota.md](cost-and-quota.md) §Peak and off-peak |
| **off-peak dates** | The 33 bundled **2026** Chinese public holidays on which DeepSeek does not charge peak. Configurable, and expiring — past 2026 every weekday hour prices at peak. | [internal/pricing/table.go](../../internal/pricing/table.go) |
| **third-party model prefix** | A model id prefix (`deepseek-` by default) whose rows bill **pay-as-you-go**: routed to the `api` account and `api` billing mode per row rather than taking the collector's subscription default. Config, not a code rule. | [internal/jsonlogs/jsonlogs.go](../../internal/jsonlogs/jsonlogs.go) |
| **API-equivalent cost** | What a subscription's tokens *would* have cost at API rates. Hypothetical, and stored in its own column so it can never be summed with a real one. | `api_equivalent_cost_usd` |
| **`cost_source`** | How a cost figure was arrived at. Vocabulary: `shipped` \| `provisional` \| `user` \| `approximate:<reason>` \| `unpriced`. | [cost-and-quota.md](cost-and-quota.md) |
| **`unpriced`** | No rate row exists for the model. The cost columns stay NULL — never `$0.00`, which reads as "this was free". | [cost-and-quota.md](cost-and-quota.md) |
| **`unconfigured`** | No limit row exists for the plan. The tool shows the snapshot % and the user's own burn, and says so — it never guesses what "Max 20x" means. | [cost-and-quota.md](cost-and-quota.md) |
| **calibration** | Learning a plan's limit empirically: a snapshot window that reached 100% makes the burn recorded at that instant an observation of the limit, offered as a *candidate* for confirmation, never applied silently. | [internal/quota/calibration.go](../../internal/quota/calibration.go) |
| **burn** | Tokens consumed inside a rolling window, summed over **every** token column. Using `input_tokens` alone would under-count every cached call. | [cost-and-quota.md](cost-and-quota.md) |
| **`auth_kind`** | The credential *shape* a call presented: `oauth`, `api_key`, `admin`, `cloud`, `unknown`. The classification is stored; the credential never is. | [cost-and-quota.md](cost-and-quota.md) |
| **`auth_kind_anomaly`** | The warning raised when `auth_kind` disagrees with the account's configured `billing_mode`, in either direction. | [internal/analyze/rules.go](../../internal/analyze/rules.go) |
| **cache breakpoint** | A `cache_control` marker in the request body. Below the model's minimum cacheable prefix it caches nothing — a per-model, deliberately non-monotonic threshold. | [internal/analyze/rules.go](../../internal/analyze/rules.go) |
| **minimum cacheable prefix** | The shortest marked prefix a given model will actually cache: 512 / 1024 / 2048 / 4096 tokens depending on the model generation. | `minimumCacheablePrefix` in [internal/analyze/rules.go](../../internal/analyze/rules.go) |
| **`input_tokens`** | The **uncached remainder only**. Total prompt size is `input_tokens + cache_write_5m + cache_write_1h + cache_read`. A row reporting `input_tokens` alone as the prompt size is wrong. | [storage-schema.md](storage-schema.md) |
| **seam** | A settable function value on the API type through which the dashboard reaches a package it may not import. An unwired seam answers `503`. | [architecture.md](architecture.md) |
| **replay** | Re-issuing a captured call. Sends through the live proxy handler, so it inherits the same transport, tee, body cap and redaction. Opt-in; off unless `--replay`. | [internal/replay](../../internal/replay/) |
| **`prefix_hash`** | A hash of the cacheable prefix, used to line up a cache write with the later read that should have hit it. | `internal/analyze` |
| **sidechain** | A sub-agent call that is part of a session but not the main thread. `is_sidechain`. | [internal/store/schema.sql](../../internal/store/schema.sql) |
| **`capture_complete`** | Whether the captured body is the whole thing (policy and cap permitting) or was truncated. | [internal/store/schema.sql](../../internal/store/schema.sql) |
| **GI-1 / bead** | The work-item vocabulary: `GI#<n>` is a GitHub issue; `br-GI-1-NN` is one bead under it. Enforced by the commit-msg hook. | [CLAUDE.md](../../CLAUDE.md) §Conventions |
