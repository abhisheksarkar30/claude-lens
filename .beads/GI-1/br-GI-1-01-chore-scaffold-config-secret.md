# Bead br-GI-1-01: Repo scaffold, module init, config, accounts, secret, doctor

**Plan Reference**: `docs/planning/GI-1-claude-lens-v1.md` — §What changes, §Security posture, §Infrastructure, §CLI

- **Bead ID**: br-GI-1-01
- **Priority**: P0 (critical)
- **Original Estimate**: 4h
- **Dependencies**: None
- **Blocks**: br-GI-1-02, br-GI-1-04, br-GI-1-06

## Description

Stand up the module and the three things every later bead needs: configuration, the credential file,
and a `doctor` that reports the truth about both.

**Module + scaffold.** `go.mod` for `github.com/abhisheksarkar30/claude-lens`, Go 1.24. Single
binary `clens` (`cmd/clens`). `.gitignore` must contain `*.db`, `*.db-wal`, `*.db-shm`,
`config.toml`, `accounts.toml`, `secrets.toml`, `/**/*review*/`, and the built binary. `LICENSE`,
and a `README.md` stub (the real one is br-GI-1-19).

**Dispatch skeleton.** Create `cmd/clens/main.go` with a package-level
`var commands = map[string]func([]string) error{...}`, an empty map in this bead, and a `main()` that
prints `clens <name>: not implemented yet` and exits non-zero for an unknown name (mirror
`deepseek-lens/cmd/lens/main.go`). Later beads append entries to this map: br-GI-1-15 adds its six,
br-GI-1-17 adds its twelve. Creating the table here is what keeps those two from both "creating" it —
see the note on the file conflict in the summary.

**`internal/config`.** Flags + TOML file + env, resolved in that order; defaults; `Validate()`.
Fields at minimum: proxy addr (default loopback `8797` — **not** deepseek-lens's 8787, to avoid a
port collision with an existing install), dashboard addr (`8798`), DB path (`~/.clens/lens.db`),
upstream default `https://api.anthropic.com`, body cap (256 KB), capture policy (`full` /
`truncated` / `off`), session gap minutes, retention days, replay-enabled flag, `--allow-remote`
footgun. `Validate()` enforces loopback binding unless `--allow-remote`, and rejects a non-loopback
`--allow-remote`-less config. `accounts` (the plan lists it in this package) holds the configured
accounts and their `billing_mode` / plan; a mixed account is two accounts.

**`internal/secret`** — this is the plan's single most important security decision, and this bead is
the only one that writes it.

- `~/.clens/secrets.toml`, **outside the database**, holding the claude.ai `sessionKey` cookie and
  the Admin API key. Nothing else in the tree may import this package.
