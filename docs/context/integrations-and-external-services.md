[← INDEX](INDEX.md)

# External Integrations

Four network/disk touchpoints — the four sources — plus three Go modules. Nothing else leaves the
machine.

| Service | Purpose | Wired in at | Failure impact | Evidence |
|---|---|---|---|---|
| `api.anthropic.com` (or `--upstream-url`) | the proxied traffic itself | [internal/proxy](../../internal/proxy/) | **none on the client** — the proxy fails open, returning upstream's response or a synthesized error. A capture failure is logged, never propagated. | [config.go](../../internal/config/config.go) `Default()` |
| `GET /v1/models` | the model catalogue: what the API advertises, its limits and capabilities | [internal/catalog](../../internal/catalog/) | the Models tab loses its catalogue; **pricing state per model still renders** from local traffic and the shipped table | [internal/catalog](../../internal/catalog/) |
| claude.ai usage endpoint (source C) | the plan's own `utilization_pct` per window | [internal/snapshot](../../internal/snapshot/) | no snapshot rows; the Quota tab shows burn only and reports the source as failed on `GET /api/sources` | [internal/snapshot/snapshot.go](../../internal/snapshot/snapshot.go) |
| Admin API usage + cost + rate-limit reports (source D) | the real billed dollars, per day and model | [internal/adminrep](../../internal/adminrep/) | `reconcile` has nothing to compare against — `cost_drift` cannot fire; every other tab is unaffected | [internal/adminrep/adminrep.go](../../internal/adminrep/adminrep.go) |
| `~/.claude/projects/**/*.jsonl` (source B, **local disk**, not a service) | Claude Code's own transcripts — calls that never went through the proxy, plus session identity | [internal/jsonlogs](../../internal/jsonlogs/) | ingest reports a failed source; no merge partner for proxy rows | [internal/jsonlogs](../../internal/jsonlogs/) |

## ❓ UNVERIFIED: the source C and D endpoints

**The exact URLs and response shapes of sources C and D are not verified in this codebase**, and the
source says so directly:

- [internal/snapshot/snapshot.go:5](../../internal/snapshot/snapshot.go#L5) — *"The endpoint's exact
  URL and response shape are UNVERIFIED. It is an injectable `baseURL`, rather than asserting a
  guessed real path as fact."*
- [internal/adminrep/adminrep.go:4](../../internal/adminrep/adminrep.go#L4) — the usage/cost report
  endpoints *"are documented as curl-only and are [UNVERIFIED] against a live response"*, so every
  field is extracted **permissively** against a per-report injectable base URL.

This is a deliberate design choice, not an oversight: rather than hard-coding a guessed path, both
collectors take their base URL as a constructor argument, so a wrong guess is a configuration
change rather than a code change. **Evidence that would confirm them:** one live call per endpoint
with a real credential, recorded the way [docs/acceptance.md](../acceptance.md) records its local
half — which the acceptance run explicitly did **not** do, because it requires spending the
operator's own credential.

An agent adding or changing a field here should preserve the permissive extraction: the response
shape is not a contract this repo can currently assert.

## Request-side integrations

| Thing | Detail | Evidence |
|---|---|---|
| Auth | the user's own credential, passed through **unmodified**; `clens` never injects one except on the collectors' own calls | [internal/proxy](../../internal/proxy/) |
| `anthropic-version` | `2023-06-01` on Admin API calls | [internal/adminrep/adminrep.go:79](../../internal/adminrep/adminrep.go#L79) |
| Pagination | Admin reports are fetched following `has_more` / a next-page cursor until exhausted | `fetchAllPages`, [internal/adminrep/adminrep.go:245](../../internal/adminrep/adminrep.go#L245) |
| Credential source | `~/.clens/secrets.toml` via [internal/secret](../../internal/secret/) — **never** the database | [security-and-permissions.md](security-and-permissions.md) |

## Go module dependencies

Three beyond the standard library, and no more — the count is a stated invariant of the design:

| Module | Why | Evidence |
|---|---|---|
| `modernc.org/sqlite` | the pure-Go SQLite driver — **no cgo**, which is what keeps the single static binary story true | [internal/store/store.go:22](../../internal/store/store.go#L22) (blank import) |
| `github.com/klauspost/compress` | zstd, for bodies a client sent with `Content-Encoding: zstd` | [internal/decode](../../internal/decode/) |
| `github.com/andybalholm/brotli` | brotli, for the same reason on `br` | [internal/decode](../../internal/decode/) |

The compression libraries exist *because* decoding happens in the cold path — see
[decisions/002](decisions/002-hot-path-never-parses.md). A claim that this tool had only one
non-stdlib dependency could not be true alongside "we decode `br`/`zstd`".

`golang.org/x/sys/windows` is the named fallback for the credential ACL but is **not used**:
`icacls` is stdlib and keeps the dependency set at three. `golang.org/x/sys` is already present
indirectly via `modernc.org/sqlite`, so reaching for it would add no new module — the decision was
to prefer the stdlib path anyway (see [decisions/004](decisions/004-credentials-outside-the-db.md)).

## What is deliberately *not* integrated

| Not integrated | Why |
|---|---|
| Any CDN, webfont, or analytics | the dashboard must work offline and must not create an exfiltration path ([decisions/006](decisions/006-dashboard-with-no-build-step.md)) |
| Bedrock / Vertex / Foundry pricing | classified as `auth_kind=cloud` and stored, **not priced** — partner rates differ from first-party and are not verifiable from here |
| Any hosted service, sync, or account | single-user local tool; multi-machine sync is out of scope in v1 |
