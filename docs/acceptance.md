# Acceptance run

The end-to-end run br-GI-1-19 closes on, executed once against a real install
on 2026-09-19 (Windows 11, `clens` built from `GI-1-claude-lens-v1`) and
recorded here as it happened — including the two parts that could not be run
on this machine, and why.

Two halves:

- **Local** — everything reachable with the binary, a loopback listener, and a
  throwaway database. **Executed.**
- **Live** — everything that needs a real Anthropic credential: a billed call,
  a quota snapshot, an Admin cost row, and the test-11(b) `request-id` ↔
  `requestId` capture. **Not run.** Every credential environment variable was
  unset, and `~/.claude/.credentials.json` is the operator's own OAuth
  credential, which this tool must not read or spend.

The commands below are literal; `$CLENS` stands for the built binary and
`$BODY` for a request body file.

## Local half — executed

### 1. `clens serve` starts and both listeners answer

```
$ clens serve
```

The banner printed the resolved config, both bind addresses
(`127.0.0.1:8797` proxy, `127.0.0.1:8798` dashboard) and no warnings. Every
read route answered:

| Route | Status | Observed |
|---|---|---|
| `GET /` | 200 | the embedded dashboard shell |
| `GET /api/health` | 200 | healthy |
| `GET /api/sources` | 200 | all four sources present |
| `GET /api/requests` | 200 | empty page, not an error |
| `GET /api/sessions` | 200 | empty |
| `GET /api/warnings` | 200 | empty |
| `GET /api/warnings/summary` | 200 | empty |
| `GET /api/models` | 200 | catalogue |
| `GET /api/stream` | 200 | SSE connection held open |

`GET /api/quota`, `GET /api/reconcile` and `GET /api/accounts` answered 200
with a `"(no accounts configured)"`-shaped body rather than 503, because their
read seams *are* wired — a 503 here would have meant the composition root
failed to bind them.

### 2. Cross-origin write is rejected

```
$ curl -i -X POST -H 'Origin: http://evil.example' http://127.0.0.1:8798/api/ingest
HTTP/1.1 403 Forbidden
```

403, as the shared guard requires, and the collector did not run.

### 3. The proxy passes through, fails open, and captures

```
$ curl -i -X POST -H 'x-api-key: <placeholder>' -H 'content-type: application/json' \
    --data @$BODY http://127.0.0.1:8797/v1/messages
HTTP/1.1 401 Unauthorized
```

The 401 came from **upstream** — the listener forwarded the call, returned
upstream's status unchanged, and did not fail closed on the placeholder
credential. That is the fail-open and pass-through behaviour in one
observation.

The call then appeared in `GET /api/requests` as:

| Field | Value |
|---|---|
| `Source` | `proxy` |
| `AuthKind` | `api_key` |
| `BillingMode` | `api` |
| `RequestID` | the composite fallback key, since the 401 carried no `request-id` header |
| `CostUSD` | NULL, `CostSource` `unpriced` — an uncosted row is absent, never `$0.00` |

This is also the one place the fallback identity key was exercised for real:
with no `request-id` response header, the row was still keyed and merged-able.

### 4. Collectors run, and a dead one is visibly dead

```
$ curl -s -X POST -H 'Origin: http://127.0.0.1:8798' http://127.0.0.1:8798/api/ingest
{"triggered":true}
```

Afterwards `GET /api/sources` reported:

| Source | Status | Rows written |
|---|---|---|
| `jsonl` | **ok** | 72836 |
| `snapshot` | unknown | — |
| `admin` | unknown | — |
| `proxy` | ok | — |

`jsonl` flipping to `ok` is the collector working. `snapshot` and `admin`
staying `unknown` is the more useful half: they had no credential, and the
surface says *unknown*, not `ok` and not `error`. A dashboard that rendered
"nothing reported" as green would be lying, and this is the evidence that it
does not.

```
$ clens refresh
jsonl root ok rows=4
```

`rows=4` on the second pass is the byte cursor doing its job — `refresh` is
incremental, so it re-read only the four lines appended since the first run
rather than re-reading 72836.

### 5. Quota, reconcile and models

```
$ clens quota
(no accounts configured)
```

Every window reports `unconfigured` rather than a percentage of a limit nobody
supplied. The configured-limit path was **not** exercised — see below.

```
$ clens reconcile
<header: computed (sources A/B) vs billed (source D), per day and model>
(no billed rows yet — an API account has not been reconciled against the Admin report)
source_mismatch warnings on record: 0
```

