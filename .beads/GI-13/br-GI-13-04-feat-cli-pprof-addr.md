# Bead br-GI-13-04: `--pprof-addr` as a config-resolved, loopback-locked profiler, plus the README runbook

**Plan Reference**: `docs/planning/GI-13-session-pass-cost.md` — §3.4 (C4), §5 D5/D6/D7, §6 test 8,
§8

- **Bead ID**: br-GI-13-04
- **Priority**: P2 (medium)
- **Original Estimate**: 3h
- **Dependencies**: None (independent of br-GI-13-01/02/03)
- **Blocks**: br-GI-13-06 (the re-profile uses this listener)

> **A profile is a dump of whatever is in memory**, which for this process includes prompt and response
> bodies. That is why this listener is loopback-only and why `--allow-remote` must not widen it. Read
> D5 before changing the validation.

## Description

The profile that found this bug came from an uncommitted local patch to `internal/cli/serve.go`
(`:117`, `:425-440`) that reads `os.Getenv("CLENS_PPROF_ADDR")` directly. Make it a real, flag-gated,
config-resolved feature so the *next* investigation does not re-derive the patch.

### `internal/config/config.go`

- Add `PprofAddr string` to `Config` (`:30-60`), left as the **zero value** in `Default()` (`:63-77`) —
  disabled unless asked for, with the same "deliberately absent from `Default()`" note
  `PeakOffPeakDates` carries (`:56-59`).
- Add `"CLENS_PPROF_ADDR": "PprofAddr"` to `fieldsByEnv` (`:104-118`) — the same env name the local
  patch already uses, so an existing invocation keeps working.
- Add a `PprofAddr` case in `applyKV` (`:180`) and `--pprof-addr` in `applyFlags` (`:226`).

### `Validate` (`:319`) — the literal `false` is the point

```go
if c.PprofAddr != "" {
    validateLoopback("PprofAddr", c.PprofAddr, false)
}
```

`validateLoopback` returns `nil` immediately when `allowRemote` is true (`:378-380`), so reusing it
*with* `c.AllowRemote` would let `--allow-remote` open a listener whose heap profile contains whatever
is in memory. Hardcoding `false` reuses the function and closes that door (D5). This listener is
loopback-only and `--allow-remote` cannot widen it.

### One home for "what counts as loopback"

Today `validateLoopback` (`config.go:373-388`) accepts any loopback IP via `ip.IsLoopback()`, while
`startPprof` (`serve.go:430`) accepts only `127.0.0.1`, `::1`, `localhost`. The mismatch is fail-closed
but costs a legitimately-loopback `127.0.0.2` a silent refusal after passing validation — two spellings
of one predicate, this repo's own named defect class. Settle it by extracting
`config.IsLoopbackHost(host string) bool` and calling it from **both** `validateLoopback` and
`startPprof`.

> **The extracted predicate must keep `"localhost"`.** `net.ParseIP` returns `nil` for it, so a naive
> `ip.IsLoopback()`-only function would accept two of the three spellings and regress a working
> `--proxy-addr localhost`. Both sites check `host == "localhost"` **before** parsing today
> (`config.go:381`, `serve.go:430`); the shared predicate must preserve that order. One definition,
> two callers, strictness decided in one place.

### `internal/cli/serve.go`

Replace `startPprof(os.Getenv("CLENS_PPROF_ADDR"))` (`:117`) with `startPprof(cfg.PprofAddr)`, and make
`startPprof`'s host check call `config.IsLoopbackHost`. The refusal stays as the second layer and keeps
its rationale comment; it now agrees with `Validate` by construction.

### `internal/cli/doctor.go`

One printed line, so the feature is discoverable instead of folklore — when `PprofAddr` is unset, name
the flag that enables it.

### `README.md`

A short profiling subsection. This is the runbook home, **not** `docs/context/` — the repo settled
that in GI-3's D7 and GI-5's §10, and `docs/context/build-and-run.md:128-129` states the rule: that
tree is generated, so a hand-written runbook there is clobbered by the next refresh.

