# Bead br-GI-3-04: `reload()` re-takes `Peak`, `NewLoader(path, dates)` signature, zero-write write-rate inheritance

**Plan Reference**: `docs/planning/GI-3-deepseek-peak-pricing.md` — §3 D3/D4 (Ordering, F2.2/F2.3/F3.1), §4 (`pricing.go`, `api/prices.go`, `serve.go`/`prices.go`/`models.go` call sites), §5 T5 (Loader half)/T13/T17(b) (plan sketch §9 bead 2 second half + bead 3 signature)

- **Bead ID**: br-GI-3-04
- **Priority**: P0 (critical)
- **Original Estimate**: 2h
- **Dependencies**: br-GI-3-02, br-GI-3-03
- **Blocks**: br-GI-3-05, br-GI-3-09, br-GI-3-10

## Description

Three coupled changes to `internal/pricing`, testable now that br-GI-3-03's shipped `Peak` rows
exist.

**1. `reload()` re-takes `Peak` from the shipped row, after the override merge (F2.3).**
`Loader.reload()` (`internal/pricing/pricing.go:315-329`) merges overrides wholesale — an override
replaces the whole `Rate` (`pricing.go:315-329`), so without a re-take `clens prices --set
deepseek-flash` would silently revert the model to flat pricing (R4). `LoadOverrides` does **not**
stamp `Peak`; instead `reload()` applies the resolved off-peak list as its **final** step, over
every row that carries a `Peak` window:

- Take the window from the **shipped** row (not the override, which has none).
- Write the resolved dates into a **freshly-built window** — never mutate a shared or package-level
  instance (F3.1). With a nil configured list, the shipped 33-date window stands; with a non-nil
  list, its dates replace the shipped ones (an empty slice yields an empty exclusion set).
- Because `Peak` is re-taken from the shipped row here, an override can never clobber the
  configured dates — one place, applied uniformly, which also covers the `POST /api/prices` path
  for free.

**2. `NewLoader` gains an off-peak-dates parameter.**
`NewLoader(path string, offPeakDates []string) *Loader`, stored as a `Loader` field. A **required**
argument, deliberately: it forces the compiler to enumerate every call site rather than trusting a
human to remember a `SetOffPeakDates` call. `nil` means "use the shipped default"; a non-nil list
replaces it.

Update **all 6 production call sites** and **9 test call sites** to pass `nil` in this bead — the
build must stay green:

