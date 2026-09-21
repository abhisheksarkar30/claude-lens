# Bead br-GI-9-07: the session is the conversation the request already names — `x-claude-code-session-id` reaches the id, and the JSONL tailer records

**Plan Reference**: `docs/planning/GI-9-merge-jsonl-and-proxy-rows.md` — §3 D7 (and D3, D4's ordering),
§4 (the `meta.go`, `meta_test.go`, `types.go` ×2, `session.go`, `session_test.go`, `rules.go`,
`rules_test.go`, `ingest.go`, `refresh.go`, `serve.go`, `cli_test.go` rows), §5 (`internal/parse`,
`internal/session`, `internal/analyze`, `internal/cli` tailer wiring), §6, §9 bead 08 (which §9 says
has no bead yet and needs one)

- **Bead ID**: br-GI-9-07
- **Priority**: P0 (critical)
- **Original Estimate**: 5h
- **Dependencies**: br-GI-9-01 (the `Usage.MessageID` field the identity rule keys on)
- **Blocks**: br-GI-9-04, br-GI-9-05

> **This bead is large because it is one decision.** D7 is a single rule — the proxy adopts the
> conversation id the request already carries — and its four changes are consequences of it, not
> options. Splitting it would ship a state where `ExtractMeta` reads the header but `Resolve` still
> mints `s_…` (the header then changes nothing), or the reverse (an id nothing supplies). **It must
> land before br-GI-9-04 runs**: pass 2 assumes this forward rule, and pass 3's re-ingest writes ids the
> old resolver would mint differently.

## Description

The proxy already receives Claude Code's real conversation id — `x-claude-code-session-id`, present in
**1,364** of ~1,400 stored request headers, **3** distinct values, all 3 real JSONL `sessionId`s
(§2.6) — and stores it verbatim in every row's `req_headers`. Reading it is not sufficient on its own:
one side already carries that exact value and the other never does.

- **JSONL rows already carry the real conversation id**: `buildEvent` sets `session_id` from the line's
  `sessionId` — the parent's for a sidechain, the filename stem as fallback
  (`internal/jsonlogs/jsonlogs.go`).
- **Proxy rows always carry a minted `s_<unixMilli>_<hex>`**: `newSessionID` is returned
  **unconditionally** by `Resolve`. The header selects only a `groupKey`; it never becomes the id.

Four changes, each a **consequence** of that gap or of the change above it:

### 1. `ExtractMeta` gains `x-claude-code-session-id` as a second source for `Meta.SessionHeader`

`internal/parse/meta.go`. **`x-clens-session` still wins** — it is the explicit operator override, so
it is read first and the new header only fills the value **when it is absent**. The comment block is
updated: it currently documents only `x-clens-session` and the `maxSessionHeaderLen` reasoning, and
both must now cover the second source — **the length bound applies to whichever header supplies the
value**, and an overlong `x-clens-session` does not supply one, so the second header's value is used.

### 2. `Resolve` returns `meta.SessionHeader` as the id when non-empty

`internal/session/session.go`. Placed **at the top**, ahead of the `seen`-map logic. A header-carrying
call's identity **is** the header; the inactivity-gap window is for header-less calls only. No code or
doc assumes the `s_` prefix (checked), so nothing downstream has to learn a second id shape. The
residual **~3%** of requests without the header fall back to today's gap-window behaviour, and
`Resolver.Resolve` keeps its shape.

### 3. The JSONL tailer is given a `SessionRecorder`

Today **no caller anywhere in the module wires one** — `SetSessionRecorder`
(`internal/jsonlogs/jsonlogs.go:143`) is defined and never called — so `t.recorder` stays nil and the
JSONL half silently records no `sessions` rows. **That, not a design choice, is the real reason behind
§2.6's "0 of 207".** The wiring goes where the tailer is built — `newTailer`
(`internal/cli/ingest.go`) — the one shape both `runIngest` and `addCollectors` (reached from `serve`
and `refresh`) use, so the three callers cannot drift. Because the `collectorStore` surface `newTailer`
takes does not carry `UpsertSession`/`ReconcileSession`, the recorder is **passed in**, not constructed
inside `newTailer`:

- `runIngest` **must build one** — the parameter cannot come from `st`, but `runIngest` already holds
  the `*store.Store`, so it constructs `session.New(st, …)` and passes it to `newTailer`.
- `addCollectors` gains the resolver as a parameter; `serve` already builds the session
  (`serve.go:89`), so its `addCollectors` call passes it through.
- **`runRefresh` must build one** — there is no `session.New` in `refresh.go`, so without a new one
  the tailer's `SetSessionRecorder` cannot be wired from this path.

### 4. `ExtractMeta` stops nil-ing `PrefixHash` when a header is present

`internal/parse/meta.go` currently sets `m.PrefixHash = nil` whenever `SessionHeader != ""` — an
optimisation, on the belief "the hash is not needed", which D7 falsifies: the two session rules that
key on it (`ruleCacheExpiredBetweenTurns`, `rules.go:416`; `ruleCacheConcurrentWriteRace`,
`rules.go:444`) `continue` on a nil hash, and D7 makes `SessionHeader` non-empty for the **~97%** of
proxy calls that carry a header, while the residual **~3%** fall back to the `else` branch's hash —
so **the nil-ing silently kills the two rules for the majority of calls**. The fix is to **always
compute the hash**: `groupKey` checks `SessionHeader` **first**, so grouping is unchanged, and
`RecordCall` then stores a real `prefix_hash` on the session row — an improvement, not a regression.
The two comments that call the NULL-ness load-bearing (`internal/parse/types.go`, 
`internal/store/types.go`) are corrected to name what actually is: **the header wins in `groupKey`**,
not the nil. `TestExtractMetaPrefixHashNilWhenSessionHeaderPresent` pins nil-for-header, so its
**invariant moves** rather than being deleted — it asserts a **computed** hash with a header present,
and the "header wins" invariant it was really protecting already lives in `session_test.go`'s
`TestResolveHeaderOverridesPrefix`.

### Only with (2) does the rest follow

`ExtractMeta` alone takes 184 heuristic sessions to ~3 and stops the gap-splitting, **but those 3 stay
`s_…` and still never equal a JSONL `sessionId`** — the sets stay disjoint. Change (2) is what makes the
proxy's `session_id` the same value the JSONL side already writes.

### And the session-scoped analyzer's row set

D7 turns a "session" from one gap-window burst of proxy rows into the whole conversation, so
`SessionEvents` would return proxy rows and JSONL rows interleaved. `ruleCacheInvalidatedByTools`
(`rules.go:313-334`) compares **consecutive** pairs and skips any pair whose `ReqBody` is empty, and
JSONL rows structurally never carry one — so the interleaving would silently break the adjacency and
the rule would fire **less** for no reason anyone chose. The rule therefore filters its own row set to
**the rows carrying a request body** before the pair walk: under the default body policy that is
exactly the proxy population, and because the session is now the whole conversation it hands the rule
**more** proxy rows than it sees today — strictly better, not a narrowing. **The filter is inside this
rule, not in `AnalyzeSession`**, so the body-less fixtures in `rules_test.go` are untouched, and **no
other session rule's code changes** — but every session rule's **input set** grows, and one other is a
consecutive-pair rule too: `ruleCachePrefixInvalidation` (`rules.go:293-307`) walks the same rows
pairwise with **no** `ReqBody` guard, so the JSONL interleaving shifts its result with no code change.
That degradation is **accepted** and asserted (§5), not fixed here. The pass's cost is §6's risk, not
this bead's — **the row-set filter is the behaviour fix, not the cost fix**.

## Rationale

Without this the proxy's `session_id` and the JSONL `session_id` are disjoint sets, so the merge —
however well it keys — lands rows in a `s_…` session the JSONL side never knows, and `clens sessions`
shows the proxy's heuristic groups rather than the conversations the requests already name. The
forward rule is also what makes br-GI-9-04's pass 2 (re-attribution) and its ordering coherent.

## Outcome Definition

- A proxy request carrying `x-claude-code-session-id` (and no `x-clens-session`) has that value as its
  `session_id`; `x-clens-session`, when present, still wins.
- Two header-carrying calls beyond the inactivity gap share one id; two header-less calls beyond it
  still split.
- `clens ingest` / `serve` / `refresh` over a fixture transcript write a `sessions` row for the
  transcript's `sessionId` — green today only because nothing wired the recorder.
- `PrefixHash` is computed even when a session header is present.
- `ruleCacheInvalidatedByTools` fires on two consecutive proxy rows whose session has JSONL rows
  interleaved between them.
- `go build ./...`, `go vet ./...`, `go test ./...` pass.

## Test Specifications

Cases are named as they appear in §5, never by a plan line range.

- Unit Tests (`internal/parse/meta_test.go`): `x-claude-code-session-id` populates
  `Meta.SessionHeader` when `x-clens-session` is **absent**; **`x-clens-session` still wins** when both
  are present; an **overlong `x-clens-session` with a valid `x-claude-code-session-id`** uses the
  second, because the overlong override does not supply a value; `TestExtractMetaPrefixHashNilWhenSessionHeaderPresent`
  changes its invariant — it asserts a **computed** hash with a header present.
- Unit Tests (`internal/session/session_test.go`): `Resolve` returns the header **as the id** — two
  header-carrying calls beyond the gap return the **same** id, and it is the header verbatim, not
  `s_…`; two header-**less** calls beyond the gap still split (the window is scoped, not dead).
  `TestResolveHeaderOverridesPrefix` and `TestResolveHeaderOnOneCallDoesNotGroup` keep passing with
  **changed meaning** (distinctness → identity) and each gains the direct assertion (the value is the
  header).
- Unit Tests (`internal/analyze/rules_test.go`): `ruleCacheInvalidatedByTools` fires on two
  **consecutive proxy rows** in a session whose rows include JSONL rows **interleaved** between them —
  the case that fails if the rule keeps its pre-filter walk. **And the accepted degradation of**
  `ruleCachePrefixInvalidation` — the mixed session does not fire where the proxy-only rows would —
  asserted so the cost is visible rather than silent.
- Unit Tests (`internal/cli`, the **tailer-wiring case**): `runIngest` over a fixture transcript writes
  a `sessions` row for the transcript's `sessionId`, asserted beside
  `TestIngestRebuildRereadsWithoutDuplicating`'s row assertions in `additions_test.go`. This case fails
  **today** (the recorder is nil), and it is the **existence** gate for the wiring the re-ingest
  wall-clock case in br-GI-9-04 only measures.
- Integration Tests: the row's `session_id` is the conversation id **both** sources name, and the
  **subagent-sidechain variant** — a `…/<sessionId>/subagents/agent-*.jsonl` line takes the **parent's**
  `sessionId`, so a sidechain line merged with its parent's proxy capture lands in the **parent**
  conversation. (Consistent with br-GI-9-04's integration assertion, not duplicated by it.)
- E2E: none.

## Files to Touch

- `internal/parse/meta.go` (modify — the second header source with `x-clens-session` winning; the
  comment block covering both sources and the length bound; **delete the `PrefixHash` nil-ing**)
- `internal/parse/meta_test.go` (modify — the second source, the override, the overlong-override case;
  the changed `PrefixHash` invariant)
- `internal/parse/types.go` (modify — `Meta.PrefixHash`'s doc comment corrected: the header wins in
  `groupKey`, not the nil. **Disjoint from br-GI-9-01's `Usage.MessageID` addition to this file** — the
  two edits do not touch the same lines and must not be merged)
- `internal/store/types.go` (modify — `EventSummary.PrefixHash`'s doc comment, same correction)
- `internal/session/session.go` (modify — `Resolve` returns `meta.SessionHeader` as the id when
  non-empty, ahead of the `seen`-map)
- `internal/session/session_test.go` (modify — the header's value **is** the id; the header-less path
  still gap-groups)
- `internal/analyze/rules.go` (modify — `ruleCacheInvalidatedByTools` filters its row set to the rows
  carrying a request body before the pair walk)
- `internal/analyze/rules_test.go` (modify — the mixed proxy+JSONL adjacency case, both rules)
- `internal/cli/ingest.go` (modify — `newTailer` calls `SetSessionRecorder`; `runIngest` builds the
  resolver)
- `internal/cli/refresh.go` (modify — `addCollectors` takes the resolver; `runRefresh` builds one)
- `internal/cli/serve.go` (modify — pass the resolver `serve` already builds through `addCollectors`)
- `internal/cli/additions_test.go` (modify — the tailer-wiring case)
- `internal/cli/cli_test.go` (modify — the seven `newTailer` call sites gain the resolver argument.
  **Disjoint from br-GI-9-04's edit to this file** — that adds `rekey` to the credential-subcommand
  table; this adds an argument to the tailer call sites. Keep the two edits separate)