The header states the scope rather than leaving an empty table ambiguous, and
the two figures are separate labelled columns — computed and billed are never
added together.

```
$ clens models
<the shipped rate catalogue: claude-fable-5-1, claude-sonnet-5, ... marked `shipped`>;
 <claude-opus-4-7, claude-opus-4-6, claude-haiku-4-5 marked `provisional`>
```

`shipped` vs `provisional` is visible in the output, which is the point: a
bundled-but-unverified rate is labelled rather than presented as fact.

The server was stopped with `taskkill //PID <pid> //F` (SUCCESS).

## Live half — NOT RUN

None of `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` or
`CLAUDE_CODE_OAUTH_TOKEN` was set in the environment, and
`~/.claude/.credentials.json` holds the operator's own OAuth credential, which
this run must not read or spend. Each item below needs a credentialed operator
at a keyboard.

### Not run: a real billed call through the proxy

```
$ clens serve
$ export ANTHROPIC_BASE_URL=http://127.0.0.1:8797
$ claude -p "say hi"          # or any client pointed at the proxy
$ clens ls                    # expect exactly one new row, Source=proxy, AuthKind=oauth
```

The local half proved the capture path with a 401; what is unproven here is
that a **200** with a real `usage` block lands exactly one row with tokens and
a cost, and that the Sources tab renders the proxy source green.

### Not run: a real quota snapshot

```
$ clens refresh               # with a sessionKey configured via the Settings tab
$ clens quota                 # expect a snapshot line per account
```

The snapshot collector has no credential, so `snapshot` stayed `unknown` and
no `quota_snapshots` row exists to cross-check the computed burn against.

### Not run: an Admin cost row, and the reconcile happy path

```
$ clens refresh               # with an admin key configured (the store asks for confirmation)
$ clens reconcile             # expect per-(day, model) computed vs billed columns, and a drift verdict
```

`cost_drift` only ever applies to API accounts with billed rows on record; with
no Admin key, `reconcile` correctly reports that it has nothing to reconcile
against rather than reporting zero drift (which would read as "verified").

### Not run: the Quota tab with a limit configured

```
$ clens quota                 # after configuring a 5h/7d limit
```

The `unconfigured` rendering is confirmed; the configured-percentage rendering
and the calibration candidate list are not. Both need a subscription account
and a limit, neither of which this run had.

### Not run: test 11(b) — the `request-id` ↔ `requestId` equivalence

This is the verification the plan's *Cross-source identity* section names as a
prerequisite to relying on the cross-source merge, and it is owned by no bead.
It needs one live call captured on both sides at once:

```
$ clens serve
$ export ANTHROPIC_BASE_URL=http://127.0.0.1:8797
$ curl -si -X POST -H "x-api-key: $ANTHROPIC_API_KEY" -H 'content-type: application/json' \
    --data @$BODY http://127.0.0.1:8797/v1/messages | grep -i '^request-id:'
request-id: <HEADER_VALUE>

$ grep -h '"requestId"' ~/.claude/projects/**/*.jsonl | tail -1
{"requestId":"<JSONL_VALUE>", ...}
```

**Outcome: never exercised, not falsified.** Recorded in
`docs/planning/GI-1-claude-lens-v1.md` §Cross-source identity, scoped as: moot
on this install — GI#9's message-id tier already converges a proxy row and its
JSONL counterpart without needing the header match — but load-bearing and
unverified, and **silently failing** (no error, just two rows instead of one)
on any install whose upstream sends `request-id` if that header were ever
byte-unequal to the JSONL `requestId`. No equality may be asserted here that
was not observed.

## Findings

**`clens doctor`'s `client_config` check cannot fail.** It reported **PASS**
while `~/.claude/settings.json` declared
`ANTHROPIC_BASE_URL=http://127.0.0.1:8787` — port **8787**, not this tool's
**8797**. `internal/cli/doctor.go` reports the declared URL and returns
`statusPass` unconditionally, so a client pointed at the wrong port (or at
nothing at all) reads as healthy.

This is out of scope for br-GI-1-19, which touches documentation rather than
the check. It is recorded here because a doctor that cannot report the one
misconfiguration it exists to find is a defect worth its own bead: the check
should at minimum compare the declared URL's port against the resolved proxy
address, and WARN on a mismatch.

**`go.mod`'s `// indirect` markers are all stale.** Every requirement is
marked indirect, including the three modules the repo's own source imports
directly. `go mod tidy` would correct the markers; until then nothing may
derive the direct set from them, which is why the README-consistency test
derives it from the source's own imports instead.
