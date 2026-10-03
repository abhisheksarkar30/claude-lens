# Bead br-GI-22-01: Fix `prices --set` so every field in one invocation survives

**Plan Reference**: `docs/planning/GI-22-prices-set-clobber-and-rebuild-backfill-gaps.md` v8 (`<!-- version=8, status=converged -->`), §3.1, §5.1, §5.2, §5.4, §8 bead 01. Repo `D:\github\claude-lens`.

- **Bead ID**: br-GI-22-01
- **Priority**: P0 (critical)
- **Status**: done
- **Original Estimate**: 1h
- **Dependencies**: None
- **Blocks**: None
- **Commit**: `GI#22 fix: keep every prices --set field in one invocation (br-GI-22-01)`

## Description

`clens prices --set model:field=value` repeated for the same model in one invocation keeps only the last field. The cause is the frozen snapshot in `applyPriceEdits` (`internal/cli/prices.go:89-128`): `effective := pricing.NewLoader(path, nil).Table()` is taken once at `prices.go:94`, before the `--set` loop. Every iteration reads `r, ok := effective[model]` (`prices.go:101-107`) and writes `overrides[model] = r` (`prices.go:112`) without ever reading `overrides[model]` back. That is true for a model absent from `effective`, for a shipped rate, and for a rate already overridden. The fresh-zero branch (`r = pricing.Rate{Model: model}` when the lookup misses) is one symptom of that lookup, not the cause. A single `--set` never re-reads its own write, so that path is already correct. Two or more `--set`s in the same call are not: each iteration restarts from the snapshot, so an earlier field edit is replaced by the snapshot's value.

Inside the `for _, s := range sets` loop, replace the lookup at `prices.go:101-107` with:

```go
r, ok := overrides[model]
if !ok {
    r, ok = effective[model]
}
if !ok {
    fmt.Fprintf(w, "prices: %s has no shipped rate; defining it from scratch\n", model)
    r = pricing.Rate{Model: model}
}
```

`overrides` is already loaded once via `pricing.LoadOverrides(path)` (`prices.go:90-93`) and is the map every iteration writes back into. Checking it first picks up whatever the previous `--set` in this call already produced. Falling back to `effective[model]`, then to a fresh zero `pricing.Rate`, stays as it is for the first `--set` of a model this call has not yet written.

Do not change `setRateField` (`prices.go:159-184`), `splitSet` (`prices.go:134-149`), or the unset loop (`prices.go:115-117`). The lookup above is the whole production change.

**Message behavior.** With the bug, `prices: %s has no shipped rate; defining it from scratch` fires on every `--set` for a brand-new model, because `effective[model]` misses every time. After the fix it fires only on the first `--set` for that model in the invocation, because later iterations find `overrides[model]` already populated. `TestPricesSetMultipleFieldsNewModelInOneInvocation` must assert the message prints exactly once across five `--set`s.

**Tests.** New file `internal/cli/prices_test.go` (`package cli`). `internal/cli/prices_test.go` does not exist today. Each test calls `withHome(t)` (`internal/cli/doctor_test.go:17-24`) and uses the returned `home` before `runPrices`. `runPrices` reads and writes `pricing.DefaultPath()` (`prices.go:31`), which joins `userHomeDir()` with `.clens/prices.toml` (`internal/pricing/pricing.go:182-187`). `userHomeDir` returns `$HOME` when it is non-empty (`pricing.go:190-198`), so setting only `USERPROFILE` does not isolate the test when `HOME` is already set. `withHome` sets `HOME` to `t.TempDir()` and clears `USERPROFILE`. Assert against `filepath.Join(home, ".clens", "prices.toml")`, not the real home directory. Read that file back with `pricing.LoadOverrides`. Compare `*big.Rat` values to the `--set` decimals (USD per million tokens, the units `setRateField` divides by `1_000_000` at `prices.go:164`). `LoadOverrides` fills a nil cache-write rate from `input_rate` (`pricing.go:295-300`), so a non-nil derived write rate is not proof a `--set` survived — assert the numeric value of each field the test named.

`claude-sonnet-5` is a shipped row (`internal/pricing/table.go:164`). `claude-sonnet-5-5` is not in `table.go` (the shipped sonnet key is `claude-sonnet-5` only). Use `claude-sonnet-5-5` as the brand-new model. The brand-new test must not seed an override for that model; seeding one would no longer be the brand-new case.

The five `--set` keys for the brand-new test are the five non-fast keys `setRateField` accepts (`prices.go:166-175`): `input_rate`, `output_rate`, `cache_read_rate`, `cache_write_5m_rate`, `cache_write_1h_rate`, with `cache_write_1h_rate` last. The reproduced failure left only `cache_write_1h_rate` set.

