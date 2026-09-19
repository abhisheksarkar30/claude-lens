# Bead br-GI-3-06: `newTailer(cfg, root, st)` — pure extract, behaviour-preserving

**Plan Reference**: `docs/planning/GI-3-deepseek-peak-pricing.md` — §3 D5 (the `newTailer` chain, F5.1), §4 (`refresh.go`/`ingest.go`), round-4 escalation option (b) (plan sketch §9 bead 4b)

- **Bead ID**: br-GI-3-06
- **Priority**: P1 (high)
- **Original Estimate**: 1h
- **Dependencies**: br-GI-3-05
- **Blocks**: br-GI-3-07

## Description

The tailer-construction block currently appears **twice, mirrored**, in
`internal/cli/refresh.go` (`addCollectors`, `refresh.go:91-95`) and `internal/cli/ingest.go`
(`runIngest`, `ingest.go:47-51`). Collapse it into one helper, as a **pure, behaviour-preserving
move**:

```go
// newTailer is the one tailer shape both callers want. root is an explicit
// parameter: runIngest's local root and addCollectors' jsonlRoot() are
// distinct expressions and only one is the settled default.
func newTailer(cfg *config.Config, root string, st collectorStore) *jsonlogs.Tailer {
    t := jsonlogs.New(root, st)
    t.SetPriceTable(newPriceLoader(cfg))
    if acct := firstAccount(cfg, "subscription"); acct.Name != "" {
        t.SetAccount(acct.Name, acct.BillingMode)
    }
    return t
}
```

Home it in `internal/cli/ingest.go` beside `firstAccount` and br-GI-3-05's helpers.

**Root is an explicit parameter — a hard constraint.** `runIngest` holds `root := jsonlRoot()`
(`ingest.go:39`) and `addCollectors` calls `jsonlRoot()` inline (`refresh.go:91`). They are distinct
expressions, and a helper that resolved the root internally would silently drop ingest's. Pass it
in.

**The subscription step is moved verbatim, guard included (F5.1).** The block

```go
if acct := firstAccount(cfg, "subscription"); acct.Name != "" {
    t.SetAccount(acct.Name, acct.BillingMode)
}
```

moves unchanged. The `acct.Name != ""` guard is **load-bearing, not decorative**: `SetAccount`
assigns unconditionally (`internal/jsonlogs/jsonlogs.go:102-105`) and `New` seeds `"subscription"`
(`:91-93`), so a zero `Account` on an install with no subscription account would blank the mode and
mis-bill every row into `cost_usd` (the sharp edge recorded in §8). The diff must show a **move**,
not a rewrite — a reviewer must be able to see the guard arrived intact.

**`SetModelBilling` is out of scope.** It joins the helper in br-GI-3-07, so this bead's diff
contains only the move.

**Only construction collapses.** The two callers keep their own drive logic:
`resetJSONLCursors` + one `Poll` in `runIngest`; collector registration in `addCollectors`. After
the change:

- `addCollectors`: `tailer := newTailer(cfg, jsonlRoot(), st)`
- `runIngest`: `tailer := newTailer(cfg, root, st)`

**Why this lands before br-GI-3-07**: so br-GI-3-07's diff shows only the semantic change (D5's
routing) and stays bisectable. Landing the wiring first would bury the extract inside a semantic
diff.

## Rationale

D5's semantic change (per-row billing routing) must land in **one** place, not mirrored per site.
This extract is the prerequisite that makes that possible without the two call sites drifting. It
is deliberately its own bead and deliberately has no behaviour change, so a bisect over the story
never blames the extract for a routing bug.

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass with **no behaviour change**.
- Both `addCollectors` and `runIngest` build their tailer through `newTailer`; neither retains a
  bare `jsonlogs.New` + `SetPriceTable` + `SetAccount` sequence.
- The `acct.Name != ""` guard is present in the moved block, unchanged.
- `newTailer` takes the root as an explicit parameter; `runIngest`'s local `root` and
  `addCollectors`' `jsonlRoot()` are both passed through, neither unified away.
- `SetModelBilling` is **not** added here.
- The existing `internal/cli` ingest/refresh tests pass unchanged.

## Test Specifications

- Unit Tests (`internal/cli`, existing test files): the existing `clens ingest`/`clens refresh`
  tests pass unchanged — they are the behaviour-preservation guard.
  - One new case asserting the guard's contract: `newTailer` on a config with **no** subscription
    account leaves the tailer's account empty and `billing_mode == "subscription"` (the `New`
    seed), while a config **with** a subscription account applies that account — i.e. a zero
    `Account` never blanks the mode (F5.1).
- Integration Tests: none (br-GI-3-07 asserts the routing this helper will carry).
- E2E: none.

## Files to Touch

- `internal/cli/ingest.go` (modify — host `newTailer`; `runIngest` calls it with `root`)
- `internal/cli/refresh.go` (modify — `addCollectors` calls `newTailer(cfg, jsonlRoot(), st)`)
- `internal/cli/cli_test.go` or `internal/cli/serve_test.go` (modify — the guard case)
