[← INDEX](INDEX.md)

# Security & Permissions

High-stakes module: this tool sits in the path of every prompt, holds a credential that can spend
money, and stores the content of every request. The design premise is that traffic and prompts stay
on this machine, and the defaults are chosen to hold that.

## AuthN / AuthZ model

**There is no authentication and no user model.** This is a single-user local developer tool, and
the bind address *is* the security boundary.

| Surface | Control | Enforced by |
|---|---|---|
| Both listeners | bind `127.0.0.1`, loopback only | `config.Validate()` rejects a non-loopback bind unless `--allow-remote` was passed |
| Dashboard reads | **none** — loopback is the control | deliberate |
| Dashboard writes | same-origin guard (`originReject`) | [internal/api/origin.go](../../internal/api/origin.go) |
| Replay (the billable route) | opt-in, off by default (`clens serve --replay`) | [internal/api/replay.go:42](../../internal/api/replay.go#L42) |
| `POST /api/shutdown` (GI#13) | same-origin **plus** a loopback-caller check on `r.RemoteAddr`; **`--allow-remote` widens neither guard** | [internal/api/shutdown.go](../../internal/api/shutdown.go) |
| `POST /api/reload` (GI#16) | same guards as shutdown: same-origin **plus** a loopback-caller check; `--allow-remote` widens neither | [internal/api](../../internal/api/) |
| `archive/` directory (GI#16) | `0700` dir, `0600` day files on Unix; **no-ops on Windows** (no ACL is applied, unlike `secrets.toml`). Holds full bodies, so as sensitive as `lens.db` | [internal/store/archive.go](../../internal/store/archive.go) |
| `net/http/pprof` (`--pprof-addr`, GI#13) | off by default; loopback only, **and `--allow-remote` cannot widen it** | `config.Validate()` hardcodes `allowRemote=false` for this one field ([internal/config/config.go](../../internal/config/config.go)); `internal/cli`'s `startPprof` re-checks with the same `IsLoopbackHost` predicate before spawning the listener, so a `Config` built by anything other than `config.Load` can't skip `Validate` and open one anyway |

`--allow-remote` is documented as the footgun flag, not a feature: it exists so that binding a
public interface is an explicit act rather than a default nobody noticed.

### The same-origin guard

Every write route shares `originReject`:

- A request with **no** `Origin` header **passes** — browsers always send one, so its absence means
  a non-browser caller, which is the CLI's own path. This is deliberate, not a hole.
- A request **with** an `Origin` must match the request's own `Host`, compared as host:port.
- A failure is `403`, and is rejected **before any byte reaches upstream**.

Rejections are counted in `replayRejected` on `GET /api/health`, so a probe against the one billable
route leaves a trace.

### The shutdown route's second guard (GI#13)

(GI#16: `POST /api/reload` reuses this guard verbatim. It rewrites live retention/archival settings, so it is loopback-only for the same reason.)

`POST /api/shutdown` is the one route that stops the process, and same-origin alone is not enough
for it: `originReject` reads only the `Host` header, so a caller that can already reach a dashboard
bound to `0.0.0.0` (`--allow-remote`) can forge `Host: 127.0.0.1` and pass it regardless of where
the connection actually came from. The handler additionally splits `r.RemoteAddr` with
`net.SplitHostPort` (it may be `"[::1]:port"`) and tests the host with `loopbackHost` — a
package-local predicate in [internal/api/origin.go](../../internal/api/origin.go), deliberately not
`config.IsLoopbackHost`, since `internal/api` does not import `internal/config` (see Containment
rules below). `--allow-remote` cannot widen this: the route is loopback-only unconditionally. The
response is written, then the stop func runs on its own goroutine, so a shutdown that cancels the
server's context first can never race its own response out of existence.

## Credential handling

**Credentials never reach the database.** The mechanism, in three parts:

### 1. Redaction before the tee

[internal/proxy/redact.go](../../internal/proxy/redact.go) rewrites a clone of the header map
before the bytes are submitted to the sink. The caller still sends the **original, unredacted**
copy upstream — redaction is applied to the capture only.

| Constant | Value | What it covers |
|---|---|---|
| `sensitiveHeaders` | `X-Api-Key`, `Authorization`, `Cookie`, `Sessionkey` | replaced with `[redacted]` **regardless of how many values they carry** |
| `redactedValue` | `[redacted]` | the replacement |
| `adminKeyPattern` | `sk-ant-admin` | any header value containing this substring, **under any header name** |

The multi-value detail is load-bearing: `http.Header.Get` returns only the *first* value, so a
second `x-api-key` would slip through unredacted. The implementation checks with `Values` and
rewrites with `Set` (which replaces every value with the single redacted one).

**`RedactCheck` is a startup self-test**, run by `serve` and `doctor`: it scans a captured call's
stored header JSON for a reachable credential value redaction should already have removed, so a
regression is caught before traffic flows. It never quotes the value it found — a log is *less*
protected than the database the check exists to keep the value out of, so quoting it would move the
credential rather than catch it. The header name and length are reported instead.

**Redaction is not conditional on the body policy.** `--body-policy off` narrows what is kept, not
what is protected: the header clone is redacted before the policy branch is reached, so an `off` row
still stores `[redacted]` in the header blobs it *does* keep. That is the case that matters most —
`off` is the setting an operator picks *because* they care what lands in the database — and it is
pinned by a test rather than left to the ordering of two statements.

**Fail-open, and one place it used to break.** A capture failure is logged, never propagated: the
client's session must not depend on this tool. The `off` path violated that between GI#7's bead 06
and its bead 09 — `requestID` hashed a nil request body, and on the transport-failure path the panic
landed *before* the 502 was written, so a failed upstream returned an aborted connection instead. It
was invisible because `net/http` recovers handler panics and logs them, and because the only test
that drove the path set the very header that avoids the nil read. `TestPolicyOffSurvivesAMissingRequestID`
now covers both halves.

### 2. The credential file lives outside the database

`~/.clens/secrets.toml` holds the claude.ai `sessionKey` cookie and the Admin API key.
[internal/secret](../../internal/secret/) is the **only** package that touches a value, and the
database is never a copy of it — so a copied `lens.db` does not leak a credential.

The package's whole surface is fixed so no caller needs an accessor added:

| Function | Purpose |
|---|---|
| `Save(name, value string) error` | the write; bound to the `SetCredentialWriter` seam |
| `Get(name string) (string, error)` | the collectors' read. Returns the sentinel `ErrUnset` when absent — **an empty credential is not a configured one** |
| `Exists(name string) bool` | presence only |
| `LastUsed(name string) time.Time` | when it last worked |
| `ProtectionLevel() (kind string, ok bool)` | the **observed** protection, read back rather than assumed |

`Exists` and `LastUsed` are the *only* facts the API and web layers may ever render — whether a
credential is present and when it last worked, never its value (invariant 7, test 18).

### 3. Protection: POSIX modes, or an explicit Windows ACL

The target platform is Windows, where **permission bits are not a control**: Go's `os.Chmod` and
the `os.OpenFile` perm argument only toggle the read-only bit, so `0600` there is a no-op and would
be a false comfort.

| Platform | Mechanism |
|---|---|
| Unix | directory `0700`, file `0600`, via `os.Chmod` |
| Windows | an explicit ACL applied with stdlib `icacls` through `os/exec` |

The Windows sequence, and why each step is load-bearing:

1. **Resolve the principal in Go** (`os/user.Current()` or the account SID; `USERNAME` as fallback)
   and pass it as an `exec.Command` **argument slice**. Never a shell string: `os/exec` expands no
   `"${USERNAME}"`, so that would reach `icacls` literally and fail with *"No mapping between
   account names and security IDs was done"*.
2. **Write a temp file in the same directory** as the target.
3. **Apply the complete ACL**: `icacls <tmp> /reset`, then
   `icacls <tmp> /inheritance:r /grant:r <user>:F`. All three verbs are needed, for different
   reasons: `/inheritance:r` drops the ACEs the new file inherits from its parent
   (`%USERPROFILE%`'s `SYSTEM` / `Administrators` / user ACEs); `/reset` handles the *other* half —
   a file Go has just created on Windows carries **explicit** ACEs for SYSTEM, Administrators, and
   the current user (a real `icacls` read-back shows no `(I)` flag on any of them), and
   `/inheritance:r` strips only *inherited* ACEs while `/grant:r` replaces only the named
   principal's own rights. The sequence converges on exactly one principal; dropping `/reset`
   leaves three.
4. **Read the DACL back** and assert the principal set is **exactly** the intended one.
5. **Only then `os.Rename`** over `secrets.toml`. The rename is the single atomic commit point.

**Why temp-then-rename, not direct-on-target:** `icacls` applies its arguments in sequence, so a
direct `/inheritance:r` followed by a failing `/grant:r` would leave a **zero-ACE DACL that denies
everyone**, stranding an existing `secrets.toml` unreadable. Temp-then-rename leaves the live file
byte- and ACL-identical on any failure.

**Fail closed.** A non-zero `icacls` exit, or a read-back DACL that does not match, means the ACL
was not applied: `Save` **refuses to write the credential** and does not overwrite an existing file.

The one safe direct-on-target case is a file that does not yet exist — there is no prior credential
and no prior permissions to lose.

`clens doctor` renders the **actual observed** protection level, so a failed ACL shows as a FAIL
naming the file unprotected, in plain words.

## Role / access model

None — there are no roles. The closest thing is `auth_kind`, which classifies the *shape* of the
credential a call presented (`oauth` / `api_key` / `admin` / `cloud` / `unknown`) and stores only
the classification. See [cost-and-quota.md](cost-and-quota.md). An `admin` key appearing on the data
plane is a misconfiguration the analyzer flags as `auth_kind_anomaly`.

## Containment rules (import direction)

These are mechanical properties enforced by tests, not review habits:

| Rule | Asserted by |
|---|---|
| `internal/api` and `internal/web` never import `internal/secret` **at all** — including test files | [internal/api/importguard_test.go](../../internal/api/importguard_test.go) |
| …nor `internal/config` or `internal/ingest` (non-test files) | the same guard, plus [internal/cli/serve_test.go](../../internal/cli/serve_test.go) |
| `internal/proxy` imports only `sink` and `config` | [internal/proxy/importguard_test.go](../../internal/proxy/importguard_test.go) |
| `internal/store` never imports `internal/pricing` (non-test files; the guard skips `_test.go` on purpose, so a test may use a real rate table) | [internal/store/importguard_test.go](../../internal/store/importguard_test.go) |

A route that must write a credential reaches it through an **injected function-value seam**
(`SetCredentialWriter` → `secret.Save`), so there is no import edge a future handler could reach one
back through. An unwired seam answers `503`, never an empty result.

The store/pricing rule is the same seam in the other direction, and it is the one a reader is most
likely to mistake for an oversight: `Store.RepriceCosts` needs to price rows, and it takes a
`PriceComputer` argument rather than importing the rate table. The reason is directional — the store
is the single-writer transaction, the pricing table is a leaf, and an import edge between them would
make the write path depend on the cost catalogue. See [conventions.md](conventions.md) §Layering.

## Untrusted rendering (the dashboard)

The dashboard builds HTML by string concatenation and assigns `innerHTML`, so escaping is a manual,
load-bearing control rather than a framework's. Two inputs are attacker-influenced in the ordinary
course of the tool's job, and both arrive from a **remote** endpoint rather than from the user:

| Input | Where it is rendered | Control |
|---|---|---|
| response and request **bodies** | the call detail's `<details>` sections | `esc()` inside one top-level `bodySection` — a body is arbitrary bytes, so this is the surface that matters most |
| stored **header** blobs | the same detail's `kv` tables | `esc()` per key and value in `headerRows` |

The design rule is **one renderer per untrusted class**, so the escaping has one place to review
rather than one per call site. A second call site is not a style question here: this page also holds
a replay button that spends money, so an injected `<script>` is not merely a defaced dashboard.

`internal/web/assets_test.go` guards it — `TestAssetsTheBodyRendererEscapes` fails if `esc(` leaves
the renderer, and its doc states the ceiling plainly: with no JS runtime in this toolchain the test
proves the escaping **call is present in the source**, not that the rendered pixels are safe. A
change to body rendering should be re-checked by hand in a browser; the test cannot do it for you.

## Network posture

**Nothing is fetched from the network by the dashboard.** No CDN, no webfont, no analytics — the
HTML, CSS, and JS are `go:embed`-ed, and the charts are hand-rolled inline SVG. A third-party
script load would be both an offline failure mode and an exfiltration path for a tool that holds
every prompt. See [decisions/006](decisions/006-dashboard-with-no-build-step.md).

## Known gaps

- **`--allow-remote` is a footgun with no further guard.** Once set, there is no auth in front of
  reads, and the same-origin check is not a substitute (a non-browser caller sends no `Origin`).
  ⚠️ ASSUMPTION: the flag is intended for a trusted network only; the plan documents the loopback
  default rather than the remote case.
- ❓ UNVERIFIED: whether the Windows ACL survives a `robocopy`/backup-restore of `%USERPROFILE%`.
  The code asserts the ACL on a freshly written file; it does not re-assert it on an existing one
  that something else has rewritten. Evidence that would confirm or refute: `icacls` on a restored
  `secrets.toml` after a profile migration.
