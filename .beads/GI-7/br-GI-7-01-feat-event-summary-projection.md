# Bead br-GI-7-01: `EventSummary` — the list path stops reading bodies, and the four full-row callers say so out loud

**Plan Reference**: `docs/planning/GI-7-header-and-body-visibility.md` — §3 D1, §4 (`types.go`, `store.go`, `api.go`, `api/replay.go`, the `cli/*` rows, `quota.go`), §5 T1/T2/T3, §6 (the drift row and the "silently breaks a caller" row), §9 bead 01

- **Bead ID**: br-GI-7-01
- **Priority**: P0 (critical)
- **Original Estimate**: 4h
- **Dependencies**: None
- **Blocks**: br-GI-7-03, br-GI-7-05, br-GI-7-06, br-GI-7-07

## Description

`GET /api/requests?limit=50` returns **12,149,782 bytes** today because
`ListEvents` selects all 45 `events` columns and the list handler encodes the rows straight to the
wire (`api.go:350`, `:363`). The Calls tab calls that on every tab switch and renders none of the
body columns. This bead introduces **`EventSummary`** — the scalar block and no header/body column —
makes `ListEvents` return it, and gives the callers that genuinely need a body an explicitly named
full-row method. After this bead the list path never *reads* the blobs out of SQLite, not merely
never ships them.

The plan's §9 collapses this into one bead; that is not a style choice and this bead is deliberately
large. See "Why this cannot be split" below.

### `internal/store/types.go` — the type split (D1)

- **New `EventSummary`**: every scalar field `Event` carries today — `ID`, `RequestID`, `Source`,
  `SourceRefs`, `FirstSource`, `StartedAt`, `EndedAt`, `AuthKind`, `Account`, `BillingMode`,
  `ModelRequested`, `ModelResolved`, the seven token counts, `ServiceTier`, `Speed`, `Effort`,
  `InferenceGeo`, `StopReason`, `StopCategory`, `IsSidechain`, `SessionID`, `Project`, `GitBranch`,
  `ClientVersion`, `CliEntrypoint`, `CostUSD`, `ApiEquivalentCostUSD`, `CostSource`, `PrefixHash`,
  `ReplayOf`, `ReplayEdits`, `CaptureComplete`, `Method`, `Path`, `Status`. No JSON tags (the existing
  wire contract is the Go field names — see `app.js:7-13`).
- **`Event` embeds `EventSummary`** and keeps exactly the four non-scalar columns directly:
  `ReqHeaders`, `RespHeaders`, `ReqBody`, `RespBody`. `Method`/`Path`/`Status` are scalars and move
  into the summary — `statusCell` (`ls.go:105`, which reads `Source` and `Status`) must keep compiling
  against the summary.

  Embedding, not duplication: `store.Event`'s wire keys are its Go field names, so a second,
  hand-kept copy of the scalar block could drift from `Event` field-by-field and silently split the
  list wire from the detail wire. `encoding/json` flattens an embedded struct, so the detail route's
  wire keys are unchanged. `Event` gains **no** transcript fields here — those are br-GI-7-06.

### `internal/store/store.go` — the summary SELECT, and the three read methods

- `summaryOmittedColumns` — a **named** list of the four header/body column names
  (`req_headers`, `resp_headers`, `req_body`, `resp_body`). br-GI-7-06 appends two more to this one
  list; nothing else in the projection names them.
- `summarySelectColumns` — `eventSelectColumns` minus `summaryOmittedColumns`, in the same order.
- `scanEventSummary(row rowScanner) (*EventSummary, error)` — the positional scan over the summary
  columns, sharing the existing `rowScanner` interface.
- `ListEvents` retypes to `([]*EventSummary, error)` and queries `summarySelectColumns`.
- **New `ListEventsFull(ctx, filter) ([]*Event, error)`** — today's `ListEvents` body verbatim
  (`eventSelectColumns` + `scanEvent`).
- **New `SessionEventsSummary(ctx, sessionID) ([]*EventSummary, error)`** — the same `WHERE
  session_id = ? ORDER BY started_at ASC` as `SessionEvents` (`store.go:205`), on
  `summarySelectColumns`/`scanEventSummary`.
- **`SessionEvents` itself is unchanged** and still returns `[]*Event` at full width. Its rows feed
  `analyze.AnalyzeSession` via `consumer.Run` (`consumer.go:244`), and
  `ruleCacheInvalidatedByTools` reads `prev.ReqBody`/`cur.ReqBody` (`rules.go:306-327`, `:310`,
  `:320`) — the exact field the summary exists to remove. Three `Store` interfaces declare
  `SessionEvents` (`api.go:58`, `consumer.go:39`, `jsonlogs.go:35`); retyping it would stop
  `*store.Store` satisfying the latter two and fail the build at the composition root. Do not narrow
  it.

