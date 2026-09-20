[← INDEX](INDEX.md)

# Data Privacy & Compliance

Not legal advice — this is what the code actually does with sensitive content. It is a real module
here because the tool's central design decision is **to store full request and response bodies**,
which makes the database the single most sensitive artifact the tool creates.

Companion to [security-and-permissions.md](security-and-permissions.md), which covers *credential*
handling. This file covers *content*.

## Sensitive data inventory

| Field / Entity | Sensitivity | Where stored | Evidence |
|---|---|---|---|
| `events.req_body` | **Highest.** Every prompt, and every file the agent read. | SQLite BLOB, `~/.clens/lens.db` | [internal/store/schema.sql](../../internal/store/schema.sql) |
| `events.resp_body` | **Highest.** Every completion, including thinking blocks. | SQLite BLOB | [internal/store/schema.sql](../../internal/store/schema.sql) |
| `events.req_headers` / `resp_headers` | Medium — **redacted** before the tee; a credential is never in them | SQLite TEXT | [internal/proxy/redact.go](../../internal/proxy/redact.go) |
| `events.project`, `git_branch` | Low–medium — reveals what you work on | SQLite TEXT | [internal/store/schema.sql](../../internal/store/schema.sql) |
| `sessions.id`, `prefix_hash` | Low — a hash of the cacheable prefix, not its content | SQLite TEXT | [internal/store/schema.sql](../../internal/store/schema.sql) |
| Credentials | **Never stored here at all** | `~/.clens/secrets.toml`, outside the DB | [security-and-permissions.md](security-and-permissions.md) |
| JSONL transcripts (source B) | The **source** of much of the above; owned by Claude Code, read-only to this tool | `~/.claude/projects/**/*.jsonl` | [internal/jsonlogs](../../internal/jsonlogs/) |

**The threat model is the database file.** Loopback binding, redaction, and the ACL on the
credential file are all secondary to the fact that `lens.db` contains the content. Copying it copies
everything.

Storing bodies is a deliberate trade, not an oversight — the reasoning and the rejected alternative
are in [decisions/003](decisions/003-full-bodies-stored.md).

## The capture policy

Two settings on `--body-policy`, with `full` as the default:

| Policy | Behaviour |
|---|---|
| `full` (default) | the body is captured whole, up to the 256 KB cap |
| `off` | no body is captured, but the **call is still recorded**: method, path, status, redacted headers, TTFB and duration, with both body columns NULL (br-GI-7-09) |

**There was a third value, `truncated`, and it is gone.** It was accepted and read nowhere — `full`
and `truncated` executed byte-identical code, both bounded by `--body-cap-bytes`, because one cap
leaves a second value nothing to control. The vocabulary is inherited from `deepseek-lens`, where the
policy is likewise applied only for `off`. `Validate` now rejects it with a message naming the two
values that work, so a script or a `config.toml` still passing it fails at startup instead of
silently storing whole bodies an operator believed were narrowed. See
[decisions/003](decisions/003-full-bodies-stored.md).

`off` narrows what is *kept*, not what is *recorded*. That distinction is load-bearing and was not
true before br-GI-7-09: the proxy used to return the bare `ReverseProxy` under `off`, installing no
capture state at all, so **no row was written** — while `clens serve`'s banner promised "calls are
recorded without their bodies". An operator choosing the most private setting silently lost the
records too. The banner was right and the code was wrong; the code now matches it. The same policy
governs `transcript_content` (below).

The consequence is a thin row. A body that was never kept cannot be parsed, so no model, no tokens and
no cost are derivable from it — `capture_complete` stays **true**, because nothing was *narrowed*, and
the absence is a policy the operator set uniformly. Which warnings survive on such a row is tabulated
in `br-GI-7-09`; the short version is that the ones keyed on status and credential shape still fire
and everything keyed on the body does not.

`--body-cap-bytes` (default `262144`, i.e. 256 KB) bounds the capture independently of the policy,
**per body**. `events.capture_complete` records whether what was stored is the whole thing or was
narrowed — so a reader can tell "this is everything" from "this is what we kept", which is the
difference between an absent field and a truncated one. It covers **both** bodies: a request body cut
at the cap clears it exactly as a response body does (br-GI-7-08). What the row does *not* record is
which of the two was cut, so a surface that needs to say so infers it from the body's length against
the cap in force — an inference that is only as good as the cap not having changed since, which is
why the flag is the authoritative half and the length comparison only names the body. The dashboard's
transcript section carries the same length comparison for the third content column (br-GI-7-09).

