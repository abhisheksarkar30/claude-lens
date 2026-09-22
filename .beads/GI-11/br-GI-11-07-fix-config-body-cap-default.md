# Bead br-GI-11-07: raise the body cap default to 2 MB and sweep the stale "256 KB" claims

**Plan Reference**: `docs/planning/GI-11-cost-and-capture-fidelity.md` — §2 RC-C, §4 Code (the
`internal/config/config.go` and `internal/config/config_test.go` rows; the `stale code comments` row of
the Docs table), §5 Integration (the proxy cap case and the TTFB gate), §6 (the cap-raise risk rows),
§7

- **Bead ID**: br-GI-11-07
- **Priority**: P1 (high)
- **Original Estimate**: 2h
- **Dependencies**: None
- **Blocks**: br-GI-11-10 and br-GI-11-11 (the docs state the new default), the rollout (the store moves
  to `D:`)

> **RC-C and RC-B ship together, and RC-C is what actually removes the truncation.** RC-B without RC-C
> turns a silent problem (a laundered flag) into a loud one (an honest `incomplete` on the majority of
> calls): 58% of request bodies exceed 256 KB on this install. Raising the cap is what makes the honest
> flag rare again on newly captured traffic.

> **The `capture_complete` flag keeps its meaning; the *marker* does not.** The length-vs-cap truncation
> *marker* compares a stored body against the **current** cap, so raising the default silently
> reclassifies every historical at-cap row (262,144 bytes) as `Complete`. That is a **known, accepted
> limitation** (F1.8) — a per-row recorded cap would be required to mark old rows and the plan
> deliberately does not add one. The authoritative signal is the `capture_complete` flag, not the
> marker. This is br-GI-11-10's `storage-schema.md` / `data-privacy-and-compliance.md` note to record;
> **no code change** here.

## Description

### The change

- `internal/config/config.go`: `BodyCapBytes` default **262144 → 2097152** (`:59`). `BodyCapBytes`
  bounds **both** bodies (`proxy.go:88` for the response, `:126` for the request) and **stays
  configurable** so it can be lowered without a rebuild.
- Add the **missing** doc comment on the `BodyCapBytes` field (`:36` — the field is currently
  undocumented) and update the flag help (`:222`). The earlier "doc comment at `:319`" reference is
  **dropped**: `:318-326` is the `BodyPolicy: "truncated"` rejection paragraph, which names
  `BodyCapBytes` only as the bound the two policies shared — nothing there documents the field. While
  there, correct that paragraph's "both bounded by `BodyCapBytes`" phrasing **only if** it reads as
  still true of the new default; it is a historical note about the removed `truncated` policy.
- `internal/consumer/consumer.go:25`: `defaultBodyCapBytes = 262144` — the consumer's own independent
  fallback, which silently disagrees with the new default whenever the `SetBodyPolicy`/cap seam is left
  unwired. It must move to the new default with `config.go`'s, so the two cannot disagree.