### The four callers that need a full row — each switches to `ListEventsFull` by name (D1)

1. **`internal/cli/serve.go:261`** — `checkRedaction`, the boot-time credential-leak self-test. It
   reads `ev.ReqHeaders` (`:266-270`) and hands it to `proxy.RedactCheck`. Left on the summary it
   would `continue` on every row and report zero findings, always, with no error and no log — a
   security control disabled by default.
2. **`internal/cli/export.go:90`** — `clens export`'s JSON read, documented as the complete dump
   *including the captured bodies* (`export.go:18-21`), so it moves to `ListEventsFull`.
   **Corrected during implementation:** the CSV read at `:131` does **not** move. This bead
   originally sent both; `exportColumns` (`:109-116`) carries no header/body column and
   `rowValues` reads only scalars, so the full-width read there would pull 256 KB per row to emit
   none of it. CSV stays on `ListEvents` and `rowValues` retypes to `*store.EventSummary`.
3. **`internal/cli/ls.go:55-62`** — the `--json` branch encodes each whole row (`enc.Encode(ev)`), so
   on the summary type the four header/body keys vanish. That is exactly the silent contract change
   this story exists to fix. The **table** path (`:66-97`, scalars only) stays on the summary.
4. **`internal/api/replay.go:224` (`newestReplay`) and `:249` (`awaitReplayRow`)** —
   `awaitReplayRow`'s return value is not reduced to `.ID`: `api/replay.go:142` hands the row to
   `replay.OutcomeOf(*store.Event, …)` (`internal/replay/replay.go:36`). Both keep their `*store.Event`
   signatures, so `internal/replay/replay.go` and `internal/cli/replay.go` (`:245`, the second
   `OutcomeOf` caller, fed by `getEvent` → `st.GetEvent`) are **unchanged**. The poll reads one row per
   `replayPollInterval`, not 50 rows per tab switch.

### The callers that take the summary and do not notice

No code change beyond the type flowing through, because each reads only scalars: `internal/cli/ls.go`
table path, `internal/cli/tail.go:49,72`, `internal/cli/stats.go:162` (`writeGrouped`),
`internal/cli/purge.go:138`, `internal/quota/quota.go:65`, the API list route, and the session route
on `SessionEventsSummary`.

### `statusCell` and `displayModel` retype once, to `*store.EventSummary`

`statusCell` (`ls.go:105`) and `displayModel` (`format.go:230`) take `*store.EventSummary`, so one
definition serves both halves. Summary callers pass the row (`ls.go:79-80`, `tail.go:97-98`); the
three full-path callers pass `&ev.EventSummary` — `show.go:61-62`, `export.go:182`
(`rowValues`), and `internal/cli/replay.go:162`.

### The `Event` embed reshapes every composite literal that sets a promoted scalar

Go forbids promoted fields as composite-literal keys, so `&store.Event{RequestID: …}` becomes
`&store.Event{EventSummary: store.EventSummary{RequestID: …}}`. This is mechanical and
**compile-checked** — a forgotten one is a build failure, not a silent behaviour change. A literal
that sets only one of the four direct columns (`&store.Event{ReqBody: …}`) is unchanged.

Production sites (three): `consumer.go:398` (`buildEvent`), `jsonlogs.go:381`, `stats.go:80`.

Test fixtures and the one in-package literal: `internal/store/store_test.go` (`&Event{` at `:43`),
`internal/analyze/rules_test.go` (including its **elided** `[]*store.Event{…}` elements),
`internal/api/api_test.go`, `internal/api/broker_test.go`, `internal/cli/cli_test.go`,
`internal/cli/additions_test.go`, `internal/cli/replay_test.go`, `internal/session/session_test.go`,
`internal/quota/quota_test.go`, `internal/reconcile/reconcile_test.go`,
`internal/replay/replay_test.go`.

**Not** in the list: `internal/analyze/analyze_test.go` (every literal is `&store.Event{}` or
`&store.Event{ReqBody: …}` — no promoted field is set) and `internal/jsonlogs/jsonlogs_test.go`'s own
literal (a bare `map[string]*store.Event{}`). Neither *literal* moves — but see the next section,
where `jsonlogs_test`'s **helper** does.

### The `ListEvents`-derived test helpers move even where no literal does

Three helpers declare their type *from* `ListEvents` and therefore stop compiling:

