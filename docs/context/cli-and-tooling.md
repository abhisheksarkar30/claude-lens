[← INDEX](INDEX.md)

# CLI & Tooling

One entry point: [cmd/clens/main.go](../../cmd/clens/main.go), a `map[string]func([]string) error`
of 18 subcommands dispatching into [internal/cli](../../internal/cli/). Every subcommand accepts
the same config flag set (`--proxy-addr`, `--dashboard-addr`, `--upstream-url`, `--db-path`,
`--body-policy`, `--body-cap-bytes`, `--allow-remote`, `--session-gap-minutes`, `--retention-days`,
`--replay`, `--accounts-path`) — see [build-and-run.md](build-and-run.md).

An unknown name prints `clens <name>: not implemented yet` and exits non-zero.

## Commands

| Command | Flags | What it does | Evidence |
|---|---|---|---|
| `serve` | `--replay` | **The only long-running command.** proxy + dashboard in one process; owns every goroutine. Blocks until interrupted. | [internal/cli/serve.go](../../internal/cli/serve.go) |
| `doctor` | — | prints the effective config, the bind addresses, and a PASS/WARN/FAIL per check — including the *observed* protection level of `secrets.toml` | [internal/cli/doctor.go](../../internal/cli/doctor.go) |
| `ingest` | `--rebuild` | backfill from Claude Code's JSONL transcripts; `--rebuild` restarts from zero rather than from the byte cursor, which makes it **the re-pricing path** — the re-read merges by `request_id`, so an added rate row or a changed prefix is applied to rows already captured without duplicating them | [internal/cli/ingest.go](../../internal/cli/ingest.go) |
| `refresh` | — | run every non-proxy collector once. **The cron / Task Scheduler target.** | [internal/cli/refresh.go](../../internal/cli/refresh.go) |
| `ls` | filters | the call log, newest first | [internal/cli/ls.go](../../internal/cli/ls.go) |
| `show` | `<id>`, `--body` | one call in full. A `capture` line reads `complete` or `incomplete (truncated, or the stream ended early)` for a proxy row, and `--body` adds the read-path markers: `truncated at the read cap of N bytes`, `decoded only partially: its tail was corrupt`, or `shown raw: it would not decompress`. The wording matches the dashboard's, but the *capture* line does not name which body was cut — the dashboard compares each body's length against the cap, and this line does not. `--body` also prints the request body raw, since no compressed request body is decoded | [internal/cli/show.go](../../internal/cli/show.go) |
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

## The one destructive command

`clens purge` deletes rows. Its default is therefore the **opposite of destructive**:

- nothing is deleted without `--yes`
- `--dry-run` prints what `--yes` would have deleted

This is the pattern any future destructive subcommand should follow. See
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
