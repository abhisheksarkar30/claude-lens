[← INDEX](../INDEX.md)

# ADR 004: Credentials live outside the database, ACL-protected on Windows

**Status:** Accepted

**Context:** The tool holds two credentials: the claude.ai `sessionKey` cookie and an Admin API key.
Both are spendable or account-scoped. The rejected alternatives each failed on a specific mechanic:

- **In the database** — a single copied `lens.db` leaks a credential alongside all the content.
- **In the environment only** — unusable from a scheduled task, which is the primary way
  `clens refresh` is meant to run.
- **Relying on `0600` on Windows** — Go's `perm` argument and `os.Chmod` only toggle the read-only
  bit there. `0600` is a **no-op**, and therefore a false comfort on the target platform.
- **A shell-string `"${USERNAME}"`** — `os/exec` expands nothing, so the argument would reach
  `icacls` literally and fail with *"No mapping between account names and security IDs was done"* —
  meaning the ACL silently never ran.
- **`icacls` directly on the live file** — `/inheritance:r` applied before a `/grant:r` that then
  fails leaves a **zero-ACE DACL that denies everyone**, stranding an existing `secrets.toml`
  unreadable.

**Decision:** `~/.clens/secrets.toml`, outside the database, readable only by
[internal/secret](../../../internal/secret/).

- **Unix:** directory `0700`, file `0600` via `os.Chmod`.
- **Windows:** an explicit ACL applied with stdlib `icacls` through `os/exec`, with the principal
  resolved **in Go** and passed as an argument slice — never a shell string.
- **Temp-then-rename:** the ACL is applied to a same-directory temp file, the DACL is **read back**
  and asserted to be exactly the intended principal set, and only then is the file renamed over the
  live one. The rename is the single atomic commit point.
- **Fail closed:** a non-zero `icacls` exit or a mismatched read-back means `Save` refuses to write
  and leaves an existing file byte- and ACL-identical.
- `/reset` → `/inheritance:r` → `/grant:r` — all three. A file Go has just created on Windows
  carries **explicit** ACEs (a real read-back shows no `(I)` flag), which `/inheritance:r` alone
  does not strip; dropping `/reset` leaves three principals instead of one.

`golang.org/x/sys/windows` is the named fallback and is **not used** — `icacls` is stdlib and keeps
the dependency set at three.

**Consequences:**

- The only facts the API and web layers may render are `Exists` and `LastUsed` — presence and
  recency, never a value.
- `internal/api` and `internal/web` may not import `internal/secret` **at all**, including test
  files; a write reaches it through the `SetCredentialWriter` seam.
- `Get` returns the sentinel `ErrUnset` when absent, never `""` — an empty credential is not a
  configured one.
- `clens doctor` renders the **observed** protection level, so a failed ACL shows as a FAIL naming
  the file unprotected.
- The ACL is asserted on a freshly written file; whether it survives a profile migration or
  backup-restore is untested — recorded as `❓ UNVERIFIED` in
  [../security-and-permissions.md](../security-and-permissions.md), where the confirming evidence is
  named.
