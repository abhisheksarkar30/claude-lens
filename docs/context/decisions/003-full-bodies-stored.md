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
| `truncated` | narrowed before storage |
| `off` | no body captured |

`events.capture_complete` records whether what was stored is the whole thing or was narrowed, so a
reader can tell "this is everything" from "this is what we kept".

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