**What the subsection carries** (so the tool generalizes past this one bug):

- The four cost-free profiles a hang investigation wants: `/debug/pprof/profile?seconds=30` (CPU),
  `/debug/pprof/heap`, `/debug/pprof/goroutine?debug=2` (all goroutines and their stacks — the single
  highest-value artifact for a hang), `/debug/pprof/trace?seconds=5`.
- The **method** that cracked this case: a no-DB route (`/api/health`) answering in ~5 ms while every
  DB route takes tens of seconds is the signature of `SetMaxOpenConns(1)` contention, and it
  distinguishes contention from a starved runtime or a slow disk in one curl.

## Rationale

`dlv attach` suspends the proxy and a SIGBREAK stack dump exits it, and either would kill the client
whose traffic routes through this process. A flag-gated `net/http/pprof` listener is the only way to
profile the live process safely; shipping it as a real feature is the second half of this story's
request. D7 keeps it small: a config field, a flag, and 12 lines that already exist in the working
tree — no `internal/diagnostics` package, no profile-capture helper.

## Outcome Definition

- `PprofAddr` resolves from flag, env and file at the documented precedence (flag > env > file >
  default), defaulting to empty (listener off).
- `Validate` rejects a non-loopback `PprofAddr` **even when `AllowRemote` is true**, and accepts an
  empty one as clean (the default).
- `config.IsLoopbackHost` accepts `127.0.0.1`, `::1`, `127.0.0.2` (any loopback IP) and `localhost`,
  and rejects a public host; both `validateLoopback` and `startPprof` call it.
- `clens doctor` prints a line naming `--pprof-addr` when the listener is unset.
- `README.md` documents enabling it and the four profiles plus the no-DB-vs-DB method.
- `--allow-remote` **cannot** open the pprof listener on a non-loopback address.
- The hot path is untouched; `go test ./internal/proxy/` (the TTFB gate) is unchanged.
- **Verification** (from the repo root): `go build ./... && go vet ./... && go test ./... -count=1`,
  plus `go test ./internal/config/ ./internal/cli/`.

## Test Specifications

- Unit Tests (`internal/config/config_test.go`):
  - `TestPprofAddrPrecedence`: flag beats env beats file; unset leaves the zero value (§6 test 8).
  - `TestValidateRejectsNonLoopbackPprofAddrEvenWithAllowRemote`: a public `PprofAddr` with
    `AllowRemote = true` still fails — **D5's test**, the one that catches a later "tidy-up" passing
    `c.AllowRemote` to `validateLoopback` (§6 test 8).
  - `TestValidateAcceptsEmptyPprofAddr`: the default validates clean.
  - `TestIsLoopbackHost`: accepts `127.0.0.1`, `::1`, `127.0.0.2`, `localhost`; rejects a public host;
    the `localhost` case is the one a naive `ip.IsLoopback()`-only predicate fails (§3.4).
- Unit Tests (`internal/cli/serve_test.go` or `doctor_test.go`):
  - `TestDoctorNamesThePprofFlagWhenUnset`.
  - `TestStartPprofRefusesANonLoopbackHost`: `startPprof` on a public host logs the refusal and starts
    nothing (kept as the second layer, now via `config.IsLoopbackHost`).
- Integration Tests: none.

## Files to Touch

- `internal/config/config.go` (modify — `PprofAddr` field + `Default` note, `fieldsByEnv`, `applyKV`,
  `applyFlags`, `Validate`, and the extracted `IsLoopbackHost`; `validateLoopback` refactored to call
  it)
- `internal/cli/serve.go` (modify — call site `:117`; `startPprof` (`:425-440`) host check →
  `config.IsLoopbackHost`)
- `internal/cli/doctor.go` (modify — one printed line)
- `README.md` (modify — profiling subsection)
- `internal/config/config_test.go`, `internal/cli/serve_test.go`/`doctor_test.go` (modify — the cases
  above)

No other bead touches `config.go`, `serve.go`, `doctor.go` or `README.md`, so this bead's file set is
disjoint from its siblings'.