- `internal/consumer/consumer_test.go` — `waitForEvents` (`:536`), returns `[]*store.Event` built from
  `st.ListEvents` (`:540`). Becomes `[]*store.EventSummary`.
- `internal/jsonlogs/jsonlogs_test.go` — `eventsByRequestID` (`:37`), a `map[string]*store.Event` from
  `st.ListEvents` (`:39`). Its **literal** is unchanged; its **return type** is not.
- `internal/api/replay_test.go` — `capture` (`:80`) and `recordedReplay` (`:129`), declared
  `*store.Event` and fed by `f.st.ListEvents` (`:97`, `:131`). Both read the row as **bodies**
  (`row.ReqBody` at `:179`, `:198`), so both move to `ListEventsFull` and keep `*store.Event`.

### `internal/api.Store` and `internal/quota.Store`

- `internal/api/api.go:49` — `ListEvents` retypes to `[]*store.EventSummary`; add `ListEventsFull`;
  drop `SessionEvents` (nothing else in `internal/api` uses it once `getSession` moves) and add
  `SessionEventsSummary`. Both replay-poll callers go through the interface (`api/replay.go:224`,
  `:249`), so the full method must be declared on it too.
- `internal/api/api.go:539` — `sessionDetail.Calls` becomes `[]*store.EventSummary`; `:557`
  (`getSession`) calls `SessionEventsSummary`. Its only use of a row is `c.ID` (`:557`, `:565`), so
  nothing else in the handler changes.
- `internal/quota/quota.go:19` — the package's own `Store` interface declares
  `ListEvents(...) ([]*store.Event, error)`, so `*store.Store` stops satisfying it unless it retypes
  with the store. Retype it; `:65` is unchanged.

### Why this cannot be split

The `Event` embed and the `ListEvents` retype are one compile-breaking unit, and the split must not
leave a non-compiling tree between two beads:

- The moment `Event` embeds `EventSummary`, every literal that sets a promoted scalar fails to build —
  including three in production.
- The moment `ListEvents` returns `[]*EventSummary`, `api.Store` and `quota.Store` stop being
  satisfied by `*store.Store`, and `checkRedaction`/`export`/`ls --json`/`awaitReplayRow` stop
  compiling because they name a body field. All four must move in the same commit.

A "land `EventSummary` first, retype later" split would produce a commit whose deliverable is a type
nothing uses — dead code a reviewer should reject. So the store change, the two interfaces, the four
full-row callers and the fixture wraps land together.

## Rationale

The list route is the dashboard's hottest fetch and ships 12.1 MB the view discards. Zeroing the
fields in the handler would fix the wire size and leave the SQLite read — on a local tool the blob
read is the real cost. The projection is therefore a prerequisite for br-GI-7-04's body view, not a
nicety beside it.

A **distinct type** rather than a `store.EventFilter` body flag is deliberate: with a flag, a caller
that forgets it silently reads bodies it did not ask for, and — defaulted off — silently gets `nil`
where it needed bytes. `store.Event` has no JSON tags, so a `nil []byte` marshals as `"ReqBody":null`
and a body-less `jsonl` row is byte-identical on the wire to an unselected proxy row. With a distinct
return type a caller that needs a body cannot even name the field: reaching one is a **compile
error**, not a silent default.

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass.
- `GET /api/requests?limit=50` encodes to **under 64 KB** and contains no `ReqBody`, `RespBody`,
  `ReqHeaders` or `RespHeaders` key, against a fixture of 50 rows each carrying a 1 MB body (T1).
- `GET /api/sessions/{id}` does the same, and `len(calls) == 50` (T2).
- The summary `SELECT` column list equals `eventSelectColumns` minus `summaryOmittedColumns` (T1's
  companion, in `internal/store`).
- The boot-time redaction self-test still reports a seeded un-redacted header, and `clens ls --json`
  still emits all four header/body keys with their stored values (T3).
- `SessionEvents` still returns `[]*store.Event`; `internal/analyze` and `internal/replay` are
  unchanged; `TestNoBufferingSSE` stays green.

## Test Specifications

- Unit Tests (`internal/store/store_test.go`):
  - `TestSummaryColumnsAreTheFullSetMinusBodies` — both column-list constants are `package store`
    internals (`store.go:983-994`), so this is the only package it can live in. Assert
    `summarySelectColumns` equals `eventSelectColumns` **minus `summaryOmittedColumns`**, comparing
    the parsed lists, not the raw strings. The assertion tracks the list, so br-GI-7-06 extends
    `summaryOmittedColumns` and this test demands six columns absent without being edited.
