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
| [cli-and-tooling.md](cli-and-tooling.md) | before adding or changing a subcommand | conditional — trigger: the 21-entry dispatch table in `cmd/clens/main.go` |
| [workflows.md](workflows.md) | to understand a flow end to end before changing it | conditional — trigger: flows spanning more than one package |
| [security-and-permissions.md](security-and-permissions.md) | **before touching credentials, redaction, listeners, or an import edge** | conditional — trigger: `internal/secret`, the redactor, the Origin guard |
| [data-privacy-and-compliance.md](data-privacy-and-compliance.md) | before changing what is captured, or how long it is kept | conditional — trigger: the body policy, the cap, retention, `clens purge` |
| [testing-and-quality.md](testing-and-quality.md) | before writing a test, or wondering what CI gates on | conditional — trigger: 56 test files |
| [infra-and-deploy.md](infra-and-deploy.md) | before touching a workflow, a hook, or branch policy | conditional — trigger: `.github/workflows/` |
| [integrations-and-external-services.md](integrations-and-external-services.md) | before changing a collector or adding a dependency | conditional — trigger: four external endpoints, three Go modules |
| [dashboard.md](dashboard.md) | before changing anything in `internal/web` | conditional — a non-catalogue module: 1142 lines of hand-written JS under a hard no-build-step rule |
| [decisions/](decisions/000-index.md) | before "simplifying" something that looks over-built | conditional — nine genuine forks, each with a rejected alternative a change could reintroduce |

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

