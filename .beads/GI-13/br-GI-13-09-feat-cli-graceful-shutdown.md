# Bead br-GI-13-09: `clens shutdown` — stop a running `serve` gracefully from another shell

**Plan Reference**: none — **post-convergence addition**, requested alongside br-GI-13-07/08. It is a
tooling gap this story's own investigation ran into, not a perf change.

- **Bead ID**: br-GI-13-09
- **Priority**: P2 (medium)
- **Original Estimate**: 3h
- **Dependencies**: None
- **Blocks**: None

> **A shutdown endpoint is a remote kill switch if it is reachable remotely.** The dashboard may bind
> `0.0.0.0` (`--allow-remote`, and this repo's own operator config does exactly that) — and this
> listener **can** be widened by `--allow-remote` (`config.go:336` runs `DashboardAddr` through
> `validateLoopback` with the operator flag), so the pprof-style "put it on a listener `--allow-remote`
> cannot widen" defense is unavailable here. The route must therefore refuse a non-loopback
> **caller**. This is D5's rule applied to the caller side — read it before changing the check.

## Description

### The gap

`Serve` already shuts down correctly: `signal.NotifyContext(context.Background(), os.Interrupt)`
(`serve.go:68`) cancels the context, the select at `:260` falls through, the proxy and dashboard get a
bounded `Shutdown`, and `<-consumerDone` (`:278`) waits for the consumer's own bounded drain-and-flush
before the store closes.

The problem is reaching that path. The context is cancelled by **`os.Interrupt` only**, and the
process runs in the foreground of its own console. On Windows there is no portable way to deliver
Ctrl+C to another process's console, so the only options today are `Stop-Process` / `taskkill` — a hard
kill that skips the drain, skips the flush of the buffered sink, and skips the clean store close. That
is also the wall this story's own work hit: the running binary cannot be replaced while it is running,
so every rebuild-and-restart cycle was a hard kill of a process that a coding session's traffic was
flowing through.

### The shape

- **`internal/api`** — a `POST /api/shutdown` route, wired through the same seam pattern as
  `SetPricing` / `SetCredentialWriter` (`serve.go:126-135`, the composition root's deliberately
  injected write capabilities — `internal/api` must not reach the process lifecycle on its own). Add
  `SetShutdown(func())` and set it from `Serve` with the `NotifyContext` cancel func, so this route
  drives **the same path** Ctrl+C does rather than growing a second shutdown implementation.
- **The route needs both guards, and `originReject` alone is not enough.** Call
  `originReject(r, "shutdown")` like every other write route, **and** additionally check the caller:
  split `r.RemoteAddr` with `net.SplitHostPort` and require `loopbackHost` (`internal/api/origin.go:68`
  — the package's own predicate, not `config.IsLoopbackHost`; see the trap below) to accept the host.
  `originReject` reads only the **Host header**, and its documented purpose is the DNS-rebinding case
  ("the connection itself arrived over loopback"). A caller that can reach a `0.0.0.0` dashboard can
  send `Host: 127.0.0.1` and pass it. For a route that stops the process, the connection's actual
  origin is the thing that has to be checked. **`AllowRemote` must not widen either guard.**

- **Respond before shutting down.** Write the response and return; trigger the shutdown on a
  goroutine. A handler that cancels the context first can race its own response out of existence, and
  the CLI would report a failure for a shutdown that worked.
- **Method-gated by the method-prefixed pattern, as every write route is**: register
  `mux.HandleFunc("POST /api/shutdown", a.shutdown)`. Do **not** wrap it in `methodGet` — that helper
  accepts only GET (`api.go:221`) and would 405 the very POST the CLI sends. Do **not** add a second
  (non-method) registration or a manufactured 405 either. The catch-all `mux.Handle("/", …)`
  (`api.go:213`) matches every request, so Go's own method-mismatch 405 path never fires — a GET falls
  through to the file server and gets a **404**. `/api/ingest` (`api.go:211`) is the repo's exact
  precedent: a POST-only route with no GET sibling, whose test asserts only "not 200"
  (`ingest_test.go:104-118`, `TestIngestIsNotReachableByGet`). A 404 for a GET is also the better
  answer for a kill switch — it does not advertise the route.
- **`internal/cli/shutdown.go`** — the command. Resolve the effective config the same way `serve` does
  (flag > `CLENS_*` env > `~/.clens/config.toml` > default), then dial `DashboardAddr` **as loopback**:
  when the configured host is `0.0.0.0` or `::`, dial `127.0.0.1` with the same port. That is the
  common case, not an edge case — this repo's *operator* config (`~/.clens/config.toml`) sets
  `DashboardAddr = 0.0.0.0:8798`.
  Apply `normalizeAddr` (`serve.go:316`) first, as `serve` does, to strip a scheme/path and lowercase
  the host — but note it maps **only** `localhost` → `127.0.0.1` and leaves `0.0.0.0`/`::` untouched.
  The wildcard → loopback rewrite is therefore **new logic performed after `normalizeAddr`**, not
  something that helper covers: inspect the normalised host and, if it is any wildcard form
  (`0.0.0.0`, `::`, or empty), substitute `127.0.0.1` before dialing.
- **`cmd/clens/main.go`** — register `"shutdown": cli.Shutdown`.

### What the user sees

`clens shutdown` prints the address it contacted and returns 0. A refusal (non-loopback caller, or a
dashboard bound to a non-loopback address that the CLI therefore cannot legitimately reach) is reported
as an error naming the reason, not as a silent no-op. The process itself takes up to `shutdownGrace` to
exit after responding — **the command says so**, because a user who assumes it returned too early will
kill the process and defeat the bead.

### Traps

1. **`internal/api` cannot import `internal/config`, and that is deliberate.** The package's import
   set (catalog, consumer, decode, pricing, proxy, quota, reconcile, replay, sink, store) excludes
   `config`, which is exactly why `loopbackHost` (`origin.go:68`) exists as a **second copy** of the
   predicate `br-GI-13-04` extracted into `config.IsLoopbackHost` and wired into `validateLoopback`
   and `startPprof`. Do **not** "unify" them by importing config — that breaks the seam. Use the
   package-local one here, and leave a cross-reference comment naming the other, so a reader changing
   one predicate's rules finds the other.
2. **Do not reuse `originReject` as if it were a caller check.** See above; it is a header guard. A
   `TestShutdownRefusesANonLoopbackCaller` written with a loopback `Host` and a public `RemoteAddr` is
   the test that tells the two apart — one written with a public `Host` passes either way and proves
   nothing about the caller.
3. **`RemoteAddr` is `"ip:port"`, and may be `"[::1]:port"`.** Split it; do not string-prefix it. The
   loopback alias in this repo's own config is `127.0.0.1` but `::1` is equally loopback and equally
   reachable from the CLI.
4. **A GET is refused, not answered with a mux-generated 405.** Because the catch-all `"/"` route
   (`api.go:213`) matches every request, the ServeMux never reaches its own method-mismatch branch, so
   a `GET /api/shutdown` is served by the file server and returns **404**. `TestShutdownRefusesNonPost`
   asserts the real status — the request is **not 200** and the func is not called — mirroring
   `ingest_test.go`'s `TestIngestIsNotReachableByGet` (`:104-118`). Do not "fix" this by adding a GET
   sibling or a manufactured 405; a 404 that does not advertise the route is the intended behaviour for
   a kill switch.

## Rationale

Everything this story does has to be deployed by restarting a proxy that a live coding session is
routed through. Every one of those restarts was a hard kill. A loopback-only control route makes the
restart clean, and it is the same mechanism a future `clens reload` would need — but this bead stops at
`shutdown` and adds no reload (YAGNI).

## Outcome Definition

- `POST /api/shutdown` on a loopback-bound dashboard triggers the same graceful path as Ctrl+C, and the
  response is written before the process begins stopping.
- The route **refuses** a request whose `RemoteAddr` is not loopback, **even with `AllowRemote` true**,
  and a non-POST method is refused, not served: a GET falls through to the catch-all file server and
  gets a 404, never a 200.
- `--allow-remote` cannot make the route reachable from off-box.
- `clens shutdown` resolves `DashboardAddr` from flag/env/file, dials it as loopback when it is bound
  to a wildcard address, prints the address contacted, and exits non-zero with a reason on refusal or
  when nothing is listening.
- The hot path is untouched; `go test ./internal/proxy/` (the TTFB gate) is unchanged.
- **Verification** (from the repo root): `go build ./... && go vet ./... && go test ./... -count=1`,
  plus `go test ./internal/api/ ./internal/cli/`.

## Test Specifications

- Unit Tests (`internal/api/api_test.go`):
  - `TestShutdownTriggersTheStopFunc`: a request from a loopback `RemoteAddr` returns 2xx and the
    injected func is called exactly once. Assert the func was called **after** the response was
    written, not merely that it was called.
  - `TestShutdownRefusesANonLoopbackCaller`: a request with a **loopback `Host` header** and a
    **public `RemoteAddr`** returns 403 and the injected func is **not** called. Both halves matter:
    the loopback Host is what makes this a caller test rather than a repeat of the Host-header test,
    and it is the case a forged `Host: 127.0.0.1` from an off-box caller would present.
  - `TestShutdownRefusesANonLoopbackHost`: a public Host header with a loopback caller returns 403 —
    the DNS-rebinding case `originReject` covers, kept so the two guards cannot be collapsed into one.
  - `TestShutdownRefusesNonPost`: a GET is refused — the status is **not 200** — and the func is not
    called. Assert "not 200" rather than pinning 405, mirroring `ingest_test.go:110-113`
    (`TestIngestIsNotReachableByGet`): the catch-all route makes the mux serve the file server's 404
    rather than emit its own 405.
  - `TestShutdownWithoutASeamIsNotFatal`: with no `SetShutdown` wired, the route refuses rather than
    panicking on a nil func.
- Unit Tests (`internal/cli/shutdown_test.go`):
  - `TestShutdownDialsLoopbackForAWildcardDashboardAddr`: a config with `DashboardAddr = 0.0.0.0:8798`
    dials `127.0.0.1:8798` — asserted against the address the test server actually received. The
    wildcard case is the one that silently produces a "connection refused" nobody can explain.
  - `TestShutdownReportsAnUnreachableDashboard`: nothing listening returns an error naming the address,
    not a zero exit.
- Integration Tests: none — the two units above plus the manual check below cover the path.
- Manual (recorded in the PR body): start `clens serve`, run `clens shutdown` from a second shell, and
  show the serve console's own shutdown log lines — the proxy and dashboard shutdown reports and the
  consumer drain completing — which a hard kill does not produce.

## Files to Touch

- `internal/api/api.go` (modify — the route, its registration, and `SetShutdown`)
- `internal/api/api_test.go` (modify — the five cases above)
- `internal/cli/serve.go` (modify — wire `SetShutdown(stop)` at the composition root, next to the
  other seams)
- `internal/cli/shutdown.go`, `internal/cli/shutdown_test.go` (create — the command)
- `cmd/clens/main.go` (modify — register `"shutdown"`)

`internal/cli/serve.go` is shared with br-GI-13-04, which has landed and touched `startPprof` and the
`serve` entry point; this bead's edit is the one seam line beside the others. No other open bead
touches `internal/api`. `cmd/clens/main.go` is shared with br-GI-13-07, which adds its own
`"backfill-tool-names"` key to the same `commands` map (`main.go:16`) — the two edits are additive keys
and do not conflict.