- Unit Tests (`internal/api/api_test.go`):
  - `TestListRouteOmitsBodies` (T1): seed 50 rows each carrying a 1 MB body (50 MB stored), fetch
    `GET /api/requests?limit=50`, and assert the encoded response is under 64 KB **and** contains none
    of the four keys. The size bound is the regression guard for a future "just add it back"; the
    fixture is sized so a leaked body would blow 50 MB past it, which is what keeps the bound
    non-vacuous.
  - `TestSessionRouteOmitsBodies` (T2): the same 50 MB fixture, `GET /api/sessions/{id}`, and assert
    **both** halves — no body key in `calls[]`, **and** `len(calls) == 50` with the encoded response
    under 64 KB. The length half is load-bearing: with `Calls` typed as `EventSummary` the "no body
    key" half is guaranteed by the type checker and catches nothing new, so the size bound is the only
    live guard — and a non-empty `calls[]` is what stops an empty array satisfying it vacuously.
- Unit Tests (`internal/cli` — T3):
  - `TestCheckRedactionStillSeesHeaders`: seed one row whose `req_headers` carries an un-redacted
    credential, run `checkRedaction`, assert it reports the finding. On the summary path it would
    `continue` on every row and report zero findings, silently, always.
  - `TestLsJSONKeepsBodyFields`: seed a proxy row with non-empty `req_headers`/`resp_headers`/
    `req_body`/`resp_body`, run `runLs(["--json"], w)`, decode the emitted JSON, and assert all four
    keys are present and equal to the stored values.
- Integration Tests: none — no new route, no schema change.
- E2E: none (no JS harness exists).

## Files to Touch

- `internal/store/types.go` (modify — `EventSummary`; `Event` embeds it and keeps the four header/body
  fields)
- `internal/store/store.go` (modify — `summaryOmittedColumns`, `summarySelectColumns`,
  `scanEventSummary`, `ListEvents` retyped, `ListEventsFull`, `SessionEventsSummary`)
- `internal/store/store_test.go` (modify — `TestSummaryColumnsAreTheFullSetMinusBodies`; the `&Event{`
  literal at `:43`)
- `internal/api/api.go` (modify — `Store` interface; `sessionDetail.Calls`; `getSession`)
- `internal/api/replay.go` (modify — `newestReplay`, `awaitReplayRow` on `ListEventsFull`)
- `internal/api/api_test.go` (modify — T1/T2; fixture literals)
- `internal/api/replay_test.go` (modify — `capture`/`recordedReplay` on `ListEventsFull`; literals)
- `internal/api/broker_test.go` (modify — fixture literals)
- `internal/cli/serve.go` (modify — `checkRedaction` on `ListEventsFull`)
- `internal/cli/export.go` (modify — both reads on `ListEventsFull`)
- `internal/cli/ls.go` (modify — `--json` on `ListEventsFull`; `statusCell` retyped)
- `internal/cli/tail.go` (modify — `printNew` retyped to `[]*store.EventSummary`)
- `internal/cli/show.go` (modify — `displayModel`/`statusCell` pass `&ev.EventSummary`)
- `internal/cli/stats.go` (modify — `&store.Event{…}` wrap at `:80`)
- `internal/cli/purge.go` (modify — the summary flows through; type only)
- `internal/cli/replay.go` (modify — `displayModel(&orig.EventSummary)` at `:162`)
- `internal/cli/format.go` (modify — `displayModel(*store.EventSummary)`)
- `internal/cli/cli_test.go`, `internal/cli/additions_test.go`, `internal/cli/replay_test.go` (modify —
  fixture literals / helpers)
- `internal/consumer/consumer.go` (modify — `&store.Event{…}` wrap at `:398`)
- `internal/consumer/consumer_test.go` (modify — `waitForEvents` returns `[]*store.EventSummary`)
- `internal/jsonlogs/jsonlogs.go` (modify — `&store.Event{…}` wrap at `:381`)
- `internal/jsonlogs/jsonlogs_test.go` (modify — `eventsByRequestID` returns
  `map[string]*store.EventSummary`)
- `internal/quota/quota.go` (modify — the package `Store` interface's `ListEvents` retypes; `:65` flows
  through)
- `internal/quota/quota_test.go`, `internal/reconcile/reconcile_test.go`,
  `internal/session/session_test.go` (modify — fixture literals)
- `internal/analyze/rules_test.go` (modify — fixture literals, elided elements included)
- `internal/replay/replay_test.go` (modify — fixture literals; `internal/replay/replay.go` itself is
  unchanged)
