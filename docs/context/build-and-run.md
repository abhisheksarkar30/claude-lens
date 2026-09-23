[← INDEX](INDEX.md)

# Build & Run

One buildable unit: the `clens` binary. Commands are taken from
[CLAUDE.md](../../CLAUDE.md) §Setup/§Commands and [README.md](../../README.md), not inferred.

## `clens` — the single binary

| Task | Command | Source |
|---|---|---|
| Build | `go build ./...` | CLAUDE.md §Commands |
| Install | `go install github.com/abhisheksarkar30/claude-lens/cmd/clens@latest` | README §Install |
| Test (all) | `go test ./...` | CLAUDE.md §Commands |
| Test (single package) | `go test ./internal/proxy/` | CLAUDE.md §Commands |
| Test (race, the two concurrent packages) | `go test ./internal/api/... ./internal/web/... -race` | the SSE broker and the embedded-asset reader are the concurrent pair |
| Vet | `go vet ./...` | CLAUDE.md §Commands |
| Format | `gofmt -l .` (must print nothing) | no linter config exists in the repo |
| Run locally | `go run ./cmd/clens serve` | CLAUDE.md §Commands |
| Run (doctor) | `go run ./cmd/clens doctor` | CLAUDE.md §Commands |
| Deploy | **none** — a local developer tool, no hosted deployment | README §Install |

No `Makefile`, no build tags, no cgo (`modernc.org/sqlite` is the pure-Go driver, which is what
keeps the single-static-binary story true).

`gofmt -l .` printing nothing is the intent, but **on a Windows clone with `core.autocrlf=true` it
flags every file** — the checkout is CRLF throughout and `gofmt` always wants LF. That is a line-
ending artefact, not a style failure, and `gofmt -w` there would rewrite every line of every file.
The gates this repo actually enforces are `go build`, `go vet`, and `go test` ([CLAUDE.md](../../CLAUDE.md)
§Commands); there is no format gate in CI.

## Local dev setup

One step per clone, and it is not optional:

```
git config core.hooksPath .githooks
```

That arms [.githooks/commit-msg](../../.githooks/commit-msg) (rejects any commit whose subject does
not start with `GI#<n>`) and [.githooks/pre-commit](../../.githooks/pre-commit), which delegates to
a machine-wide secret scan and **refuses every commit** until that scan is installed. Installing it
is a one-time step from a separate checkout:

```
bash git-hooks/install.sh      # from your agentic-ai-artifacts checkout
```

## Running it against your own traffic

```
go run ./cmd/clens serve            # proxy 127.0.0.1:8797, dashboard 127.0.0.1:8798
export ANTHROPIC_BASE_URL=http://127.0.0.1:8797
```

Then open `http://127.0.0.1:8798`. `clens doctor` first is the tool's own advice: it reports the
config the binary *actually resolved*, the resolved bind addresses, and a PASS/WARN/FAIL per check.

## Environment variables / secrets

Names only — no values appear in this repo.

| Name | Purpose | Required in |
|---|---|---|
| `CLENS_*` | the environment tier of config resolution (flags > `CLENS_*` > config file > defaults) | any subcommand |
| `ANTHROPIC_BASE_URL` | set by the **user**, in their shell or `~/.claude/settings.json`, to point a client at the proxy | client side, not read by `clens` |
| `USERNAME` / the account SID | Windows fallback for resolving the ACL principal | [internal/secret](../../internal/secret/) on Windows only |

