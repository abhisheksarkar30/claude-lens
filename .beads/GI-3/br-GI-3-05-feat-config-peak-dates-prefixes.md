# Bead br-GI-3-05: Config keys, `Validate`, the `none` sentinel, `newPriceLoader`/`resolvedAPIPrefixes`, and `doctor` rows

**Plan Reference**: `docs/planning/GI-3-deepseek-peak-pricing.md` — §3 D4 (F2.1/F2.4/F2.5/F3.2), §4 (`config.go`, `refresh.go`, `ingest.go`, `serve.go`, `doctor.go`), §5 T10/T15 (plan sketch §9 bead 4)

- **Bead ID**: br-GI-3-05
- **Priority**: P1 (high)
- **Original Estimate**: 2h
- **Dependencies**: br-GI-3-04
- **Blocks**: br-GI-3-06

## Description

Make holiday exclusion and model-prefix routing configurable, and route every loader through one
helper.

**1. Two config keys (three touchpoints each).** Add to `internal/config/config.go`:

| Field | Env | Format |
|---|---|---|
| `PeakOffPeakDates []string` | `CLENS_PEAK_OFF_PEAK_DATES` | comma-separated `YYYY-MM-DD` |
| `ApiModelPrefixes []string` | `CLENS_API_MODEL_PREFIXES` | comma-separated model prefixes |

Comma-separated is forced by `parseFlatFile`, which is flat `key = value` with no list syntax.
Three touchpoints: the `Config` struct fields, `fieldsByEnv` entries, and `applyKV` cases (the
`default:` arm rejects unknown keys, `config.go:171-172`). There are **no flags** for these keys.

**Nil means the shipped default; `none` means empty.** `config.Default()` (`config.go:45-59`)
leaves both fields **nil** — deliberately absent from the enumerated defaults — and nil resolves to
the shipped default: the 33-date list in `internal/pricing/table.go` for the dates, and
`pricing.ShippedAPIModelPrefixes()` = `{"deepseek-"}` for the prefixes (F2.1). No config edit is
required for the DeepSeek repair to fire.

nil vs. non-nil-empty is a real distinction:

| field | `nil` (no key set) | non-nil, zero-length (`none`) | non-nil list |
|---|---|---|---|
| `PeakOffPeakDates` | use the shipped 2026 list | exclude **no** dates | exactly those dates |
| `ApiModelPrefixes` | use the shipped `{"deepseek-"}` | **no routing at all** | **replaces** the shipped list wholesale |

`none` is the only way a *file* can produce the empty slice: `applyKV` skips `val == ""` entirely
(`config.go:143-146`), so a blank value in the file is a no-op, not an empty list. A **non-nil**
prefix list **replaces** the shipped list — `CLENS_API_MODEL_PREFIXES=acme-` drops `deepseek-`.

**2. `Config.Validate` checks both.** Reject a malformed date (`time.Parse("2006-01-02", s)`
failure) — a typo'd date silently over-charges at peak — and a malformed prefix (empty or
whitespace-only). Errors name the field and value, matching the existing style.

**3. New `Validate()` calls in `runIngest` and `runRefresh` (F1.2/R8).** `config.Load` does **not**
call `Validate` itself (`config.go:203-238`), so the `clens ingest --rebuild` backfill path is validated **only** once
`runIngest` calls it — and today it does not. Add `cfg.Validate()` to `runIngest`
(`internal/cli/ingest.go`, after `config.Load`, before the store is opened) and to `runRefresh`
(`internal/cli/refresh.go`, before `addCollectors`). `runRefresh` is the site, not `addCollectors`:
`addCollectors` has no error return (`refresh.go:90`), so a `Validate()` there could only be
discarded (F5.2). `runServe`/`runDoctor` already call it.

**4. Two helpers, three sites (F3.2).** Home both in `internal/cli/ingest.go` beside the existing
shared `firstAccount` (`ingest.go:70`):

```go
// resolvedAPIPrefixes: nil means "unset -> use the shipped default".
func resolvedAPIPrefixes(cfg *config.Config) []string {
    if cfg.ApiModelPrefixes == nil {
        return pricing.ShippedAPIModelPrefixes()
    }
    return cfg.ApiModelPrefixes
}

// newPriceLoader is the one loader shape every pricer in the process wants.
func newPriceLoader(cfg *config.Config) *pricing.Loader {
    return pricing.NewLoader(pricing.DefaultPath(), cfg.PeakOffPeakDates)
}
```

