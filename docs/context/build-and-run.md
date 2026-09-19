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
| `--body-policy` | `full` | `full` \| `truncated` \| `off` |
| `--body-cap-bytes` | 262144 (256 KB) | |
| `--allow-remote` | off | the footgun flag: without it, `Validate()` rejects a non-loopback bind |
| `--session-gap-minutes` | — | session boundary heuristic |
| `--retention-days` | — | drives `clens purge` |
| `--replay` | off | the replay endpoint is opt-in |
| `--accounts-path` | `~/.clens/accounts.toml` | |

Source: [internal/config/config.go](../../internal/config/config.go) `Default()` and the `Config`
struct. `clens doctor` prints the resolved values.

## The acceptance run

[docs/acceptance.md](../acceptance.md) is the end-to-end run performed once against a real install,
recorded as it happened — including the half that could not be run on that machine (anything
needing a live Anthropic credential) and why. It is the closest thing to an E2E suite this repo has.
