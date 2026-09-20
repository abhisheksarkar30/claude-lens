[← INDEX](INDEX.md)

# Testing & Quality

## Test frameworks in use

| Layer | Framework | Location | Evidence |
|---|---|---|---|
| Unit + integration | Go's stdlib `testing` only — no assertion library, no mocking framework | `*_test.go` beside the code | [go.mod](../../go.mod) has no test dependency |
| End-to-end | none — no suite; one recorded manual run instead | [docs/acceptance.md](../acceptance.md) | the doc states which half could not be run |

**51 test files, 14,563 lines** against 14,655 lines of non-test Go (re-measured at
`GI-7-header-and-body-visibility`, which took the count 50 → 51 on one new file —
[internal/api/mode_test.go](../../internal/api/mode_test.go); everything else it added went into
existing test files). Real components are used
rather than mocked: tests open a real temp SQLite store, run a real `httptest.Server` upstream, and
drive the real `ServeMux`.

## What's covered

Named for the invariant, not the function — that convention is documented in
[conventions.md](conventions.md).

| Invariant | Test | Where |
|---|---|---|
| **The hot path never buffers the stream** | `TestNoBufferingSSE` — a fake upstream streaming SSE slowly, asserting the client sees its first event *before* upstream sends its last | [internal/proxy/proxy_test.go:40](../../internal/proxy/proxy_test.go#L40) |
| Bytes pass through unchanged | `TestByteIdentityNonStreaming` | [internal/proxy/proxy_test.go:95](../../internal/proxy/proxy_test.go#L95) |
| A broken observer never breaks the session | `TestFailOpenOnUpstreamFailure` | [internal/proxy/proxy_test.go:139](../../internal/proxy/proxy_test.go#L139) |
| Credentials are redacted before the tee | redact tests | [internal/proxy/redact_test.go](../../internal/proxy/redact_test.go) |
| **The two billing models are never summed** | `TestBillingModeInvariants`, `TestSessionCostSplit` | [internal/store/store_test.go:179](../../internal/store/store_test.go#L179) |
| **`input_tokens` is the uncached remainder** | `TestDerivedPromptTotal` | [internal/store/store_test.go:151](../../internal/store/store_test.go#L151) |
| The merge is idempotent and re-derives the session | [internal/store/merge_test.go](../../internal/store/merge_test.go) | |
| A slow SSE subscriber is dropped, not blocking | `TestBrokerDropsSlowSubscriberRatherThanBlocking` | [internal/api/broker_test.go:41](../../internal/api/broker_test.go#L41) |
| Writes are idempotent (upsert) | `TestAdminUpsertIdempotent`, `TestIngestStateUpsert`, `TestWarningUpsertIdempotent` | [internal/store/store_test.go](../../internal/store/store_test.go) |
| Readers proceed during a write batch | `TestConcurrentReadersDuringWriteBatch` | [internal/store/store_test.go:623](../../internal/store/store_test.go#L623) |
| Every warning kind has one spelling, and the README agrees | `internal/analyze/readme_test.go` parses the README's table and compares it to `AllKinds()` | [internal/analyze/readme_test.go](../../internal/analyze/readme_test.go) |
| Every shipped model has a minimum-cacheable-prefix entry | `TestMinimumCacheablePrefixCoversShippedModels` | [internal/analyze/analyze_test.go](../../internal/analyze/analyze_test.go) |
| Containment import rules | three guards — see [security-and-permissions.md](security-and-permissions.md) | `internal/{api,proxy}/importguard_test.go`, `internal/cli/serve_test.go` |
| **The list routes carry no body columns** | `TestListRouteOmitsBodies`, `TestSessionRouteOmitsBodies` — both share `assertNoBodyColumns`, which names all six omitted keys *and* bounds the response at 64 KB, against a 50-row fixture storing 1 MB per blob. The size bound is what makes it non-vacuous | [internal/api/api_test.go](../../internal/api/api_test.go) |
| **`CaptureComplete` covers both bodies** | `TestCaptureCompleteCoversBothBodies` — request over the cap, request exactly at it, request under it, response over it, and both over it | [internal/proxy/proxy_test.go](../../internal/proxy/proxy_test.go) |
| The migration runner's four paths | `TestMigrateFreshDatabase`, `TestMigrateExistingDatabase`, `TestMigrateHealsAPartialDatabase`, `TestMigrateDoesNotReAddColumnsOnAPartialNewSchema` — see [decisions/007](decisions/007-schema-migrations-by-user-version.md) | [internal/store/store_test.go](../../internal/store/store_test.go) |
| The browser's completeness integers | `TestDetailPinsCompletenessWireValue` — all four spellings on the wire, not the typed constants every other test compares | [internal/api/api_test.go](../../internal/api/api_test.go) |
| The body renderer escapes, and its markers select in order | `TestAssetsTheBodyRendererEscapes` — source-shape only; its ceiling is stated in the test | [internal/web/assets_test.go](../../internal/web/assets_test.go) |

### The one test that is a design gate

`TestNoBufferingSSE` is not a correctness test. A proxy that buffered the whole stream would still
return the **correct bytes** — just late — so no other test in the suite would catch the regression.
It exists to keep a property that is invisible to every other check. Treat a change that requires
touching it as a design change, not a test fix.

## Known gaps

- **No E2E suite.** The closest artifact is [docs/acceptance.md](../acceptance.md), a manual run
  recorded as executed. Its live half — a billed call, a quota snapshot, an Admin cost row, and the
  `request-id` ↔ `requestId` capture — was **not run**, because it needs a real Anthropic credential.
- **No coverage measurement** is configured; no coverage threshold is enforced.
- ❓ UNVERIFIED: the Windows ACL path's real-world behaviour on a non-English Windows install, where
  the principal name returned by `os/user.Current()` may not match what `icacls` accepts. The tests
  stub `runICACLS` and `currentPrincipal` (both are package-level `var`s for exactly this reason),
  so the real `icacls` invocation is exercised only by the acceptance run on the developer's own
  machine.
- **The `-race` set is narrow** — `./internal/api/... ./internal/web/...`, the concurrent pair. The
  consumer and the collectors are not run under `-race` in the documented command.

## CI gates

**There are none for tests.** `.github/workflows/` contains two governance guards and no test job:

| Workflow | Gates on | Runs tests? |
|---|---|---|
| [branch-guard.yml](../../.github/workflows/branch-guard.yml) | PR shape: branch name, `GI#<n>` title, closing keyword, per-commit prefix | no |
| [main-guard.yml](../../.github/workflows/main-guard.yml) | that a commit reaching `main` came from a merged story-branch PR | no |

**Tests are not a CI gate** — this is stated plainly in [CLAUDE.md](../../CLAUDE.md) §Enforcement.
The developer's gate is `go build ./... && go vet ./... && go test ./...`, run locally, plus
`.githooks/pre-commit` for secrets. See [infra-and-deploy.md](infra-and-deploy.md).

## A practice worth keeping

**Mutation-verify a rule test.** Several analyzer rules had fixtures that varied the *outcome*
rather than the *input the rule reads*, so the test passed even with the rule's logic inverted.
The remedy applied during this repo's implementation cross-review: apply the inverse of the fix,
confirm the specific test **fails**, restore, confirm it passes. `cache_prefix_below_minimum`'s
fixture was rebuilt this way — it now holds the body and usage fixed and varies only the model,
which is the input the per-model minimum table actually reads.