- **The package's whole API is defined here**, so no later bead has to touch `secret.go` to add an
  accessor (and no two beads edit it concurrently — see the file-conflict flag in the summary):
  - `Save(name, value string) error` — the name is `"sessionKey"` or `"admin"`; this is the function
    the `SetCredentialWriter` seam (br-GI-1-16, wired in br-GI-1-17) is bound to.
  - `Get(name string) (string, error)` — the read used by the collectors (br-GI-1-12's snapshot poll,
    br-GI-1-13's admin pull). Returns a sentinel `ErrUnset` when the credential is absent, not `""`:
    an empty credential is not a configured one.
  - `Exists(name string) bool` and `LastUsed(name string) time.Time` (or an equivalent last-worked-at
    record) — the *only* facts the API and web layers may render; they report whether a credential is
    present and when it last worked, never its value (invariant 7, test 18).
  - `ProtectionLevel() (kind string, ok bool)` — the actual observed protection (the POSIX mode, or
    the Windows DACL entries), read back rather than assumed; `doctor` (this bead) renders it, so a
    failed ACL shows as a FAIL naming the file unprotected.
- **POSIX**: directory `0700`, file `0600`, via `os.Chmod`.
- **Windows** (the target platform): permissions bits are **not a control** — Go's `os.Chmod` and
  the `os.OpenFile` perm argument only toggle the read-only bit on Windows, so a `0600` there is a
  no-op. Access is restricted by an explicit ACL applied with `icacls` through stdlib `os/exec`.
  The mechanism is exactly:
  1. Resolve the principal **in Go** (`os/user.Current()` or the account SID; `os.Getenv("USERNAME")`
     as fallback) and pass it as an `exec.Command` **argument slice**. Never a shell string: `os/exec`
     expands no `"${USERNAME}"`, so that would reach `icacls` literally and fail with *"No mapping
     between account names and security IDs was done"*.
  2. Write a temp file **in the same directory** as `secrets.toml`.
  3. Apply the complete ACL to the temp file: `icacls <tmp> /reset`, then
     `icacls <tmp> /inheritance:r /grant:r <user>:F`. Both are load-bearing, for **different**
     reasons. `/inheritance:r` drops the ACEs the new file inherits from its parent directory
     (`%USERPROFILE%`'s `SYSTEM` / `Administrators` / user ACEs) — inheritance is what a new file
     gets, so without it those survive. `/reset` handles the other half: a file Go has just created
     on Windows carries **explicit** ACEs for SYSTEM, Administrators, and the current user —
     verified against a real `icacls` read-back, which shows no `(I)` flag on any of them — and
     `/inheritance:r` strips only *inherited* ACEs while `/grant:r` replaces only the named
     principal's own rights. `/reset` → `/inheritance:r` → `/grant:r` is the sequence that
     converges on exactly one principal; dropping `/reset` leaves three.
  4. **Read the DACL back** (`icacls <tmp>`) and assert the resulting principal set is **exactly** the
     intended one.
  5. Only then `os.Rename` over `secrets.toml`. The rename is the single atomic commit point.
- **Why temp-then-rename, not direct-on-target**: `icacls` applies its arguments in sequence, so a
  direct `/inheritance:r` followed by a failing `/grant:r` would leave a **zero-ACE DACL that denies
  everyone**, stranding an existing `secrets.toml` unreadable — a state the "does not overwrite an
  existing credential" promise does not cover, because its *permissions* would already be clobbered.
  Temp-then-rename leaves the live file byte- and ACL-identical on any failure.
- **The one safe direct-on-target case** is a file that does not yet exist: there is no prior
  credential and no prior permissions to lose, so `Save` may create it in place and apply the same
  ACL directly. Every path where the file already exists goes through temp-then-rename.
- **Fail closed.** A non-zero `icacls` exit, or a read-back DACL that does not match, means the ACL
  was not applied: `Save` **refuses to write the credential** and, when a file already exists, does
  not overwrite it. The caller reports the failure.
- `golang.org/x/sys/windows` is the fallback for shelling out, and is **not** used by default:
  `icacls` is stdlib and keeps the dependency set at the plan's stated count (plus the two decoders
  br-GI-1-04 adds — see that bead).

**`internal/cli/doctor.go`.** PASS/WARN/FAIL checks: resolved config, port-collision check, live
stats, `client_config` (read `~/.claude/settings.json` and report the effective
`ANTHROPIC_BASE_URL` upstream and any model mapping, because that env block overrides the shell), and
— this is the point — the **actual observed protection level** of `secrets.toml`: the POSIX mode, or
the Windows ACL entries on the file. If the ACL step failed, show a **FAIL** naming the file as
**unprotected**, in plain words. Report the Admin key's org-wide read scope in plain words when one is
configured. Per-source health is added in br-GI-1-14 (it needs `ingest_state`), so this bead's doctor
is config + ports + secret-protection only.

**Governance.** `.githooks/commit-msg` (rejects a commit whose subject does not start with `GI#<n>`),
`.githooks/pre-commit` (delegates to the machine-wide secret scan and refuses every commit until it
is installed), and `.github/workflows/{branch-guard,main-guard}.yml`. These are placed **here**, not
in br-GI-1-19 as the plan's bead table has it — see the flag in the summary.

**The guards must be adapted, not copied verbatim.** They are *carried over from* deepseek-lens, not
identical to it, and copying them as-is would block this repo's own pull requests:
`deepseek-lens/.github/workflows/branch-guard.yml:18` fails any PR into `main` whose `head_ref` is
not `develop`, and `main-guard.yml:33` requires every commit landing on `main` to trace to a *merged*
`develop -> main` PR. This repo deliberately has no `develop` (recorded deviation: v1 lands on `main`
via a `GI-1-…` branch), so both guards would reject exactly the workflow the plan describes — starting
with this story's own PR. Adapt the `head_ref` predicate to accept the `GI-<n>-…` shape and drop the
`develop -> main` provenance requirement (or introduce `develop`, which the plan explicitly defers).
Keep the `GI#<n>` title-prefix check unchanged — that part is correct and is the reason the guards
exist. The plan at v6 states this adaptation requirement; **it was discharged ahead of this bead**,
in the governance commit that landed `CLAUDE.md`, both workflow files, the two hooks, `.gitignore`
and `LICENSE` (`GI#1 chore: adopt deepseek-lens repository governance conventions`). This bead
therefore **verifies** those files rather than creating them, and arms the hooks with
`git config core.hooksPath .githooks`. The adaptation actually taken: the `head_ref` predicate
became `^GI-[0-9]+-[a-z0-9-]+$`, `main-guard`'s jq filter became `.head.ref | test("^GI-[0-9]+-")`,
and — because one hop to `main` no longer proves a PR came through the story-branch flow the way
`develop` did — `branch-guard` gained a fourth step requiring the branch's issue number to be one
the PR body closes.

## Rationale

Every later bead depends on config resolution, the dispatch entry point and — because the tool now
holds credentials it must persist — a credential file whose protection is a real control. Handling
the Windows ACL here means no later bead can regress to the naive in-place `icacls`; the ACL is
applied once, in one function, with the fail-closed read-back.

`secret` is a separate package so the import-direction guarantee is mechanical: no other package may
import it, and br-GI-1-18 asserts that.

## Outcome Definition

- `go build ./...`, `go vet ./...`, `go test ./...` pass.
- `clens doctor` runs against an empty install and reports config, ports and secret state without error.
- `secret.Save` writes a readable file, and a second `Save` overwrites it.
- On Windows, after a successful save the DACL read back from `secrets.toml` contains **exactly** the
  one intended principal; a simulated `icacls` failure leaves any pre-existing file byte-identical.
- `internal/config.Validate()` rejects a non-loopback bind without `--allow-remote`.
- `.githooks/commit-msg` rejects a `{"subject": "no ticket prefix"}` message.

## Test Specifications

- Unit Tests (`internal/config/config_test.go`):
  - Flag > file > env resolution order.
  - Defaults applied when nothing is set (ports 8797/8798, upstream `api.anthropic.com`).
  - `Validate()` rejects a non-loopback proxy addr without `--allow-remote`; accepts it with.
  - A mixed account (subscription + API key) parses as two accounts.
- Unit Tests (`internal/secret/secret_test.go`):
  - Round-trip: save then read returns the value.
  - Overwrite: a second save replaces the first.
  - `Get` on an unset name returns `ErrUnset`, not `""`; `Exists` is false before a save and true after.
  - `ProtectionLevel` reports the mode written (POSIX) / the DACL read back (Windows).
  - POSIX mode is `0600` and the directory `0700` (skipped on Windows).
  - **Windows DACL read-back**: after save, `icacls <file>` lists exactly the intended principal
    set (one entry) — the same read-back `Save` performs.
  - **Fail-closed**: with a stubbed `icacls` that exits non-zero, `Save` returns an error, does not
    write the credential, and leaves any pre-existing file byte- and ACL-identical.
  - **Principal is passed as an argument, not a shell string**: a save with a principal containing
    spaces resolves via `os/exec` (no shell) rather than failing account resolution.
- Unit Tests (`internal/cli/doctor_test.go`):
  - `doctor` reports FAIL naming the file unprotected when the ACL step is stubbed to fail.
  - `client_config` check reads a fixture `~/.claude/settings.json` and reports its
    `ANTHROPIC_BASE_URL`.
- E2E: none (first bead).

## Files to Touch

- `go.mod`, `README.md` (create)
- `.gitignore`, `LICENSE`, `CLAUDE.md` (verify — already committed by the governance commit)
- `.githooks/commit-msg`, `.githooks/pre-commit` (verify, then arm via `core.hooksPath`)
- `.github/workflows/branch-guard.yml`, `.github/workflows/main-guard.yml` (verify — already adapted)
- `cmd/clens/main.go` (create — dispatch map skeleton)
- `internal/config/config.go`, `internal/config/config_test.go` (create)
- `internal/secret/secret.go`, `internal/secret/secret_test.go` (create)
- `internal/cli/doctor.go`, `internal/cli/doctor_test.go` (create)