| Production site | New argument |
|---|---|
| `internal/cli/serve.go:101` (consumer's loader) | `nil` — br-GI-3-05 re-routes it through `newPriceLoader(cfg)` |
| `internal/cli/refresh.go:92` (`addCollectors`) | `nil` — br-GI-3-05 re-routes |
| `internal/cli/ingest.go:48` (`runIngest`) | `nil` — br-GI-3-05 re-routes |
| `internal/cli/models.go:34` (display-only) | `nil` — stays `nil` |
| `internal/cli/prices.go:50` (display-only) | `nil` — stays `nil` |
| `internal/cli/prices.go:94` (display-only) | `nil` — stays `nil` |

The three display-only sites keep `nil` permanently: the shipped list is what a display-only loader
wants, and `runPrices` never loads config at all (`internal/cli/prices.go:26-31`), so it has no
`cfg` to pass. Test sites: `internal/api/api_test.go:336`; `internal/api/prices_test.go:17,38,82,114,125,143`;
`internal/pricing/pricing_test.go:199,222`.

**3. Zero-write write-rate inheritance in `LoadOverrides` and the `POST /api/prices` builder
(F2.2).**
Both surfaces can re-invent a cache-write fee for a DeepSeek row:

- `LoadOverrides` (`pricing.go:219-224`) derives `1.25x`/`2x` input for any override that omits
  the write fields — for a DeepSeek model that would price cache writes at 1.25x input instead of
  `0`.
- `POST /api/prices` (`internal/api/prices.go:94-114`) builds a fresh `Rate` holding only the
  fields the request sent, so posting just `input_rate`/`output_rate` reintroduces the
  Anthropic-shaped write premium.

**Rule (narrowed):** when the shipped table already has a row for the model **and that row's own
write rates are zero**, both surfaces **inherit `CacheWrite5mRate` and `CacheWrite1hRate`** from
that shipped row before the derivation runs. Otherwise the `1.25x`/`2x` derivation stands, so a
partial override on a shipped **Claude** row still prices cache writes at `1.25x`/`2x` of the
*effective* (possibly overridden) input rate — the invariant `rate()` guarantees at
`table.go:21-22`. Inheriting unconditionally would break that invariant (an override setting
`input_rate = 5.00` on a shipped Claude model would otherwise keep writes derived from the *old*
shipped input). The zero-write guard is exactly the case the inheritance is for, and nothing wider.

`internal/api` must not import `pricing` internals it does not already; expose a small exported
helper from `internal/pricing` so the rule has one home rather than being duplicated.

**Corrected: the second surface is `LoadOverrides` alone, and `internal/api/prices.go` needs no
edit.** The api builder was thought to re-derive independently. It does not. It sends its fields
through `pricing.SetOverride` → `SaveOverrides`, which writes only the non-nil ones, and the Loader
then reads that file back through `LoadOverrides` — which is where the 1.25x/2x derivation actually
runs. So the POST path reaches the same code as the file path, and fixing `LoadOverrides` fixes
both. Verified by mutation: disabling only the `LoadOverrides` inheritance fails the api-surface
test with the same invented fee as the loader test. Adding inheritance to the builder as well would
be worse than redundant — the inherited rates would be non-nil, so `SaveOverrides` would write them
into the user's override file, pinning the shipped zero there and stopping a later shipped-rate
change from flowing through.

**Invariant 5.** Inheritance must never turn a zero write rate into a non-nil non-zero rate on a
DeepSeek row, and must never zero a Claude row's derived writes. Money stays exact via `big.Rat`.

## Rationale

Without the re-take, a DeepSeek price edit silently reverts the model to flat pricing; without the
write-rate inheritance, a partial override or POST prices DeepSeek cache writes at 1.25x/2x input
instead of `0`. Both are R4. The signature change is the deliberate compile-time forcing function
the plan chose over a `SetOffPeakDates` setter; the display-only sites pinning `nil` is F4.4.

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass.
- A `Loader` built with a custom one-date list plus an override on `deepseek-flash` still excludes
  exactly that one date (the override does **not** revert the model to the shipped 33) — T13; and
  a `none` list yields no exclusions.
- A partial override (`input_rate`/`output_rate` only) on a DeepSeek model through the Loader
  yields write rates of `0` and a non-nil `Peak`; the same shape on `claude-sonnet-5` keeps
  `cache_write_5m_rate == 1.25 x input` and `cache_write_1h_rate == 2 x input` of the *override's*
  input — T5's Loader half.
- A loader built with a non-default list leaves `ShippedTable()`'s DeepSeek `Peak.OffPeakDates` at
  the shipped 33, and a **second** loader built with a different list resolves independently —
  T17(b). This guard is **deterministic**: it must not depend on `-race`, because `go test ./...`
  does not enable the race detector (F3.1).
- The `POST /api/prices` path inherits a zero-write shipped row's write rates the same way — the
  api-surface half of T5, asserted here, because this bead is what changes the rule it routes
  through. The assertion lives in `internal/api/prices_test.go` even though the edit is in
  `internal/pricing`: it is the surface claim, and a change to `SetOverride`/`SaveOverrides` that
  stopped routing through `LoadOverrides` would break it and nothing in `internal/pricing`.

## Test Specifications

- Unit Tests (`internal/pricing/pricing_test.go`):
  - **T5 (Loader half)**: partial override on a shipped DeepSeek model → write rates `0`, `Peak`
    non-nil; partial override on `claude-sonnet-5` → `1.25x`/`2x` of the override input.
  - **T5 (API half)**: `POST /api/prices` carrying only `input_rate`/`output_rate` for a shipped
    DeepSeek model writes a `Rate` whose write rates are `0` (inherited from the shipped row, not
    re-derived at `1.25x`/`2x`); the same partial POST for `claude-sonnet-5` writes `1.25x`/`2x` of
    the **posted** input. The api-surface half is asserted here, not deferred: this bead is what
    changes `internal/api/prices.go`, and deferring it left the change verified only by "it
    compiles" — pointing at a bead (br-GI-3-09) that specifies no such test.
  - **T13**: custom one-date list + an override on `deepseek-flash` still excludes exactly that one
    date; `none` (non-nil empty slice) yields no exclusions.
  - **T17(b)**: a loader with a custom list leaves `ShippedTable()`'s 33 dates unchanged, and a
    second loader with a different list resolves independently (no shared-window hazard).
- Unit Tests (`internal/pricing/pricing_test.go`, call sites): the existing loader tests
  (`pricing_test.go:199,222`) updated to the two-argument `NewLoader`.
- Integration Tests: `internal/api` call-site updates compile. The api-surface **behavioural** case
  is T5 (API half) above, in this bead — not br-GI-3-09, which specifies no such test.
- E2E: none.

## Files to Touch

- `internal/pricing/pricing.go` (modify — `reload()` re-take into a fresh window, `NewLoader`
  signature + `Loader` dates field, `LoadOverrides` zero-write inheritance)
- `internal/pricing/pricing_test.go` (modify — T5 Loader half, T13, T17(b), call sites)
- `internal/pricing/table.go` (modify — `ZeroWriteShippedRates`, the one home for the rule)
- `internal/api/prices.go` (**not modified** — see the correction above; the POST path reaches the
  rule through `LoadOverrides`, and inheriting in the builder would write the shipped zero into the
  user's override file)
- `internal/api/api_test.go`, `internal/api/prices_test.go` (modify — 7 `NewLoader` call sites;
  T5's API half lands in `prices_test.go`)
- `internal/cli/serve.go`, `internal/cli/refresh.go`, `internal/cli/ingest.go` (modify — the three
  live loaders pass `nil`; br-GI-3-05 re-routes them)
- `internal/cli/models.go`, `internal/cli/prices.go` (modify — the three display-only loaders pass
  `nil`, permanently)
