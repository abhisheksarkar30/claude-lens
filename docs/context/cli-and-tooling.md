[← INDEX](INDEX.md)

# CLI & Tooling

One entry point: [cmd/clens/main.go](../../cmd/clens/main.go), a `map[string]func([]string) error`
of 21 subcommands dispatching into [internal/cli](../../internal/cli/). Every subcommand accepts
the same config flag set (`--proxy-addr`, `--dashboard-addr`, `--upstream-url`, `--db-path`,
`--body-policy`, `--body-cap-bytes`, `--allow-remote`, `--session-gap-minutes`, `--retention-days`,
`--replay`, `--accounts-path`) — see [build-and-run.md](build-and-run.md).

An unknown name prints `clens <name>: not implemented yet` and exits non-zero.

## Commands

| Command | Flags | What it does | Evidence |
|---|---|---|---|
| `serve` | `--replay` | **The only long-running command.** proxy + dashboard in one process; owns every goroutine. Blocks until interrupted. | [internal/cli/serve.go](../../internal/cli/serve.go) |
| `doctor` | — | prints the effective config, the bind addresses, and a PASS/WARN/FAIL per check — including the *observed* protection level of `secrets.toml` and the database's `db_schema` version beside the one this binary knows. `db_schema` reports Open's own outcome, since Open migrates before returning: a skew is refused there, so the check FAILs rather than reporting a mismatch | [internal/cli/doctor.go](../../internal/cli/doctor.go) |
| `ingest` | `--rebuild` | backfill from Claude Code's JSONL transcripts; `--rebuild` restarts from zero rather than from the byte cursor, so an added rate row or a changed prefix is applied to rows already captured without duplicating them. It is **not** the general re-pricing path (it was described as one before GI-11): a re-read produces a *JSONL* row, priced with `speed=""` / `serviceTier=""`, and the `request_id` merge never replaces the proxy's bodies — so it cannot reach the proxy-only rows. Use `reprice` for stored costs | [internal/cli/ingest.go](../../internal/cli/ingest.go) |
| `refresh` | — | run every non-proxy collector once. **The cron / Task Scheduler target.** | [internal/cli/refresh.go](../../internal/cli/refresh.go) |
| `ls` | filters | the call log, newest first | [internal/cli/ls.go](../../internal/cli/ls.go) |
| `show` | `<id>`, `--body` | one call in full. A `capture` line reads `complete` or `incomplete (truncated, or the stream ended early)` for a proxy row, and `--body` adds the read-path markers: `truncated at the read cap of N bytes`, `decoded only partially: its tail was corrupt`, or `shown raw: it would not decompress`. The wording matches the dashboard's, but the *capture* line does not name which body was cut — the dashboard compares each body's length against the cap, and this line does not. `--body` also prints the request body raw, since no compressed request body is decoded. A row captured under `--body-policy off` reads `complete` and prints no bodies: nothing was narrowed, so there is nothing to mark | [internal/cli/show.go](../../internal/cli/show.go) |
| `tail` | — | the last few calls, then every new one as it lands | [internal/cli/tail.go](../../internal/cli/tail.go) |
| `stats` | `--by` | window totals, a per-model split, and `--by` another grouping | [internal/cli/stats.go](../../internal/cli/stats.go) |
| `sessions` | filters | one row per agentic run | [internal/cli/sessions.go](../../internal/cli/sessions.go) |
| `warnings` | `--detail` | findings grouped by kind, or one row each with `--detail` | [internal/cli/warnings.go](../../internal/cli/warnings.go) |
| `export` | filters | every matching call as JSON lines or CSV | [internal/cli/export.go](../../internal/cli/export.go) |
| `replay` | `<id>`, `--dump`, `--diff <id>` | re-issue a captured call. `--dump` and `--diff` **send nothing** — they print the payload / the diff | [internal/cli/replay.go](../../internal/cli/replay.go) |
| `quota` | — | rolling 5h/7d burn per subscription account, and calibration candidates | [internal/cli/quota.go](../../internal/cli/quota.go) |
| `accounts` | — | configured accounts and their plan/billing state | [internal/cli/accounts.go](../../internal/cli/accounts.go) |
| `reconcile` | — | computed (A/B) vs billed (D) cost, per day and model | [internal/cli/reconcile.go](../../internal/cli/reconcile.go) |
| `models` | — | the rate catalogue, and which models have no rate at all | [internal/cli/models.go](../../internal/cli/models.go) |
| `prices` | — | the effective rate table, plus the edit paths | [internal/cli/prices.go](../../internal/cli/prices.go) |
| `purge` | `--yes`, `--dry-run` | delete captured rows by age or by the unpriced predicate | [internal/cli/purge.go](../../internal/cli/purge.go) |
| `rekey` | `--yes`, `--dry-run` | the one-off historical backfill: pass 1 re-keys `proxy:`-synthetic rows from the body id already inside `resp_body`, pass 2 re-attributes proxy rows from the conversation id already inside `req_headers`, pass 3 deletes and re-derives the `jsonl:`-keyed rows whose identity was never stored. `--dry-run` reports N/M/K/L plus the dangling-`replay_of` count and a re-pricing note; run with `clens serve` stopped | [internal/cli/rekey.go](../../internal/cli/rekey.go) |
| `reprice` | `--yes`, `--dry-run` | re-price stored rows in place from the current rate table: `cost_usd` / `api_equivalent_cost_usd` / `cost_source` are recomputed over **every** event row, and no row is inserted or deleted. Rows whose `cost_source` does not name a reconstructible input set are skipped rather than repriced (`repriceInScope`): `unpriced`, and any `approximate:<reason>` except `cache_ttl_unknown`. The repair for the per-class cent rounding GI-11 fixed — and, unlike `ingest --rebuild`, a path to the proxy-only rows that carry an understated cost. `--dry-run` prints the moved/unchanged/skipped split and writes nothing | [internal/cli/reprice.go](../../internal/cli/reprice.go) |
| `reflag` | `--yes`, `--dry-run` | re-derive `capture_complete` from a `Content-Length` witness: a stored body that is a strict prefix of the client's declared length flips the flag to `incomplete`. Reports flipped / already honest / residual, where a residual row is one the merge laundered with no witness left to prove it — **not repairable**, and reported rather than guessed at. The matching historical repair for RC-B | [internal/cli/reflag.go](../../internal/cli/reflag.go) |

