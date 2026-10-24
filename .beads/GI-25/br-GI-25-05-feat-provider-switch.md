# Bead br-GI-25-05: Live-swappable provider target — `proxy.Targets`, request stamping, persisted state

**Plan Reference**: `docs/planning/GI-25-local-llm-toggle-and-store-hardening.md` v8, §2 R1, §3.5, §4.1, §5.1 ("Switch races a request"), §5.2 (`TestSetLocalFailsWithNoURL`, `TestProviderBootFallsBackToCloud`, `TestTargetsSwitchIsLiveAndTagged`, `TestProviderPersistsAcrossRestart`, `TestValidateLocalUpstreamLoopback`), §6 R1/R2, §7 (pre-flight checks a/b/c), Change History v3–v7 (the `Targets`/`NewWithTargets` wiring's four-round settling) and v8's carried "empty-stateDir no-I/O" test. Repo `D:\github\claude-lens`.

- **Bead ID**: br-GI-25-05
- **Priority**: P0
- **Status**: pending
- **Original Estimate**: 2h
- **Dependencies**: br-GI-25-04 (needs `config.BillingLocal`/`LocalAccount` to exist for the tailer-prefix wiring, and `sink.CapturedCall.Provider` to already be a field to stamp into)
- **Blocks**: br-GI-25-06
- **Commit**: `GI#25 feat: a live-swappable cloud/local provider target (br-GI-25-05)`

## Description

### First step — the three pre-flight checks against the real LM Studio server (plan §7), by hand, before writing any code that depends on their answer

**Run these at the very start of Phase 5, before br-GI-25-01, not when this bead is reached.** They need only a running LM Studio and `curl`, no code, and check (a) shapes the dedup assumptions in br-GI-25-04's merge rule; a failure there is a stop-and-ask for the user, and finding it after beads 01-04 are committed wastes work. Record the outcome in this bead's Review Notes either way.

1. **(a) Message id in the response body — stop-and-ask if this fails.** Send one request to the running LM Studio server's `/v1/messages` and inspect the response JSON for a `message.id` field that matches what Claude Code would write to its own JSONL transcript for the same call. The proxy's cross-source dedup (`internal/jsonlogs/dedup.go:99-115`) matches on this body id; without it, a proxy row gets a synthetic key (`consumer.go:456-466`) and the JSONL row gets a `jsonl:`-prefixed key, so the two never merge — a local call then produces **two** rows and **two** projected costs. The plan's merge design in §3.4 (br-GI-25-04) has **no fallback path** for a missing body id. If this check fails, stop this bead and surface the gap to the user rather than writing `Targets`/wiring code against an assumption that does not hold — this is exactly the ambiguity the plan says must not be silently designed around.
2. **(b) Usage frames in the streamed response.** Confirm the streamed SSE response from LM Studio carries a usage frame (`input_tokens`, `output_tokens`), the same shape `parse.ExtractUsage` already parses from Anthropic's stream. Without it every local row is zero-token, priced at $0.00 (actually `nil`/`"unpriced"` per invariant 5) and useless for the projected-cost feature this ticket exists to add.
3. **(c) Model id accepted unchanged.** Confirm LM Studio accepts the model id Claude Code sends (e.g. `claude-sonnet-4-6`) as-is. The proxy rewrites no request bodies (`internal/proxy/proxy.go`'s `Rewrite` only calls `pr.SetURL`, never touches the body); if LM Studio requires its own model id, a body-rewrite step is a plan extension this bead does not add — note the finding instead.

Record the outcome of all three checks in this bead's Review Notes (pass/fail and what was observed), even if all three pass — this is the one place in the ticket a real external dependency is checked by hand, and the record is what makes a later regression (an LM Studio upgrade changing its response shape) diagnosable.

### `internal/proxy/targets.go` (new file)

```go
type Targets struct {
	cloud, local *url.URL
	path         string // stateDir/"provider", or "" when stateDir == ""
	mu           sync.Mutex // held inside Set only — serializes the file write and the pointer store
	active       atomic.Pointer[string]
}

func NewTargets(cfg *config.Config, stateDir string) (*Targets, error)
func (t *Targets) Active() string          // one atomic load, no lock — the hot-path read
func (t *Targets) Set(which string) error  // "cloud" | "local"
```

- `NewTargets` parses both `cfg.UpstreamURL` (always present, already validated) and `cfg.LocalUpstreamURL` (may be empty — "local" not configured). When `stateDir != ""`, `path = filepath.Join(stateDir, "provider")` (use `filepath.Join`, not string concatenation — a bare `stateDir+"/provider"` was an earlier round's bug, fixed and now the contract: `path` stays `""` when `stateDir == ""`, and **both** `Set` and the boot-time state-file read are no-ops in that case — no I/O at all). When `path != ""`, read the one-word state file: absent, unreadable, unrecognised content, or the content is `"local"` but `cfg.LocalUpstreamURL == ""` → fall back to `"cloud"`. Otherwise the file's content is the initial `Active()` value. `NewTargets` **writes nothing** — it is read-only at boot; the file is only ever written by `Set`.
- `Active() string` is a single `atomic.Pointer[string]` load — no lock, no allocation, because `Rewrite` calls it on every request (hot path, R2 in §6: no lock, no per-request allocation beyond the stamp, no parse).
- `Set(which string) error` validates `which` is `"cloud"` or `"local"`; rejects `"local"` with an error when `t.local == nil` (i.e. `cfg.LocalUpstreamURL` was never configured) — this is the same gate enforced at boot (the fallback above) and at the API layer (br-GI-25-06), stated once here. Holds `t.mu` for the duration of the call (file write + pointer store), so two concurrent `POST /api/provider` calls cannot interleave a write and a store. Writes the one word atomically (temp file + rename, the same pattern any other atomic single-file state write in this repo uses) when `t.path != ""`; skips the write when `t.path == ""`.

### `proxy.go` wiring

- `Rewrite` (`proxy.go:42-44`) reads `targets.Active()` instead of closing over a single parsed `target` — resolve to `t.cloud` or `t.local` based on the returned string, then `pr.SetURL(resolved)`.
- Each request stamps `st.captureState`'s provider from the **same** `Active()` read `Rewrite` used (read it once per request, not twice, so a switch mid-request cannot stamp a different value than the one that actually served the request — this is what `TestTargetsSwitchIsLiveAndTagged`'s `-race` assertion checks). `sink.CapturedCall.Provider` (added in br-GI-25-04) is populated from this stamp.
- `New(cfg, sk)` **keeps its existing two-parameter signature** — internally it calls `NewTargets(cfg, "")` (empty stateDir, so no state-file I/O) and delegates to the new `NewWithTargets`. Every existing `New(cfg, sk)` call site (`grep -rn 'proxy\.New(\|New(cfg, sk)'` for the live list; they are in `internal/proxy/proxy_test.go` and `internal/api/replay_test.go`) compiles unchanged.
- New: `NewWithTargets(cfg *config.Config, sk *sink.Sink, t *Targets) (http.Handler, error)` — the real constructor; `New` is now a thin wrapper around it.
- `NewServer(cfg *config.Config, sk *sink.Sink, t *Targets) (*http.Server, error)` (`proxy.go:170-181`) gains the `*Targets` as a new third parameter. The one production caller is `internal/cli/serve.go:120` (`proxySrv, err := proxy.NewServer(cfg, sk)` → `proxy.NewServer(cfg, sk, targets)`).

### `internal/cli/serve.go` wiring

```go
targets, err := proxy.NewTargets(cfg, filepath.Dir(cfg.DBPath))
if err != nil {
	return fmt.Errorf("serve: build provider targets: %w", err)
}
proxySrv, err := proxy.NewServer(cfg, sk, targets)
```
placed immediately before the existing `proxySrv, err := proxy.NewServer(cfg, sk)` call (`serve.go:120`). After `dashAPI := api.New(...)` (`serve.go:130`), alongside the other `dashAPI.SetXxx(...)` seam wiring (`serve.go:135-145`), wire the read/write seam br-GI-25-06 will populate (`dashAPI.SetProvider(api.ProviderSeam{Get: ..., Set: targets.Set})`) — **this bead defines `targets.Set` and the `Targets.Active`/state-read surface that seam needs; wiring the actual `ProviderSeam` struct and the `/api/provider` route is br-GI-25-06's job**, since `api.ProviderSeam` is defined in br-GI-25-06's new `internal/api/provider.go`. If br-GI-25-06 has not yet landed when this bead ships, leave the `dashAPI.SetProvider(...)` call out of `serve.go` (it would not compile against a type that does not exist yet) and note in Review Notes that the wiring line is deferred to br-GI-25-06's own `serve.go` edit — the two beads touch the same function in `serve.go` sequentially, not concurrently, since 06 depends on 05.

### `internal/config` additions

- `Config.LocalUpstreamURL string` and `Config.LocalModelPrefixes []string` (new fields, `config.go:30-72`'s `Config` struct). Both follow the existing precedence (flag > env (`CLENS_*`) > file > default), mirroring the shape of `UpstreamURL` (a flag, `config.go:249`) and `ApiModelPrefixes` (env/file only, no flag — `config.go:131` in `fieldsByEnv`, and no corresponding `applyFlags` registration, since no existing list-typed field has a CLI flag in this file today). Concretely: `fieldsByEnv["CLENS_LOCAL_UPSTREAM_URL"] = "LocalUpstreamURL"`, `fieldsByEnv["CLENS_LOCAL_MODEL_PREFIXES"] = "LocalModelPrefixes"` (new entries beside the existing map, `config.go:117-133`); `applyKV`'s switch (`config.go:195-240`) gains `case "LocalUpstreamURL": cfg.LocalUpstreamURL = val` and `case "LocalModelPrefixes": cfg.LocalModelPrefixes = splitList(val)`; `applyFlags` (`config.go:245-261`) gains `fs.StringVar(&cfg.LocalUpstreamURL, "local-upstream-url", cfg.LocalUpstreamURL, "local (e.g. LM Studio) upstream URL; empty disables the local provider")` — no flag for `LocalModelPrefixes`, matching the `ApiModelPrefixes` precedent.
- `Validate` (`config.go:340-408`) gains, after the existing `UpstreamURL` check (`config.go:363-372`) and before the `SessionGapMinutes` check (`config.go:373-375`): when `c.LocalUpstreamURL != ""`, parse it the same way (`url.Parse`, require `http`/`https` scheme, require a non-empty host), **then** check loopback **without** reusing `validateLoopback` directly — `validateLoopback` expects a `"host:port"` string and calls `net.SplitHostPort`, which errors on a portless host (LM Studio's default URL may omit a port, e.g. `http://localhost`), so write the check inline against `u.Hostname()`:
  ```go
  if !c.AllowRemote && !IsLoopbackHost(u.Hostname()) {
  	return fmt.Errorf("config: validate: LocalUpstreamURL: non-loopback host %q requires AllowRemote", u.Hostname())
  }
  ```
  This mirrors the existing listener guard's *intent* (`validateLoopback`, `config.go:410-422`) without its port-mandatory precondition. The `ApiModelPrefixes` empty-or-whitespace check (`config.go:392-396`) gets a `LocalModelPrefixes` sibling loop, same body, different field.

### `internal/jsonlogs` wiring (the consumer-side classification logic itself was added in br-GI-25-04; this bead only wires the config value in)

`internal/cli/ingest.go`'s `newTailer` (`ingest.go:87-109`) is the one place both `clens ingest` and `addCollectors` build a tailer (`ingest.go:79-86`'s doc comment). Alongside the existing `t.SetModelBilling(resolvedAPIPrefixes(cfg), firstAccount(cfg, "api").Name, "api")` (`ingest.go:102`), add `t.SetLocalPrefixes(cfg.LocalModelPrefixes)` — calling the setter br-GI-25-04 added to `jsonlogs.Tailer`. No `resolvedLocalPrefixes`-style default-substitution helper is needed (unlike `resolvedAPIPrefixes`, `LocalModelPrefixes` has no shipped default — an empty/unset list simply means "no local-prefix classification happens," which is correct until an operator configures LM Studio's model names).

## Rationale

Stamping the provider on the request at `Rewrite` time and carrying it through to the row (via `sink.CapturedCall.Provider`), instead of inferring it later from the model name, is the real design decision here (plan §10, self-review): inference was explicitly rejected because a local model and a cloud model can legitimately share a name, which is exactly the ambiguity invariant 5's billing-mode split must not depend on. The cost of that decision is one field threaded through `captureState` → `CapturedCall` — which already exists from br-GI-25-04 — and the one atomic pointer this bead adds.

## Outcome Definition

`go test ./internal/proxy/ ./internal/config/ ./internal/cli/ -count=1` passes, including the unmodified TTFB gate. `go test ./internal/proxy/ -race -run TestTargetsSwitchIsLiveAndTagged` passes. `go build ./... && go vet ./...` clean. The three pre-flight checks' outcomes are recorded in Review Notes regardless of pass/fail; a failure on check (a) means this bead stops short of the `Targets`/wiring code and reports the gap instead (see Description).

## Test Specifications

- `internal/proxy/targets_test.go`: `TestSetLocalFailsWithNoURL` — `targets.Set("local")` returns a non-nil error when `cfg.LocalUpstreamURL == ""`.
- `internal/proxy/targets_test.go`: `TestProviderBootFallsBackToCloud` — `NewTargets` given a state file whose content is `"local"` and `cfg.LocalUpstreamURL == ""` returns a `Targets` whose `Active() == "cloud"`.
- `internal/proxy/targets_test.go`: `TestProviderPersistsAcrossRestart` — `Set("local")` on one `*Targets` (with a configured local URL and a real `stateDir`), then `NewTargets` again against the same `stateDir` — the second instance's `Active() == "local"`.
- `internal/proxy/targets_test.go`: **new, carried from plan v8's change history ("empty-stateDir no-I/O")** `TestNewTargetsWithEmptyStateDirDoesNoIO` — `NewTargets(cfg, "")` followed by `Set("local")` (with a local URL configured) performs no filesystem write and no read — assert by using a `stateDir` that does not exist and is never created, or by asserting `t.path == ""` directly if the field is accessible from the test (same package) — and that `Set` still updates `Active()` in-memory even though nothing was persisted.
- `internal/proxy/proxy_test.go`: `TestTargetsSwitchIsLiveAndTagged` — run with `-race`: start a server with both cloud and local targets configured (two fake upstreams), fire concurrent requests while concurrently calling `Set` to flip the active target, and assert every captured call's stamped provider equals the fake upstream that actually served it (not a value a racing `Set` call changed to after the request was dispatched but before the response closed).
- `internal/proxy/api_test.go` or `internal/api/replay_test.go`: `TestProviderEndpointRejectsNonLoopbackAndForeignOrigin` — this test's actual route (`POST /api/provider`) belongs to br-GI-25-06; if that route does not exist yet when this bead lands standalone, defer this specific test to br-GI-25-06 and say so in Review Notes.
- `internal/config/config_test.go`: `TestValidateLocalUpstreamLoopback` — `Config.Validate` rejects a non-loopback `LocalUpstreamURL` when `AllowRemote` is false; accepts it when `AllowRemote` is true; accepts a loopback URL (with and without an explicit port) in both cases.
- **Manual / recorded**: the three §7 pre-flight checks (a)/(b)/(c) against the running LM Studio server — recorded in Review Notes per the Description above, not an automated test.

## Files to Touch

- `internal/proxy/targets.go` (create — `Targets`, `NewTargets`, `Active`, `Set`)
- `internal/proxy/targets_test.go` (create — the four `Targets`-level tests above)
- `internal/proxy/proxy.go` (modify — `Rewrite` reads `targets.Active()`, request stamping, `New`/`NewWithTargets`/`NewServer` signatures)
- `internal/proxy/proxy_test.go` (modify — add `TestTargetsSwitchIsLiveAndTagged`)
- `internal/config/config.go` (modify — `LocalUpstreamURL`, `LocalModelPrefixes` fields, env/file/flag wiring, `Validate` loopback check)
- `internal/config/config_test.go` (modify — `TestValidateLocalUpstreamLoopback`)
- `internal/cli/serve.go` (modify — build `Targets`, pass to `NewServer`; defer the `SetProvider` seam wiring line to br-GI-25-06 if that bead has not landed yet)
- `internal/cli/ingest.go` (modify — `newTailer` calls `t.SetLocalPrefixes(cfg.LocalModelPrefixes)`)

## Review Notes

Implemented. `go build ./...`, `go vet ./...` and `go test ./...` green; the TTFB gate is unchanged and green. `TestTargetsSwitchIsLiveAndTagged` passes under `-race`; negative control (Rewrite re-reading `Active()` instead of using the request's stamp) fails it with rows stamped for the wrong upstream.

**Pre-flight (plan section 7), run against LM Studio at 192.168.0.185:1234 before any bead was written: all three passed.** (a) The response body carries a `msg_...` message id, so the cross-source dedup key exists and a local call merges into one row. (b) The stream carries usage frames in `message_start` and `message_delta`, the shape `parse.ExtractUsage` reads. (c) A Claude model id is accepted unchanged (200); the response `model` field reports the local model's own name, which is what `ModelResolved` keeps.

Design notes. The provider is read once, in the handler, and carried on `captureState`; `Rewrite` resolves the upstream from that stamp rather than calling `Active()` again, so the serving upstream and the row's `Provider` cannot disagree. `Targets.Set` writes the temp file and renames it before moving the pointer, so a failed write leaves both unchanged. The state file is `<dir of DBPath>/provider`. `LocalUpstreamURL` is checked with `IsLoopbackHost(u.Hostname())` so a portless URL validates; the LAN LM Studio address needs `AllowRemote = true`, which also widens the listener checks, so an operator who wants only the local upstream remote has no narrower switch. Left as is: relaxing the rule is a spec change.

Deferred to br-GI-25-06: the `dashAPI.SetProvider(...)` line in `serve.go` (its type does not exist yet) and `TestProviderEndpointRejectsNonLoopbackAndForeignOrigin` (needs the route). Tests added beyond the bead: `TestLocalKeysParseFromFileEnvAndFlag`, a reject-unknown-provider case, and a no-leftover-temp-file check.
