[← INDEX](INDEX.md)

# Conventions

Only conventions actually observed in this codebase. The **process** conventions (ticket prefix,
branch shape, commit subject, PR body) are enforced and are specified in
[CLAUDE.md](../../CLAUDE.md) §Conventions — not repeated here. This file covers the code.

## Naming & layout

| Thing | Convention | Evidence |
|---|---|---|
| Package docs | **Every** package has a `// Package x …` doc comment stating its responsibility and its containment rules. 23 of 23 non-test packages. | [internal/api/api.go:1](../../internal/api/api.go#L1) is the model |
| Directories | One `internal/<concern>/` per package, flat — no nesting below `internal/` | [internal/](../../internal/) |
| Files | One concern per file; a package's files are named for their role (`rules.go`, `kinds.go`, `merge.go`, `calibration.go`) | — |
| Comment refs to work items | Comments name the bead that owns the code: `br-GI-1-16`, `br-GI-1-18` | [cmd/clens/main.go:14](../../cmd/clens/main.go#L14) |
| Deliberate ceilings | Marked `ponytail:` with the ceiling and the upgrade path | `internal/quota` projection, `internal/analyze/rules.go` prefix estimator |
| Test files | `<file>_test.go` beside the code; integration-ish tests named for the invariant, not the function (`TestNoBufferingSSE`, `TestBillingModeInvariants`) | [internal/proxy/proxy_test.go:40](../../internal/proxy/proxy_test.go#L40) |

## Layering

The import graph is a set of rules, and each is asserted by a test rather than trusted to review.

| Rule | Asserted by |
|---|---|
| `internal/proxy` imports only `sink` and `config` | [internal/proxy/importguard_test.go](../../internal/proxy/importguard_test.go) |
| `internal/api` and `internal/web` never import `secret` (any file), `config`/`ingest` (non-test files) | [internal/api/importguard_test.go](../../internal/api/importguard_test.go) |
| The same `config`/`ingest` ban, from the composition root's side | [internal/cli/serve_test.go](../../internal/cli/serve_test.go) |

The config/ingest half is deliberately asserted in **two** places; the overlap and its justification
are documented at [internal/api/importguard_test.go:25](../../internal/api/importguard_test.go#L25).

**When the guards fire, the fix is a seam, not an import.** A route that needs `secret` gets
`SetCredentialWriter`; the composition root does the importing.

## Error handling

- Wrap with `fmt.Errorf("...: %w", err)`. The dominant style by a wide margin (189 `%w` sites vs
  63 non-wrapping in `internal/`).
- Export a sentinel when a caller must branch on the *condition*: `secret.ErrUnset` means "no
  credential configured", and is distinct from an empty string on purpose.
- **A missing value is not a zero value.** `pricing.Cost` returns `(nil, "unpriced")` rather than a
  numeric zero a caller could store as `$0.00`; `quota.Projection.Configured` is false rather than
  `UtilizationPct` reading as a real 0%. See [cost-and-quota.md](cost-and-quota.md).
- Fail open at the proxy boundary: a capture failure is logged, never returned to the client.

## Dependency injection / composition

**Function-value seams, not interfaces, not a DI framework.** The dashboard's dependencies on
packages it may not import are settable fields on the API type (`SetPricing`,
`SetCredentialWriter`, `SetAccountWriter`, `SetIngestTrigger`, `SetSourceHealth`, `SetAccounts`),
all assigned in one place: the composition root in
[internal/cli/serve.go](../../internal/cli/serve.go).

Two rules that come with the pattern:

- **An unset seam is a supported state.** The consuming route answers `503` with a reason. It is
  never a nil dereference and never a silently empty result.
- **Seams are declared in the bead that owns the *setter*, not the route.** That is what let the
  write seams ship a bead before their routes existed, with no dependency edge between the two.

The one interface in the codebase is `api.Store` — a narrow read slice of `*store.Store` — kept so
tests can seed a real temp store. It is not extensibility.

## Logging & observability

- The tool observes itself: `clens` writes its own findings into the database as `warnings` rows
  rather than to stderr, so the dashboard can show them. Kinds are declared once in
  [internal/analyze/kinds.go](../../internal/analyze/kinds.go).
- Collector failures are recorded against the owning source row in `ingest_state`, surfaced by
  `GET /api/sources` — a failed collector is visible, not a quietly short chart.
- Counters that would otherwise be invisible are exposed on `GET /api/health` — e.g.
  `replayRejected`, which counts replays turned away by the opt-in or Origin guard, because a
  rejected probe against the one billable route would otherwise leave no server-side trace.

## Testing conventions

- Table-driven tests with named cases are the default for a rule or a classifier.
- **The test names the invariant, not the function.** `TestNoBufferingSSE`,
  `TestDerivedPromptTotal`, `TestBillingModeInvariants`, `TestBrokerDropsSlowSubscriberRatherThanBlocking`.
- A test that passes with the bug present is treated as not having tested anything — several rules
  were re-fixtured after a mutation check showed the original fixture varied the *outcome* rather
  than the input the rule reads. See [testing-and-quality.md](testing-and-quality.md).
- Cross-package consistency is pinned by a test that reads the other artifact:
  `internal/analyze/readme_test.go` parses the README's warning-kind table and fails if it diverges
  from `AllKinds()`.

## Formatting / lint

`gofmt` only — **there is no linter config in the repo** (no `.golangci.yml`, no `Makefile`). The
gate is the test suite plus `go vet`:

```
gofmt -l .        # must print nothing
go vet ./...
go test ./...
```

CI does **not** run any of these; it runs the two governance guards only. Tests are the
developer's gate, not a CI gate — see [infra-and-deploy.md](infra-and-deploy.md).