Route all three **loader** sites through `newPriceLoader(cfg)`: `serve.go:101` (the consumer's
loader), `addCollectors` (`refresh.go:92`, shared by `serve` and `refresh`), and `runIngest`
(`ingest.go:48`). This makes F2.4's "two pricers cannot disagree" **structural** rather than
something a reviewer re-verifies by hand.

`serve.go:101` takes the loader helper and **nothing else** — `resolvedAPIPrefixes` is consumed
only *behind* a tailer, where `SetModelBilling` lives (br-GI-3-07). The consumer resolves account
and billing mode from the captured credential (`consumer.go:310`, `:424-432`), never from a model
prefix, so there is no prefix seam there. **Why the helper matters (F3.2):** a site that misses the
nil→shipped resolution fails silently — the resolved list is only ever fed to `strings.HasPrefix`,
and `strings.HasPrefix` over a **nil** slice never matches, yielding zero routing with no error.

**5. `doctor` shows both keys, resolved (F2.5, T15).** `doctor`'s job is "print effective config",
and its table is hand-enumerated (`internal/cli/doctor.go:47-60`). Add `peak_off_peak_dates` and
`api_model_prefixes`, each rendered from the **effective** value, not the raw slice:
`33 (default)` / `none` / `N` for the dates, and `deepseek- (default)` / `none` / the joined list
for the prefixes. Printing `len(cfg.PeakOffPeakDates)` raw would print `0` in exactly the state
where the 33-date default is in force — the state the row exists to show.

## Rationale

Configurability is an explicit user requirement of this story; a key the "print effective config"
command cannot show is an incomplete feature. The helper is not ordinary deduplication: a site that
misses the nil→shipped resolution silently yields zero routing, which is the exact failure F2.1 was
accepted to remove, and hand-resolving it at three call sites is how it returns at one of them.

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass.
- Both keys parse from **file and env** (no flags); a malformed date is rejected by `Validate`; an
  empty/whitespace-only prefix is rejected.
- `none` yields a non-nil empty slice for either key; an unset key yields `nil`.
- `resolvedAPIPrefixes(cfg)` resolves an unset `ApiModelPrefixes` to `{"deepseek-"}`; a non-nil list
  (`acme-`) replaces it wholesale.
- All three loader sites call `newPriceLoader(cfg)` (grep-ably no bare `pricing.NewLoader(...)` left
  in `serve.go`/`refresh.go`/`ingest.go`).
- `clens doctor` prints `33 (default)` and `deepseek- (default)` when both keys are unset, and
  `none`/`N` (and the joined list) when set.

## Test Specifications

- Unit Tests (`internal/config/config_test.go`):
  - **T10**: both keys parse from file and from env; a malformed date is rejected by `Validate`; an
    empty/whitespace-only prefix is rejected; `none` yields a non-nil empty slice for either key;
    an unset key yields `nil`.
  - `resolvedAPIPrefixes` resolves an unset `ApiModelPrefixes` to the shipped `{"deepseek-"}` and a
    non-nil `acme-` list replaces it wholesale — asserted on the helper, not on an unreachable
    wiring site (F2.1/F3.2).
- Unit Tests (`internal/cli/doctor_test.go`):
  - **T15**: `doctor` prints `33 (default)` and `deepseek- (default)` when both keys are unset, and
    `none`/`N` (and the joined list) when set.
- Integration Tests: `runIngest`/`runRefresh` reject a malformed date before touching the store (a
  fixture config file with a bad date makes `Ingest`/`Refresh` return an error).
- E2E: none.

## Files to Touch

- `internal/config/config.go` (modify — 2 fields, 2 `fieldsByEnv` entries, 2 `applyKV` cases,
  `Validate` date/prefix checks, `none` sentinel)
- `internal/config/config_test.go` (modify — T10)
- `internal/cli/ingest.go` (modify — `resolvedAPIPrefixes`/`newPriceLoader` helpers beside
  `firstAccount`; `newPriceLoader(cfg)` at the loader site; `cfg.Validate()` in `runIngest`)
- `internal/cli/refresh.go` (modify — `newPriceLoader(cfg)`; `cfg.Validate()` in `runRefresh`)
- `internal/cli/serve.go` (modify — `serve.go:101` → `newPriceLoader(cfg)`)
- `internal/cli/doctor.go` (modify — two resolved rows)
- `internal/cli/doctor_test.go` (modify — T15)
