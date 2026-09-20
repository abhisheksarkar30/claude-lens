[← INDEX](../INDEX.md)

# ADR 003: Full bodies are stored, by default

**Status:** Accepted

**Context:** What makes this tool more than a usage counter is that it can answer *why* a call
behaved as it did — which request parameters the API silently dropped, what the cache breakpoint
actually marked, what was in the prompt when a rule fired. Those answers need the body. The
rejected design stores metadata and token counts only, which is safer and cannot answer any of them.

Storing bodies means the database holds every prompt and every file the agent read. That is the
whole privacy posture of the tool, and it is a deliberate trade rather than an oversight.

**Decision:** Capture **full request and response bodies by default**, bounded by a 256 KB cap, with
an explicit policy to narrow it:

| `--body-policy` | Behaviour |
|---|---|
| `full` (**default**) | body captured whole, up to `--body-cap-bytes` |
| `truncated` | **accepted and inert** — no code path distinguishes it from `full` (see below) |
| `off` | no body captured; the **call is still recorded** (br-GI-7-09) |

`events.capture_complete` records whether what was stored is the whole thing or was narrowed, so a
reader can tell "this is everything" from "this is what we kept". It covers **both** bodies since
`br-GI-7-08`; before that it was derived from the response buffer alone, so a request body cut at the
cap was stored as a prefix with the row still reporting a whole capture. It is a *capture* flag:
`off`, where nothing was captured, leaves it true, and so does a `transcript_content` narrowed at the
cap, because a reconstruction is not a capture.

Two corrections since this record was written, both of which change what the policy above actually
buys you and neither of which invalidates the decision itself:

- **`off` did not mean "no bodies".** Until `br-GI-7-09` the proxy returned the bare `ReverseProxy`
  under `off`, so *no call row was written at all* — a strictly larger withholding than the flag, the
  banner, this file and the README all described. The code now matches the words.
- **`truncated` is inert.** `config.Validate` accepts it and `internal/proxy`'s only policy branch is
  `== "off"`, so it executes byte-identical code to `full`. Whether it was meant to mean *uncapped* is
  not recorded anywhere; until that is answered, the `full` row above is the honest description of
  both.

Credentials are kept out by a separate mechanism — redaction before the tee
([../security-and-permissions.md](../security-and-permissions.md)) — not by refusing to store
bodies.

**Consequences:**

- **`~/.clens/lens.db` is the asset this repo protects.** Loopback binding, redaction, and the
  credential file's ACL are all secondary to that fact, because the content is in the database.
- Retention is a first-class concern: `--retention-days` plus `clens purge` (dry-run by default,
  `--yes` required).
- **No encryption at rest and no PII detection in body content.** The redactor covers credentials in
  headers only; a prompt containing personal data is stored verbatim. Recorded as a gap in
  [../data-privacy-and-compliance.md](../data-privacy-and-compliance.md).
- The dashboard is offline-only by the same reasoning: a CDN script would be an exfiltration path
  for content this tool exists to hold ([006](006-dashboard-with-no-build-step.md)).
