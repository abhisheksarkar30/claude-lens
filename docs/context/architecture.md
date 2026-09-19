# Architecture

The shipped shape of the process, and the rules a change has to keep. Read
`docs/planning/GI-1-claude-lens-v1.md` §Architecture for the design and
`CLAUDE.md` for the enforced conventions; this file is the map to the code.

## One process, two listeners

`cmd/clens/main.go` dispatches 18 subcommands from a table. `clens serve`
(`internal/cli/serve.go`) is the only one that starts a process rather than
doing a job and exiting, and it owns every long-lived goroutine:

| Listener | Default | Serves |
|---|---|---|
| proxy | `127.0.0.1:8797` | `internal/proxy` — a transparent `/v1/messages` pass-through |
| dashboard | `127.0.0.1:8798` | `internal/api` — the JSON routes, the SSE stream, and the embedded `internal/web` assets |

Both are loopback-only unless `allow_remote` is set. `clens doctor` prints the
effective config, the bind addresses, and a PASS/WARN/FAIL per check.

## Hot path and cold path

This split is the design, not an optimization:

- **Hot path** (`internal/proxy`, on the client's goroutine): no parsing, no
  decompression, no database. It tees the request and response bytes into a
  bounded `internal/sink` and returns. The client's TTFB never depends on
  anything this tool does.
- **Cold path** (`internal/consumer`, on its own goroutine): drains the sink,
  decompresses, parses, runs the analyzer rules, prices, and writes to SQLite.

The gate that keeps this true is the TTFB test — a fake upstream streaming SSE
slowly, asserting the client sees its first event before upstream sends its
last. A proxy that buffered the whole stream would still return the correct
bytes, just late, so no correctness test would catch the regression.

**The import rule that enforces the split:** `internal/proxy` may import only
`internal/sink` and `internal/config`. If it ever imports `analyze`, `store`,
or `pricing`, the hot path has grown a dependency on the cold one. Decode and
parse live in the cold path for exactly this reason.

## The four sources and their writers

| Source | Written by | Cadence |
|---|---|---|
| `proxy` | `internal/consumer` | continuous, as calls arrive |
| `jsonl` | `internal/jsonlogs` | `clens ingest` / `refresh`, incremental from a byte cursor |
| `snapshot` | `internal/snapshot` | `clens refresh` — the claude.ai usage endpoint |
| `admin` | `internal/adminrep` | `clens refresh` — the Admin usage/cost reports |

`internal/reconcile` compares `proxy`/`jsonl` computed cost against the `admin`
billed figure; the cross-source merge in `internal/store/merge.go` folds a
`jsonl` row onto a `proxy` row when they describe the same call.

Each source runs independently. A collector that fails records its error and
the others still write — see `fail-open` below. `GET /api/sources` and the
dashboard's Sources tab are how that is made visible rather than assumed.

## Fail open

Two rules, both load-bearing:

1. **A broken observer never breaks the user's coding session.** The proxy
   returns whatever upstream returned, or a synthesized error if upstream was
   unreachable. A capture failure is logged, never propagated to the client.
2. **A broken collector never prevents the others from writing.** Each
   collector's error is recorded against its own source row.

Both are tested. Container-level evidence is in `internal/proxy` (the
pass-through tests) and `internal/ingest` (the per-source outcome handling).

## Credentials

`internal/secret` is the only package that touches a credential value, and it
writes to `~/.clens/secrets.toml` — a file **outside** the database, protected
by POSIX modes on Unix and an explicit Windows ACL on Windows (Go's `perm`
argument is a no-op there, so `0600` would be a false comfort).

Two rules a change must not break:

- **Credentials never reach the database.** Not in headers — redacted in
  `internal/proxy` before the tee. Not in bodies — the body policy and the
  256 KB cap in `internal/api`/`internal/consumer`. Full bodies *are* stored,
  so what this repo protects is the content: every prompt and every file the
  agent read.
- **`internal/api` and `internal/web` never import `internal/secret`** (guard:
  `internal/api/importguard_test.go`), nor `config`/`ingest` (same guard, plus
  `internal/cli/serve_test.go`). A write route reaches its write through an
  injected function-value seam instead, wired in the composition root.

## Injected seams

The dashboard cannot name `secret`, `config`, or `ingest`, so every write and
every dependency it has on them is a settable function value on `*api.API`:

| Seam | Producer |
|---|---|
| `SetPricing` | `pricing.Loader` |
| `SetCredentialWriter` | `secret.Save` |
| `SetAccountWriter` | the accounts file |
| `SetIngestTrigger` | `ingest.RunOnce` |
| `SetSourceHealth` | `ingest.Health` (read) |
| `SetAccounts` | the accounts file (read) |

**An unwired seam answers `503` with a reason, never an empty result.** "No
collector is wired" and "every collector is fine and has written nothing" are
different claims, and a dashboard that renders them identically is one that
cannot be trusted about the difference.

## Where to look next

- `internal/api/api.go:1` — the package doc: the route table and the seam rules above.
- `internal/store/schema.sql` — every table, one file, no migrations. See
  [storage-schema.md](storage-schema.md).
- `internal/analyze/kinds.go` — the one spelling of every warning kind. See
  [cost-and-quota.md](cost-and-quota.md) for the cost model.
- `CLAUDE.md` §Architecture essentials — the invariants in their shortest form.
