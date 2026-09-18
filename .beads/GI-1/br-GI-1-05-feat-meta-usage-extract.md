# Bead br-GI-1-05: Request metadata + Claude usage extraction

**Plan Reference**: `docs/planning/GI-1-claude-lens-v1.md` — §Invariants 4, §Storage schema, §Cost engine (token classes), §Analyzer rule catalogue, tests 9, 12a

- **Bead ID**: br-GI-1-05
- **Priority**: P0 (critical)
- **Original Estimate**: 3h
- **Dependencies**: br-GI-1-04
- **Blocks**: br-GI-1-07, br-GI-1-08

## Description

The field extraction that turns parsed frames into the two value types the rest of the pipeline
consumes: `parse.Meta` (what the request asked for) and `parse.Usage` (what the response reports).

**`ExtractMeta(reqBody []byte, reqHeaders http.Header) Meta`** — populated from the captured request
body and headers. At minimum:

- `ModelRequested` (the top-level `model`), `SessionHeader` (`x-clens-session`, renamed from
  deepseek-lens's `x-lens-session`), `HasCacheControl` / `CacheControlSites`, the `tools` array and
  top-level `system` shapes br-GI-1-09's tool-invalidation rule compares, the `thinking` config, and
  the `service_tier` / `speed` / `effort` / `inference_geo` request fields.
- `PrefixHash` — the session correlation key computed from the request body (deepseek-lens's
  `parse.PrefixHash`). Its NULL-ness is load-bearing in the session resolver (br-GI-1-08): NULL means
  "keyed by an explicit header", `""` means "the body did not parse".
- `ClientVersion`, `Project`, `GitBranch`, `IsSidechain`, `CliEntrypoint` — the fields **only source
  B supplies**. `ExtractMeta` leaves them zero for a proxy capture; br-GI-1-11 fills them from the
  JSONL line. They exist here so the row type is complete and the merge can preserve whichever writer
  supplied them.

**`ExtractUsage(respBody []byte, contentType string) Usage`** — the Claude usage object. It must
model all six priced classes plus the reported extras:

| `Usage` field | Source field | Note |
|---|---|---|
| `InputTokens` | `usage.input_tokens` | **uncached remainder only** — invariant 4 |
| `OutputTokens` | `usage.output_tokens` | |
| `CacheWrite5mTokens` | `usage.cache_creation.ephemeral_5m_input_tokens` | |
| `CacheWrite1hTokens` | `usage.cache_creation.ephemeral_1h_input_tokens` | |
| `CacheReadTokens` | `usage.cache_read_input_tokens` | |
| `ThinkingTokens` | `usage.output_tokens_details.thinking_tokens` | a **subset of output** — never added again |
| `Model` | `message_start` model | the resolved model, used to confirm the requested→resolved mapping |
| `StopReason` | `stop_reason` | `max_tokens` / `refusal` / `end_turn` / … |
| `StopCategory` | `stop_details.category` | read **only** when `stop_reason == "refusal"`, as the API requires |
| `ServiceTier` | `service_tier` | `batch` / `priority` / … |
| `Speed` | `speed` | `fast` on Opus 5/4.8 |

**Flat-cache-creation fallback (belt-and-braces, plan §Cost engine).** When the `ephemeral_*` TTL
split is absent but the flat `usage.cache_creation_input_tokens` is present, record the whole flat
count into `CacheWrite5mTokens` (so invariant 4's derived total still counts it) and set a
`TTLUnknown` flag on `Usage`. The shape has not been observed — every JSONL file on the authoring
machine carrying the flat field also carries the split (1012 of 1012) — but source B exists to
backfill pre-install history and an unseen shape is not an impossible one. The true rate could be
1.25× or 2×, so br-GI-1-07 marks the row `cost_source = approximate` with a `cache_ttl_unknown` note
rather than picking one silently.

**Invariant 4 lives in this bead's type contract**: `InputTokens` is **the uncached remainder**, and
the prompt total is
`InputTokens + CacheWrite5mTokens + CacheWrite1hTokens + CacheReadTokens`. No helper in this package
may expose `InputTokens` under a name that reads as "prompt size"; the derived total is maintained in
`store` (br-GI-1-06) so no caller can get it wrong.

`ThinkingTokens` is a subset of `OutputTokens` and is **never** re-added to it.

## Rationale

This is where the most common misreading of Claude's usage object is prevented or baked in:
`input_tokens` looks like "the prompt", and it is only the uncached remainder. Isolating extraction
from framing means the invariance is testable from fixtures alone, with no proxy or store in the
loop.

## Outcome Definition

- `go test ./internal/parse/... -race` passes.
- Six-class extraction from a real SSE capture yields the documented fields; a non-stream JSON body
  yields the same.
- A body carrying the `ephemeral_*` split and a body carrying only the flat
  `cache_creation_input_tokens` parse to the **same** prompt total, with the flat shape flagged
  `TTLUnknown` (test 9's tolerance half).
- `StopCategory` is populated only when `StopReason == "refusal"`.
- `ExtractMeta` on a proxy request leaves `ClientVersion`/`Project`/`GitBranch`/`IsSidechain`/
  `CliEntrypoint` empty.

## Test Specifications

- Unit Tests (`internal/parse/usage_test.go`):
  - Full usage object → all six classes populated, `Thinking` subset of output.
  - **Flat-only cache-creation** fixture → `CacheWrite5mTokens` holds the flat count, `TTLUnknown` set.
  - **Split** fixture → `CacheWrite5m`/`CacheWrite1h` populated, `TTLUnknown` clear.
  - The two fixtures yield the **same** `total_prompt_tokens` (test 9).
  - `stop_reason: max_tokens` → `StopReason` set, no category.
  - `stop_reason: refusal` with `stop_details.category` → category populated.
  - `stop_reason` other than `refusal` **ignores** `stop_details.category`.
  - A non-stream `application/json` body → same fields as the SSE form.
- Unit Tests (`internal/parse/meta_test.go`):
  - `model`, `service_tier`, `speed`, `effort`, `inference_geo` extracted.
  - `x-clens-session` header → `SessionHeader`; absent → empty.
  - `cache_control` sites and the `tools`/`system` shapes captured.
  - `PrefixHash` is stable for the same body and differs for a changed body.
  - JSONL-only fields are empty on a proxy request.
- Integration Tests: none (br-GI-1-08 wires it).
- E2E: none.

## Files to Touch

- `internal/parse/meta.go`, `internal/parse/meta_test.go` (create)
- `internal/parse/usage.go`, `internal/parse/usage_test.go` (create)
- `internal/parse/types.go` (modify — `Meta`, `Usage` struct definitions)
- `internal/parse/testdata/` (create — split and flat cache-creation fixtures)
