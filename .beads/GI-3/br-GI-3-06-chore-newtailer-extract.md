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

- Unit Tests (`internal/jsonlogs/jsonlogs_test.go`, **not** `internal/cli`): the F5.1 contract the
  guard defends, asserted where the fields are readable. `account`/`billingMode` are unexported
  (`jsonlogs.go:82-83`) and `Tailer` exposes no accessor — only the four setters, `Poll`, and
  unexported internals (`jsonlogs.go:95-105`, `:132`, `:291`, `:366`) — so an assertion on tailer
  state is **unwritable from package `cli`**, and no test in `internal/cli` builds a tailer at all
  (its fixtures write rows straight to the store via `seedEvent`). Assert instead:
  - `New(root, st)` leaves the account empty and `billingMode == "subscription"` (the `:91-93` seed).
  - `SetAccount("", "")` assigns **unconditionally**, blanking the mode to `""` — the sharp edge
    recorded in §8, and the reason the `acct.Name != ""` guard is load-bearing at every call site
    rather than decorative.
- Unit Tests (`internal/cli`, existing test files): the existing `clens ingest`/`clens refresh`
  tests pass unchanged — they are the behaviour-preservation guard for the extract.
- **Deliberately not test-covered.** That `newTailer` *keeps* the guard is a pure-move property,
  verified by reading the diff. Making it test-observable would require either a new exported
  accessor on `Tailer` (API added to satisfy one test) or a full poll harness in `internal/cli`
  that does not exist today. The bead's existing "the diff must show a move, not a rewrite"
  requirement is the honest control; the `jsonlogs` case above pins the contract the guard
  protects, so an edit that drops the guard shows up as a diff against a stated rule.
- Integration Tests: none (br-GI-3-07 asserts the routing this helper will carry).
- E2E: none.

## Files to Touch

- `internal/cli/ingest.go` (modify — host `newTailer`; `runIngest` calls it with `root`)
- `internal/cli/refresh.go` (modify — `addCollectors` calls `newTailer(cfg, jsonlRoot(), st)`)
- `internal/jsonlogs/jsonlogs_test.go` (modify — the F5.1 guard contract; see Test Specifications
  for why this case cannot live in `internal/cli`)
