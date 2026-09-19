# Claude Lens — Context Docs

Claude Lens is a local observability proxy for Claude traffic: one Go binary, `clens`, that sits in
front of the Anthropic endpoint and records what was sent, what came back, what it cost, and which
request parameters the API silently dropped. Unlike a single-source proxy it reconciles **four
sources** — its own capture, Claude Code's JSONL transcripts, the claude.ai usage endpoint, and the
Admin API usage/cost reports — so it can report usage and cost for every subscription tier *and*
pay-as-you-go API-key billing from one install. It is a single-user developer tool: loopback-only by
default, no hosted service, no accounts to create.

## Stack inventory

| Unit | Language/Runtime | Framework | Role | Evidence |
|---|---|---|---|---|
| `clens` | Go 1.24.1 | stdlib `net/http` only | the entire product: proxy listener (`127.0.0.1:8797`), dashboard listener (`127.0.0.1:8798`), collector goroutines, SQLite store, CLI | [go.mod](../../go.mod), [cmd/clens/main.go](../../cmd/clens/main.go) |
| dashboard assets | HTML / CSS / vanilla JS | none — `go:embed`, no build step | the 10-tab UI served by the dashboard listener | [internal/web/embed.go](../../internal/web/embed.go) |
| repo governance | YAML | GitHub Actions | two PR/push guards; **no test job** | [.github/workflows/](../../.github/workflows/) |

Three non-stdlib modules and no more: `modernc.org/sqlite` (pure Go, no cgo),
`klauspost/compress` (zstd), `andybalholm/brotli`.

## Modules

| File | When to open it | Why it's here |
|---|---|---|
| [architecture.md](architecture.md) | first — the process shape, the components, and the rules a change must keep | **core** |
| [conventions.md](conventions.md) | before writing code: naming, layering, error handling, DI, testing | **core** |
| [build-and-run.md](build-and-run.md) | to build, test, run, or find a config knob | **core** |
| [glossary.md](glossary.md) | when a term in the code or the UI is unfamiliar | **core** |
| [storage-schema.md](storage-schema.md) | before touching a column, a query, or an invariant | conditional — fills the `data-model` role; trigger: `internal/store/schema.sql` |
| [cost-and-quota.md](cost-and-quota.md) | before touching pricing, quota, reconciliation, or a cost rule | conditional — a domain module (not in the standard catalogue): the cost model is upstream of every feature |
| [api-surface.md](api-surface.md) | before adding or changing a route | conditional — trigger: route registration in `internal/api` |
| [cli-and-tooling.md](cli-and-tooling.md) | before adding or changing a subcommand | conditional — trigger: the 18-entry dispatch table in `cmd/clens/main.go` |
| [workflows.md](workflows.md) | to understand a flow end to end before changing it | conditional — trigger: flows spanning more than one package |
| [security-and-permissions.md](security-and-permissions.md) | **before touching credentials, redaction, listeners, or an import edge** | conditional — trigger: `internal/secret`, the redactor, the Origin guard |
| [data-privacy-and-compliance.md](data-privacy-and-compliance.md) | before changing what is captured, or how long it is kept | conditional — trigger: the body policy, the cap, retention, `clens purge` |
| [testing-and-quality.md](testing-and-quality.md) | before writing a test, or wondering what CI gates on | conditional — trigger: 50 test files |
| [infra-and-deploy.md](infra-and-deploy.md) | before touching a workflow, a hook, or branch policy | conditional — trigger: `.github/workflows/` |
| [integrations-and-external-services.md](integrations-and-external-services.md) | before changing a collector or adding a dependency | conditional — trigger: four external endpoints, three Go modules |
| [dashboard.md](dashboard.md) | before changing anything in `internal/web` | conditional — a non-catalogue module: 838 lines of hand-written JS under a hard no-build-step rule |
| [decisions/](decisions/000-index.md) | before "simplifying" something that looks over-built | conditional — six genuine forks, each with a rejected alternative a change could reintroduce |

## Grounding rules for agents

These docs are a map, not the territory — and **the code wins on conflict**.

1. **Discover, don't assume.** Every claim here cites a file (and often a symbol or line). Follow the
   link before planning against it. Five statements are marked as not established by cited code —
   two `⚠️ ASSUMPTION` (about intent) and three `❓ UNVERIFIED` (facts needing evidence), each in
   the module that owns it and each naming what would confirm it. Grep for the markers to find them.
2. **Verify the slice you are about to touch.** Treat a claim about the exact file, endpoint, schema
   shape, or permission you are about to change as a hypothesis until you have opened the source.
   Broad context you are not acting on can be taken at face value.
3. **Mark what is not known.** Statements that could not be verified from the repo are marked
   `> ⚠️ ASSUMPTION:` (a belief about intent) or `> ❓ UNVERIFIED:` (a fact that needs evidence),
   each naming what would confirm it. The most consequential one: **the source C and source D
   endpoint URLs and response shapes are unverified by design** — see
   [integrations-and-external-services.md](integrations-and-external-services.md).
4. **A doc is not an invariant.** The enforced invariants live in `CLAUDE.md` §Architecture
   essentials and in tests. If a doc and a test disagree, the test is right and the doc is a bug —
   fix the doc.
5. **Formatting.** Relative file links, tables over prose, the codebase's own identifiers. Never
   restate a fact that already lives in exactly one other file — link it.

## Last generated / refreshed

**2026-09-19 — FRESH (first full generation), then refresh-verified.** Scope: the whole repository
at `GI-1-claude-lens-v1` (v1 complete; beads `br-GI-1-01` … `-19`). Three earlier hand-written
context docs existed without an index — `architecture.md`, `cost-and-quota.md`,
`storage-schema.md` — and were folded into this structure rather than replaced; every one of their
factual claims was re-checked against the code. One was wrong and is corrected here:
`storage-schema.md` said "nine tables" and listed ten. The module plan above was produced by the
skill's stack-detection pass, and each conditional module names the signal that earned it a place.

**2026-09-19 — REFRESH, scoped to `GI-3-deepseek-peak-pricing`** (beads `br-GI-3-01` … `-10`; plan
`docs/planning/GI-3-deepseek-peak-pricing.md` v9). **No module was added or retired** — the stack,
the module set, and the decision count are unchanged, so the plan above stands; `decisions/` gained
no ADR because the merge question is an application of 001, not a new fork, so a bullet went there.
**Ten module files changed — this index makes eleven — and the rest came back *no changes
needed*:** `cost-and-quota.md` (peak and
off-peak, exact money rounded per token class, the third-party zero-write rule, the
cache-minimum exception list), `storage-schema.md` (23 kinds; the merge that moves `billing_mode`
with the cost columns), `glossary.md` (five `nonAnalyzeKinds`; peak window, off-peak dates,
third-party prefix), `build-and-run.md` (two file-and-env-only config keys, and the CRLF caveat
under `gofmt`), `workflows.md` §2 (the merge's billing rules), `cli-and-tooling.md` (`--rebuild`
is the re-pricing path), `conventions.md` (the seam-interfaces paragraph, which claimed `api.Store`
was the codebase's one interface — there are 26, and GI#3 added three), `decisions/001` (the merge
as the one place the pair can desync), `architecture.md` (its pricing row named a `Cost()` symbol
that does not exist — the entry point is `Compute()`), and `testing-and-quality.md` (the size
figures, re-measured).