For the existing-model tests, snapshot the pre-invocation effective rate with `pricing.NewLoader(path, nil).Table()` before `runPrices`. Fields not named by any `--set` must still equal that snapshot afterward. Cover a shipped rate and a previously overridden rate as two subtests of `TestPricesSetMultipleFieldsExistingModelInOneInvocation`, not as a substitute for the single-`--set` tests. Seed a prior-session override at `filepath.Join(home, ".clens", "prices.toml")` using the `SaveOverrides` block shape (`pricing.go:323-327`: `model = "<id>"` then `key = value` lines). `applyPriceEdits` creates nothing until `SaveOverrides`, so the seed must `MkdirAll` the `.clens` directory itself.

## Rationale

One invocation is the documented way to define a rate. Today every field but the last is silently discarded, including on a model that already has a shipped or overridden rate. Callers who pass five `--set`s believe they wrote five fields.

## Outcome Definition

`go test ./internal/cli/ -count=1 -run 'TestPricesSet'` passes, and all four tests below are in that run.

Manual negative control, once, before calling the bead done: temporarily restore the `effective[model]`-only lookup at `prices.go:101`. Both `TestPricesSetMultipleFieldsNewModelInOneInvocation` and `TestPricesSetMultipleFieldsExistingModelInOneInvocation` must fail. The new-model failure is exactly the reproduced symptom: only the last of the five fields is present. The existing-model failure is: only the last `--set`'s field differs from the frozen snapshot, and the earlier field edit is gone. The brand-new failure alone does not prove the existing-model path. A fix that only special-cases a model absent from `effective` would still pass the new-model test while leaving a shipped or previously overridden model's earlier `--set` reverted to the snapshot. Restore the `overrides[model]`-first lookup after that check. Do not commit the reverted lookup.

## Test Specifications

- `internal/cli/prices_test.go`: `TestPricesSetMultipleFieldsNewModelInOneInvocation` — five `--set`s for `claude-sonnet-5-5` in one `runPrices` call, no pre-seeded override for that model. All five fields are present in `filepath.Join(home, ".clens", "prices.toml")` at the values passed in. The writer output contains `has no shipped rate; defining it from scratch` exactly once (`strings.Count` == 1), not five times.
- `internal/cli/prices_test.go`: `TestPricesSetMultipleFieldsExistingModelInOneInvocation` — two or more `--set`s in one `runPrices` call, each touching a different field. Subtest the shipped model `claude-sonnet-5` with no override file. Subtest a model whose prior override was seeded at `filepath.Join(home, ".clens", "prices.toml")` before the call. Both field edits are present afterward. Fields already on that rate and not named by either `--set` stay at their pre-invocation values.
- `internal/cli/prices_test.go`: `TestPricesSetSingleFieldExistingModelUnaffected` — one `--set` against shipped `claude-sonnet-5`. Untouched fields stay at their pre-invocation values, read back from `filepath.Join(home, ".clens", "prices.toml")`.
- `internal/cli/prices_test.go`: `TestPricesSetSingleFieldAgainstPriorSessionOverridePreserved` — seed `filepath.Join(home, ".clens", "prices.toml")` with an existing user override (at least one field populated before this invocation), then one `--set` for a different field on that same model. Every pre-existing field plus the new one is present in that file.
- **Manual / recorded**: the negative control in Outcome Definition. No evidence file. The fix remains in the tree; the revert does not.

## Files to Touch

- `internal/cli/prices.go` (modify — the `overrides[model]`-first lookup inside the `--set` loop only)
- `internal/cli/prices_test.go` (create)

## Review Notes

Built the `overrides[model]`-first lookup in `applyPriceEdits` and the four tests in `internal/cli/prices_test.go`. The function comment now says a later `--set` merges onto the rate this invocation already wrote, then onto the effective snapshot.

`go test ./internal/cli/ -count=1 -run TestPricesSet` — 6 passed (the existing-model test counts its two subtests).

Negative control, not committed: the lookup was put back to `effective[model]` only. `TestPricesSetMultipleFieldsNewModelInOneInvocation` failed with `input_rate`, `output_rate`, `cache_read_rate`, and `cache_write_5m_rate` nil and the from-scratch message printed 5 times; `cache_write_1h_rate` (the last `--set`) was the only field present. `TestPricesSetMultipleFieldsExistingModelInOneInvocation` failed on both subtests: the shipped subtest kept `input_rate` at the snapshot `2.00` (`1/500000`), and the prior-override subtest kept `cache_read_rate` at the seeded `0.40` (`1/2500000`). The lookup was restored before this commit.