A second kind of content is stored, from a source that is not the wire: **`transcript_content` /
`transcript_role`**, one assistant message's `content` from a Claude Code transcript
([internal/jsonlogs](../../internal/jsonlogs/)). It is a *reconstruction* of intent rather than a
capture — no system prompt, no tool schemas, nothing the proxy would have seen — which is why it
lives in its own columns and is labelled as such in the UI rather than being written into `req_body`.
It is the same class of data (a prompt and the files the agent read), so it is covered by the same
protections: the same file, the same file permissions, the same retention and `clens purge`.

**Both capture controls reach it, and that is new.** Until br-GI-7-09 they did not: `BodyPolicy` was
read at exactly one call site (`internal/proxy`) and this collector never consulted it, so an install
running `--body-policy off` stored a transcript line's `content` whole and uncapped. Neither GI#7's
plan nor `br-GI-7-06` mentions the policy, which is what made it read as an oversight — the bead set
out to store transcript content and never asked what the existing content controls should do about it.
The user resolved the ambiguity in favour of the controls applying, so the tailer now takes the policy
and the cap through a `SetBodyPolicy` seam wired in `internal/cli`'s `newTailer`.

Two consequences are worth knowing, because both are deliberate and neither is obvious:

- **A capped reconstruction does not clear `capture_complete`.** That flag is about the two *teed*
  bodies — br-GI-7-08 widened it to "both sides of the capture" and this is not a third. A
  reconstruction cut at a cap has narrowed nothing that was ever captured, and `transcript_content`
  lives in its own columns precisely because it is not a capture.
- **An unwired cap means no cap**, not a zero-byte one, so a caller that forgets the seam captures
  rather than silently truncating every transcript to nothing.

Before assuming the cap bounds what is in the database, check both sources.

A body that is **not captured** is `NULL`, never an empty string, and the cost model applies the
same principle to a cost it cannot compute: see [cost-and-quota.md](cost-and-quota.md).

## Retention & deletion

| Mechanism | Detail |
|---|---|
| `--retention-days` | the configured retention horizon |
| `clens purge --older-than <duration>` | delete captured rows by age |
| `clens purge --unpriced` | delete rows whose cost could not be priced |
| `--dry-run` | prints exactly what `--yes` would have deleted |
| `--vacuum` | reclaims the space afterwards |

**The destructive command defaults to the opposite of destructive.** Nothing is deleted without
`--yes`; `--dry-run` is independent of it (so `--dry-run --yes` reports and deletes in one pass,
which is deliberate — [internal/cli/purge.go:20](../../internal/cli/purge.go#L20)).

**Cascade is scoped:** `ON DELETE CASCADE` appears only on `warnings.event_id`. Purging an event
takes its warnings with it and nothing else — `sessions` is not cascaded, which is why a session
whose events were purged can still exist.

**No soft deletes.** A purge is a delete. There is no `deleted_at` column and no undo.

## What is *not* done

Recorded here so a reader does not assume a control that does not exist:

- **No PII detection or redaction of body content.** The redactor covers credentials in *headers*
  only. A prompt containing a name, an address, or a customer record is stored verbatim.
- **No encryption at rest.** `lens.db` is a plain SQLite file. Protection is filesystem
  permissions and the fact that it is on your machine.
- **No consent capture flow, no data-subject request tooling, no audit log of reads.** None of the
  flows this tool models involve a third party whose data is being processed, so none is
  implemented.
- **No telemetry or phone-home.** Nothing leaves the machine except the proxied traffic itself and
  the collector calls to the endpoints the user configured.

⚠️ ASSUMPTION: the licence and the README frame this as a single-user developer tool, so GDPR/CCPA
data-controller obligations are assumed to sit with the user, not the tool. If this were ever
distributed as a service, every "what is not done" item above becomes a compliance gap.

## Applicable regime notes

| Regime | Relevance |
|---|---|
| GDPR / CCPA | Only if a user's prompts contain personal data about other people. The tool stores whatever passes through it, so the obligation follows the content, not the tool. |
| Anthropic's own terms | Source B (JSONL) reads Claude Code's local transcripts; sources C and D call the user's own account endpoints with the user's own credential. The tool adds no third-party data flow. |
| Secrets handling | Covered by [security-and-permissions.md](security-and-permissions.md) — the credential never enters the database, and the file that holds it has an explicit ACL on Windows. |