- **The stale "256 KB" prose comments** (the plan's `stale code comments` sweep):
  - `internal/analyze/rules.go:72` — "the 256 KB cap is …"
  - `internal/decode/decode.go:90` — "256KB of brotli can expand to …"
  - `internal/cli/export.go:133` — "reading the blobs here would be 256 KB per row", stale the moment
    the default is 2 MB.
  - `internal/config/config.go`'s own comment prose, if it states a figure.

### What this bead does not do

- **No change to `internal/cli/purge.go:19-25` and `internal/cli/rekey.go:17`.** Both are about the
  *destructive* count and are **still true after GI-11** (two destructive commands). The sweep must
  **read** them and leave them correct, not rewrite them — the same "do not edit to four" instruction
  §4 carries for the docs.
- **No per-row cap column, no marker fix.** The historical at-cap marker reclassification is accepted
  and documented (br-GI-11-10), not fixed.
- **No `index.html`/`app.js`/`decode.go`/`show.go` marker logic change.** Those readers compare a stored
  body against the current cap; the plan accepts that.
- **No docs edits.** The `docs/context/*`, `README.md` and `CLAUDE.md` surfaces are br-GI-11-10/11's.
- **No change to the proxy's hot path logic.** The cap is a buffer size; nothing else moves.

## Rationale

Measured against the client's `Content-Length` across 4,873 proxy rows: **2,845 (58%)** of request
bodies are over 256 KB, 106 are over 1 MB, **0** are over 2 MB, and the largest request body seen is
**1,246,222 bytes**. Truncation is the norm, not the exception, and the dominant body is the
**request** (a Claude Code request carries the system prompt, the full tool schemas and the conversation
history), not the response the report named — 58% of calls against 12% of responses. A 2 MB cap
(`2,097,152`) captures every body this install has ever seen, with headroom. Projected growth is
**~0.35 GB**, which is why the store's move to `D:` (a rollout action, no bead) belongs in this story.

## Outcome Definition

- `config.Default().BodyCapBytes == 2097152`; `internal/consumer`'s `defaultBodyCapBytes` agrees with it.
- An explicit `CLENS_BODY_CAP_BYTES` / `--body-cap-bytes` still overrides the default.
- `internal/config/config.go`'s `BodyCapBytes` field carries a doc comment; the flag help states the new
  default.
- No stale "256 KB" / `262144` prose remains in `internal/analyze/rules.go`, `internal/decode/decode.go`,
  `internal/cli/export.go` (grep the tree for `256 KB`, `256KB` and `262144` and confirm every remaining
  hit is an intentional, cap-relative test fixture such as `proxy_test.go:82`, or a comment the sweep
  deliberately left because it stays true).
- **§5 integration, at the real default:** a 1.2 MB request body is captured **whole** and the row is
  flagged `capture_complete=1` — the end-to-end form of RC-C.
- The `bodyCap = 64` proxy cap test (`proxy_test.go:462`) stays valid unchanged — it is cap-relative,
  not cap-specific.
- **The TTFB gate stays green.** The cap change is a buffer size and a larger buffer is marginally more
  work on the hot path, so this is exactly the invariant to re-assert rather than assume.
- `go build ./...`, `go vet ./...`, `go test ./...` pass.
- **Forward half of acceptance #3 (manual, on a newly captured day, after the cap change):** the
  honest-truncation count on a day captured under the 2 MB cap is far lower than on the copied
  historical store — a past day cannot be re-captured, so this is checked on new traffic, not against
  the frozen copy (§5, §6).

## Test Specifications

**Name tests by their Go function name, never by a plan line range.**

- **Unit Tests:**
  - `internal/config/config_test.go` — a case asserting `Default()`'s `BodyCapBytes` is **2097152**
    (the new default is pinned, so a later silent revert goes red).
  - `internal/config/config_test.go` — a case asserting an explicit `CLENS_BODY_CAP_BYTES` env value
    **and** an explicit `--body-cap-bytes` flag each still override the default (the plan's
    "override still wins" requirement).
- **Integration Tests:**
  - `internal/proxy/proxy_test.go` — a case at the **real default** asserting a ~1.2 MB request body is
    captured whole (body length equals what was sent) and the resulting row reports
    `CaptureComplete == true`.
  - The existing `bodyCap = 64` cap-relative test stays as-is and must stay green (it is the control
    that the "cap-relative" property did not get coupled to the default).
  - The existing TTFB test stays green unchanged (re-run, not rewritten).

## Files to Touch

- `internal/config/config.go` (modify — `BodyCapBytes` default `:59` 262144 → 2097152; add the field doc
  comment `:36`; update the flag help `:222`; adjust the `BodyPolicy: "truncated"` paragraph `:318-326`
  only if its "both bounded by `BodyCapBytes`" phrasing no longer reads true)
- `internal/config/config_test.go` (modify — pin the new default; assert env and flag still override)
- `internal/consumer/consumer.go` (modify — `defaultBodyCapBytes` `:25` to the new default so the
  consumer's fallback cannot disagree with `config.go`)
- `internal/analyze/rules.go` (modify — the `:72` "256 KB" prose)
- `internal/decode/decode.go` (modify — the `:90` "256KB" prose)
- `internal/cli/export.go` (modify — the `:133` "256 KB per row" comment)
- `internal/proxy/proxy_test.go` (modify — add the real-default 1.2 MB capture case)