**2026-09-19 — REFRESH, scoped to `GI-5-call-detail-drilldown`** (beads `br-GI-5-01` … `-03`; plan
`docs/planning/GI-5-call-detail-drilldown.md` v6.1). **No module was added or retired** — no new
dependency, route, entity, or build step, so the plan above stands and `decisions/` gained no ADR —
the drill-down's design record is the plan's own D1–D10.
**Three module files changed — this index makes four:** `dashboard.md` (the two-mode Calls and
Sessions pattern, the `reveal`/`show` split, the `detailSeq` generation token, the corrected route
rows and asset line counts), `decisions/006-dashboard-with-no-build-step.md` (its list of
`TestAssets*` guards read as a closed set of three; GI#5 added three more, and the list now defers to
`dashboard.md`'s table), and `testing-and-quality.md` (the size figures, re-measured). Everything
else came back *no changes needed*.

**One claim was caught by this refresh rather than by the story's own review**, and it is worth
naming because of how it got there: `dashboard.md`'s `detailSeq` paragraph — hand-written during
GI#5 — said the token is compared "after every `await`". It is not. In the `[data-call]` catch,
`setStatus` runs after `await show('calls')` with no re-check, and cannot be re-checked with the
same token, because `show` bumps the generation itself. The plan's D10 already records that ceiling;
the doc did not, and an overclaim in a doc whose whole job is telling an agent which guard is
load-bearing is exactly the failure this tree exists to prevent. Corrected in place.

**2026-09-20 — REFRESH, scoped to `GI-7-header-and-body-visibility`** (beads `br-GI-7-01` … `-09`;
plan `docs/planning/GI-7-header-and-body-visibility.md` v8). **No module was added or retired.**
`decisions/` gained its **first ADR since the initial generation** — [007](decisions/007-schema-migrations-by-user-version.md),
`PRAGMA user_version` migrations — which is why the decision count above moved six → seven and the
`decisions/` hint changed. 007 is also the first record here whose status is *supersedes a GI-1
decision* rather than a standalone fork, so [000](decisions/000-index.md) now says which kind it is.

**Twelve module files changed and one was created — this index makes fourteen:**
`storage-schema.md` (a new *How the schema gets applied* section; the `events` row now names the
transcript columns and 47 total), `api-surface.md` (the `/api/mode` route; the list projection and
why it is a *type*; the three detail fields and `Completeness`'s integer spellings; and every
`api.go` line reference re-pointed — the route block moved down 18 lines, which no diff of *this*
file would ever show), `dashboard.md`
(the badge, the body/header renderers, the re-measured line counts, and a warning about `funcBody`'s
CRLF assumption), `glossary.md` (`EventSummary`/`Event`, `Completeness`, the transcript columns, the
cap, the badge), `workflows.md` §1 and §2 (`CaptureComplete` covers both teed buffers; the merge's
new preference), `data-privacy-and-compliance.md`, `security-and-permissions.md` (a new *Untrusted
rendering* section), `cli-and-tooling.md` (`show`'s capture and read-path markers), `architecture.md`
and `decisions/003` (both carried the "no migrations" claim), `testing-and-quality.md` (five
invariant rows and the size figures, re-measured — this story took the test-file count 50 → 51),
`decisions/000-index.md`, and the new `decisions/007-…md`. **Everything else came
back *no changes needed*** — `conventions.md`, `build-and-run.md`, `infra-and-deploy.md`,
`integrations-and-external-services.md`, and `decisions/001`, `-002`, `-004`, `-005`, `-006`.
`cost-and-quota.md` came back unchanged at this refresh and changed in the follow-up below.

**This refresh found a control that silently does not reach a path it appears to cover** —
`--body-policy`/`--body-cap-bytes` governed what the *proxy* kept (`BodyPolicy` had exactly one call
site) while `internal/jsonlogs` never consulted them, so the story's own new
`transcript_content`/`transcript_role` columns were stored whole and uncapped even under
`--body-policy off`. Neither the plan nor `br-GI-7-06` mentioned the policy, so the intent was not
recoverable and the finding was recorded as `❓ UNVERIFIED` rather than asserted either way.

**That finding was then escalated and fixed, and the fix found a bigger defect underneath it**
(`br-GI-7-09`, committed after this refresh ran — the paragraph above describes the tree as this
refresh left it). Eleven of the module files were updated again for the fix, and **two joined the
list that had come back clean**: `cost-and-quota.md` (`source_mismatch` now needs two *measurements*,
which is what its own contract always said) and `build-and-run.md` (the flag table, where the body
policy's real behaviour belongs). `api-surface.md`,
`decisions/000-index.md` and `decisions/007` were unaffected by the fix. Answering *"does the
policy reach source B?"* required answering *"what does the policy mean?"*, and `off` turned out to
mean **no rows at all**: the proxy returned the bare `ReverseProxy` before the closure that installs
capture state existed, so an operator choosing the most private setting silently lost the records —
while `clens serve`'s banner promised "calls are recorded without their bodies", and `printBanner`'s
own doc comment disagreed with the string beneath it. The code now matches the words, in both
sources. The bead also fixed a knock-on it created: a bodyless row is `CaptureComplete` true with
every token column zero, which made the merge both *prefer* it over a row with real counts and raise
`source_mismatch` at `SeverityError` claiming a disagreement that never happened.

**One more value went the same way, after the fix above.** `--body-policy` had a third spelling,
`truncated`, accepted by `Validate` and read by no code path — so `full` and `truncated` ran
byte-identical code, and the value that reads as *narrow it* stored the body whole. It is now
rejected at startup with a message naming `full` and `off`, rather than aliased to `full`.
`data-privacy-and-compliance.md`, `build-and-run.md` and `decisions/003` moved with it; `README.md`
is outside the generated tree. See `br-GI-7-09`'s decision log for why rejection beat aliasing.

The class is the same one `CaptureComplete` belonged to, and it appeared four times in one story: a
control that looks like it covers a path and does not. `CaptureComplete` was derived from one buffer
of two (fixed in `br-GI-7-08`, found by the manual run); the body policy reached one source of two
(fixed in `br-GI-7-09`, found by this refresh); its third value reached nothing at all (fixed in the
same bead); and none was visible to any test — the first because every fixture truncated a response,
the second because the one test that read the banner checked its text and not its claim, the third
because no test asserted the *accepted* set, only a rejected one.

**2026-09-21 — REFRESH, scoped to `GI-9-merge-jsonl-and-proxy-rows`** (beads `br-GI-9-01` … `-07`;
plan `docs/planning/GI-9-merge-jsonl-and-proxy-rows.md` v23). **No module was added or retired** — no
new dependency, route, entity, or build step. `decisions/` gained **two ADRs** — [008](decisions/008-three-tier-identity-key.md)
(the three-tier identity key) and [009](decisions/009-proxy-adopts-the-conversation-id.md) (the proxy
adopts the conversation id) — so the count above moved seven → nine and the `decisions/` hint changed.

**The story changed nine files under `docs/context/` — seven modified, two created** — and this
refresh then changed two more, for eleven. The story's seven: `INDEX.md` (19 subcommands, nine forks,
52 test files), `cli-and-tooling.md` (the `rekey` row; "the one destructive command" → two, plus
`rekey`'s pass-3 precondition), `storage-schema.md` and `data-privacy-and-compliance.md` (each
carrying its own copy of that claim — the second also the `--dry-run --yes` misstatement and its
citation, and the no-new-capture statement), `workflows.md` §2 (the merge now fires; the session
rule), `testing-and-quality.md` (the size figures), and `decisions/000-index.md`. **This refresh's
two: `testing-and-quality.md` again, and `glossary.md`** — see below. **Everything else came back
*no changes needed*** — `architecture.md`, `api-surface.md`, `conventions.md`, `build-and-run.md`,
`dashboard.md`, `cost-and-quota.md`, `security-and-permissions.md`, `infra-and-deploy.md`,
`integrations-and-external-services.md`, and `decisions/001`–`007`.

**This refresh found a size figure stale by exactly one commit.** `testing-and-quality.md` said
"52 test files, **16,544** lines against **15,498** lines of non-test Go"; both numbers were
measured and committed by `br-GI-9-05` (`aab22ad`), and the implementation cross-review's fix commit
`b871c7c` then added 28 lines to `internal/store/merge.go` and a net 90 to
`internal/cli/rekey_test.go`. The doc was 90 and 28 short of the branch tip. Re-measured: **16,634**
and **15,526**. The file count and its provenance (51 → 52, one new file, `rekey_test.go`) were
correct and stay. The clause now states that a mid-branch count is a count *of that commit*, not of
the story — the form this drift keeps taking.

**`glossary.md` carried two definitions the story had made incomplete.** `session` was still "a fold
of the events that belong to it", when D7 changed the id to the conversation the request already
carries and left the gap-window heuristic as the header-less fallback only; `request_id` named the
key without the three tiers that now resolve it. Both now state the rule and link 008/009. The
glossary is where an agent looks first for a term, so a definition predating the rule is worse there
than in any other module.

**A scope note for the next refresh on this machine:** the range for a story is `origin/main..HEAD`,
not `main..HEAD`. A local `main` that lags the remote silently folds the *previous* story into the
diff — here, all of GI-7's files appeared under GI-9's range until it was re-scoped.