## The `--yes`-gated writers: four writers, two destructive

Four subcommands refuse to write without `--yes`, and it is worth keeping the two groups apart,
because **the shared gate is not a shared property**:

| | Deletes rows? | What it rewrites |
|---|---|---|
| `purge` | **yes** | removes captured rows by age or the unpriced predicate |
| `rekey` | **yes** | pass 3 deletes and re-derives the `jsonl:`-keyed rows |
| `reprice` | no | `cost_usd` / `api_equivalent_cost_usd` / `cost_source` |
| `reflag` | no | `capture_complete` |

`reprice` and `reflag` delete nothing and insert nothing — they recompute a column from the row's own
stored inputs, which is why a wrong or unwanted run is repaired by running the other one, not by
restoring a backup. The destructive set is still exactly `purge` and `rekey`, and the comments in
`internal/cli/purge.go` and `internal/cli/rekey.go` say "two" deliberately.

All four default to the **opposite of destructive**:

- nothing is written without `--yes`
- `--dry-run` prints what `--yes` would have written

This is the pattern any future subcommand that writes or deletes should follow. `rekey` additionally refuses
outright (no rows changed, non-zero exit) when its pass-3 precondition fails — every recorded JSONL
cursor must still name a readable file at least as large as its stored byte offset, and must live
under the walked root — because pass 3's delete is unconditional over the `jsonl:` prefix and would
otherwise destroy rows nothing could rebuild. See
[data-privacy-and-compliance.md](data-privacy-and-compliance.md) for what retention means here.

## Dev tooling

| Tool | Purpose |
|---|---|
| [.githooks/commit-msg](../../.githooks/commit-msg) | rejects a commit whose subject does not start with `GI#<n>` |
| [.githooks/pre-commit](../../.githooks/pre-commit) | delegates to a machine-wide secret scan; **refuses every commit** until that scan is installed |
| `.github/workflows/{branch-guard,main-guard}.yml` | the PR governance guards — see [infra-and-deploy.md](infra-and-deploy.md) |

Both hooks are armed by `git config core.hooksPath .githooks`, a one-time step per clone
([CLAUDE.md](../../CLAUDE.md) §Setup).

There is **no** `Makefile`, task runner, or build script — `go build`/`go test`/`go vet` are the
whole toolchain.