Credentials themselves are **not** environment variables in the supported path. They live in
`~/.clens/secrets.toml`, written through `clens` (or the dashboard's `POST /api/secrets`) and read
only by [internal/secret](../../internal/secret/). See
[security-and-permissions.md](security-and-permissions.md).

## Config knobs

Every subcommand accepts the same flag set; precedence is flags > `CLENS_*` > file > defaults.

| Flag | Default | Notes |
|---|---|---|
| `--proxy-addr` | `127.0.0.1:8797` | not deepseek-lens's 8787 — deliberate, to avoid a port collision |
| `--dashboard-addr` | `127.0.0.1:8798` | |
| `--upstream-url` | `https://api.anthropic.com` | |
| `--db-path` | `~/.clens/lens.db` | |
| `--body-policy` | `full` | `full` \| `off`. **`off` keeps the call row and drops only the bodies** (br-GI-7-09). A third value, `truncated`, was accepted and read nowhere; it is now **rejected at startup** rather than silently treated as `full`; see [data-privacy-and-compliance.md](data-privacy-and-compliance.md) |
| `--body-cap-bytes` | 2097152 (2 MB) | raised from 262144 (256 KB) in GI-11: 58% of this workload's request bodies crossed the old cap and were stored as truncated conversation histories. It bounds **what is stored**, per body, never what is forwarded — and it is the bound on how much prompt and file content sits in the database at rest, so eight times the cap is eight times the exposure per row on a `full` capture. **The store's size grows with it**: the whole reason to raise it was that the bodies were worth keeping, and they are kept in `lens.db`. See [data-privacy-and-compliance.md](data-privacy-and-compliance.md) before lowering or raising it again |
| `--allow-remote` | off | the footgun flag: without it, `Validate()` rejects a non-loopback bind |
| `--session-gap-minutes` | — | session boundary heuristic |
| `--retention-days` | — | drives `clens purge` |
| `--replay` | off | the replay endpoint is opt-in |
| `--accounts-path` | `~/.clens/accounts.toml` | |
| `--pprof-addr` | unset (disabled) | serves `net/http/pprof` on this address for the process's life — a diagnostic, deliberately absent from `Default()`. **Loopback-locked even under `--allow-remote`**: a profile dumps whatever is in process memory, including prompt and response bodies, so `Validate()` hardcodes `allowRemote=false` for this one field rather than reusing `c.AllowRemote` (`internal/config/config.go`'s `IsLoopbackHost`, shared with the general bind check so a legitimately-loopback address can't pass one and fail the other) |

Source: [internal/config/config.go](../../internal/config/config.go) `Default()` and the `Config`
struct. `clens doctor` prints the resolved values.

Two keys have **no flag** — they are config-file / `CLENS_*` only, and both are deliberately absent
from `Default()` so an unconfigured install falls back to the shipped table's own values rather
than to a second copy of them that could drift:

| Config key | `CLENS_*` | Unset → | `none` → |
|---|---|---|---|
| `PeakOffPeakDates` | `CLENS_PEAK_OFF_PEAK_DATES` | the 33 bundled 2026 dates | no holiday excluded |
| `ApiModelPrefixes` | `CLENS_API_MODEL_PREFIXES` | `deepseek-` | nothing routed pay-as-you-go |

Comma-separated; `none` is the sentinel for an empty list, and unset is *not* the same as empty —
the difference is preserved by a `nil`-vs-empty check on both sides
([internal/cli/ingest.go:100-105](../../internal/cli/ingest.go#L100-L105)). Read the two `none`
columns as **more expensive, not less**:

- Clearing `PeakOffPeakDates` does not disable peak pricing. Peak is a property of the shipped
  DeepSeek rows; the dates are only the holiday exclusions, so an empty list means every weekday
  peak hour is billed at peak.
- Clearing `ApiModelPrefixes` does not make anything free — it stops DeepSeek rows being routed to
  the `api` account, so they fall back to the collector's `subscription` default and their cost
  lands in the hypothetical column instead of `cost_usd`.

`Validate()` rejects a malformed date (not `YYYY-MM-DD`) and an empty or whitespace-only prefix.
`clens doctor` prints the resolved state as `N (default)`, `N`, or `none`.

## Where the store lives

`--db-path` / `DBPath` names the SQLite file, and **that setting is the only place the location is
recorded — the repo does not own it.** Nothing in the source tree hard-codes a path: `clens` resolves
the store from the flag, then `CLENS_DB_PATH`, then the operator's `~/.clens/config.toml`, then the
`~/.clens/lens.db` default. A `D:\` literal in a Go file would be wrong on every machine but one, so
there is none — grep for `D:\` and the only hits are in these docs.

**Where the store is on any given machine is a question for `clens doctor`, not for this file** — it
prints the resolved `db_path`, and this file does not restate it. A runbook that asserts a particular
install's state is a runbook that goes stale the moment the operator follows it, and this one did:
written first as already moved, corrected to not-yet-moved. So: the reason to move it on a workstation
is that the GI-11 cap change grows the store by roughly a third of a gigabyte, better spent on a
non-system volume, and the move is an **operator action, not a migration the repo performs**. The
ordered form of it is:

1. Stop the running `clens serve`.
2. **Copy** (never move) `~/.clens/lens.db` to `D:/clens/lens.db` — the original stays as the
   fallback until step 6.
3. Set `DBPath = "D:/clens/lens.db"` in `~/.clens/config.toml`.
4. Start `clens serve` and confirm the new path is the one in use: `clens doctor` prints `db_path`.
   Read `body_cap_bytes` off the same output while you are there — after GI-11 it should read
   `2097152`, and a smaller number means the running binary predates the cap raise.
5. Confirm the store is intact and the row count matches the original.
6. Only then remove the copy on `C:`.

**Step 1 is what makes step 2 a plain file copy.** A stopped process has checkpointed its
write-ahead log, so `lens.db` is self-contained; copying it while `serve` runs would silently drop
everything still in the `-wal` beside it. It is **destructive only at step 6**, which is why the copy
is verified before the original is deleted.

> **⚠️ Stopping `clens serve` cuts the client that is talking through it.** The proxy listener is what
> `ANTHROPIC_BASE_URL` points at, so every in-flight call loses its connection the moment step 1 runs
> and does not get it back until step 4 — **including, on a machine where the agent's own traffic is
> proxied, the session issuing the command.** Stop it from a context that does not route through this
> process: a second terminal, with the client pointed at `--upstream-url` directly for the duration.
> This applies to **every** step that says "with `clens serve` stopped", not just the move — `clens
> rekey`'s pass-3 destructive run, and both of GI-11's repairs.
>
> **And the binary cannot be upgraded in place while it runs.** `go install ./cmd/clens` writes
> `$(go env GOPATH)/bin/clens`, and Windows refuses to overwrite a running image (`Permission
> denied`), so a new build needs the same stop. Order the two together: stop → `go install` → copy
> or repoint the store → start → `clens doctor`.

**Step 2 moves one file, and `~/.clens/` holds more than that.** `lens.db.pre-migration` (the
pre-`PRAGMA user_version` copy, see [decisions/007](decisions/007-schema-migrations-by-user-version.md))
and the `backups/` directory are not part of the move and are not covered by step 6 — so an operator
who follows the steps literally will find the store relocated and those two still on `C:`. That is
correct, not an incomplete move: they are history, not the live store, and deleting them is a
separate decision with its own retention question.

## The acceptance run

[docs/acceptance.md](../acceptance.md) is the end-to-end run performed once against a real install,
recorded as it happened — including the half that could not be run on that machine (anything
needing a live Anthropic credential) and why. It is the closest thing to an E2E suite this repo has.
