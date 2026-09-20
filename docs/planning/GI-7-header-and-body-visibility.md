# GI-7 — Captured headers and bodies are invisible, and nothing says whether the proxy is in the path

**Issue**: [GI#7](https://github.com/abhisheksarkar30/claude-lens/issues/7)
**Branch**: `GI-7-header-and-body-visibility`
**Status**: converged (plan v8, 8 review rounds)

---

## 1. Summary

`clens` captures request and response headers and bodies, stores them, and serves them over its own
API — and the dashboard shows none of it. A user looking at a captured call sees a metadata table,
warnings, and a replay form. The single most valuable thing the tool holds — what was actually sent
and what actually came back — is unreachable from the UI.

Three separate facts produce that outcome, and they need separate fixes:

1. **The dashboard has no body view.** `grep -rn "ReqBody\|RespBody\|atob" internal/web/` returns
   nothing. No asset in the tree references either field. This is not a rendering bug; the view was
   never written.
2. **The list routes ship every body they can find, and the detail route ships them undecoded.** The
   Calls list is the hottest fetch in the dashboard, and today `GET /api/requests?limit=50` returns
   **12.1 MB**; the sibling `/api/sessions/{id}` route ships the same class of payload, larger per
   call. Adding a body view on top of that without fixing both would make an existing performance
   problem worse, so the projection is a prerequisite, not a nicety.
3. **Nothing tells the user whether the proxy is in the request path at all** — which is the failure
   that produced this story. On this machine `clens serve` was healthy, listening, and capturing;
   Claude Code was pointed at a different product on a different port. The dashboard looked fine and
   showed only transcript rows. A user had to run `clens doctor` and read a PASS line to discover
   that the proxy was being bypassed.

A fourth question — whether non-proxy (transcript) rows can carry bodies too — has a real answer, and
it is not "no". See §3 D4.

---

## 2. Evidence base

Every claim below was verified against the running system or the source at HEAD `5e0dedf`, not
inferred from the docs.

### 2.1 Capture and storage already work in proxy mode

`events` carries `req_headers TEXT`, `resp_headers TEXT`, `req_body BLOB`, `resp_body BLOB`
(`internal/store/schema.sql:49-52`). Against the live instance, a real captured call:

| Field | Observed |
|---|---|
| `ReqBody` | 166,834 bytes, plain JSON, readable |
| `RespBody` | 917 bytes, **brotli-compressed** |
| `ReqHeaders` | JSON, `Authorization` present as `[redacted]` |
| `CaptureComplete` | `true` |

So this story adds no capture work for proxy rows. The bytes are already there.

### 2.2 The API already serves them — including where it should not

`GET /api/requests/{id}` returns `eventDetail{*store.Event, Warnings}` (`internal/api/api.go:366-371`),
and `store.Event` promotes the four fields. Measured against the live instance:

| Request | Response size | Contains bodies |
|---|---|---|
| `GET /api/requests?limit=2` | 669 KB | **yes** |
| `GET /api/requests?limit=50` | **12,149,782 bytes** | **yes** |

The list handler (`api.go:330-364`) passes `store.EventFilter` to `store.ListEvents` and encodes the
result directly. No projection, no opt-in, no `?body=` parameter. The dashboard's Calls tab calls
this on every tab switch and discards the bodies.

`[]byte` marshals to base64 in Go's `encoding/json`, which is why the wire form of both bodies is
base64 — that is stdlib behaviour, not an encoding step anyone wrote.

### 2.3 Response bodies are stored compressed

Claude Code advertises `Accept-Encoding: gzip, deflate, br, zstd`; DeepSeek honours it. The proxy
stores the bytes it teed, unmodified, because decompressing on the client's goroutine is precisely
what `TestNoBufferingSSE` exists to forbid (CLAUDE.md: *"decompressing on the client's goroutine is
what the TTFB gate forbids"*).

`internal/decode.Body(headers, body, cap)` undoes gzip/deflate/br/zstd and strips `Content-Encoding`
and `Content-Length`; it is covered by `TestBodyDecodesEveryAdvertisedCoding`. It is called from
exactly one place — `internal/consumer/consumer.go:299`, on the cold path — and only to reach the
`usage` object for token extraction. The decoded bytes are used for parsing; the stored body stays
raw.

The consequence is user-visible today:

```
$ clens show 81955 --body
request body (166834 bytes):
{"model":"deepseek-flash","max_tokens":2112,...      ← readable

response body (917 bytes):
e 81  ����OW�d���Gxd��AV%x$@:@dd@dƩ��S�T�M��U,TD   ← mojibake
```

### 2.4 Response bodies cannot be decoded in the browser

The dashboard is `go:embed`-ed vanilla JS under a hard no-build-step rule
(`decisions/006-dashboard-with-no-build-step.md`). The platform decompressor,
`DecompressionStream`, supports `gzip`, `deflate` and `deflate-raw` — **not** `br`. Brotli in the
browser would require a bundled library, which the no-build-step rule forbids.

So decoding is necessarily a server-side, read-path concern. `atob` alone would only undo the
base64, leaving brotli.

### 2.5 Transcripts do carry content; the collector discards it

A real transcript on this machine (`~/.claude/projects/d--github-claude-lens/ef875ca8-….jsonl`)
carries full message content:

```json
{"type":"assistant","message":{"id":"…","role":"assistant","model":"deepseek-flash",
 "content":[{"type":"thinking","thinking":"Let me start by understanding the task.…"}], …}}
```

Content block types present in that one file: `text`, `thinking`, `tool_use`, `tool_result`.

`internal/jsonlogs/dedup.go:12-29` parses each line into:

```go
type line struct {
    Type, UUID, SessionID, RequestID, Timestamp, CWD, GitBranch, Version, CliEntrypoint string
    Message *message `json:"message"`
}
type message struct {
    Model      string      `json:"model"`
    StopReason string      `json:"stop_reason"`
    Usage      *usageShape `json:"usage"`
}
```

`content` is never decoded, so it is never stored. Transcripts carry **no headers at all** — not
redacted, absent.

### 2.6 The mode signal exists but is informational only

`internal/cli/doctor.go:247-283` already reads Claude Code's `settings.json` and extracts
`env.ANTHROPIC_BASE_URL` (`readSettingsBaseURL`, `doctor.go:252-266`). Its own doc comment is explicit:
*"It can only PASS"*. It reports the value and never compares it against `cfg.ProxyAddr` — so the one
condition a user actually needs to know about (client pointed somewhere else) reads as `[PASS]`.

The *observed* half has **no** source today. `GET /api/sources` reports three collectors — `jsonl`,
`snapshot`, `admin` — and never `proxy`: `internal/ingest/ingest.go:42-46` defines only those three,
`:184` iterates exactly them, and `:9-12` states the proxy is *deliberately not one of `RunOnce`'s
collectors*. `LastSuccessAt` for source `proxy` is a field of nothing. The observed signal therefore
has to be built, not reused — the candidate is a store read of the newest `events.started_at` where
`source='proxy'`, exposed through the mode seam (D5, F1.6).

---

## 3. Design

### D1 — The list paths return a body-free summary; a caller that needs a body says so explicitly

**The problem.** 12.1 MB per Calls-tab fetch. The list route selects all 45 columns and encodes the
rows directly (`api.go:350-363`), so every `req_body`/`resp_body` blob is read out of SQLite and
base64'd onto the wire on a fetch whose view renders none of them. The sibling session route is worse
per call: `/api/sessions/{id}` builds `Calls []*store.Event` from `SessionEvents` (`api.go:539`,
`:557`), which uses the same full column list (`store.go:205`), and `showSession` (`app.js:263-274`)
renders id/time/model/tokens/cost — no bodies (F1.3). The route is bounded by a summary sibling
(`SessionEventsSummary`, F2.1); the store method itself is not, because the analyzer's session pass
reads bodies out of it (D1).

**The shape: `EventSummary`.** `ListEvents` returns `[]*EventSummary` (`internal/store/types.go`) — the
scalar block, and **no** non-scalar column. Concretely it carries none of the four header/body columns
(`ReqHeaders`, `RespHeaders`, `ReqBody`, `RespBody`) **and neither of D4's two transcript columns**
(`transcript_content`, `transcript_role`) — six excluded columns once bead 06 lands (F3.3). The point
is not shipping 12 MB of JSON; it is not reading 12 MB of blobs (and, after bead 06, every transcript
BLOB) out of SQLite on every tab switch. The excluded columns are never read on the list path:
omitting them from the `SELECT` — not zeroing them after the read — is the point.

**`Event` embeds `EventSummary`, so the scalar block has one home (F3.2).** `Event` is
`EventSummary` plus exactly the six non-scalar columns (the four header/body fields, plus D4's two
transcript fields). Embedding is what keeps the ~40 scalar fields defined once: a duplicate
`EventSummary` would have to agree with `Event` field-for-field, and `store.Event` has no JSON tags —
its wire keys are its Go field names — so a divergence would silently split the list wire from the
detail wire. `encoding/json` flattens the embedded struct, so the detail route's wire keys are
unchanged. Two consequences the code must absorb:

- `statusCell` (`ls.go:105`) and `displayModel` (`format.go:230`) take `*store.EventSummary`, so one
  definition serves both halves: the summary callers (`ls.go:79-80`, `tail.go:97-98`) pass the row,
  and the three full-path callers (`show.go:61-62`, `export.go:182`, `internal/cli/replay.go:162`)
  pass `&ev.EventSummary`.
- A composite literal that sets a now-promoted scalar field no longer compiles — Go forbids promoted
  fields as composite-literal keys — and becomes `&store.Event{EventSummary: store.EventSummary{…}}`.
  Three production sites (`consumer.go:398`, `jsonlogs.go:381`, `stats.go:80`) and the test fixtures
  that build an `Event` by field take that mechanical one-line wrap; a literal that sets only one of
  the six direct columns (`&store.Event{ReqBody: …}`) is unchanged. §4 names them. This ripple is the
  one cost of sharing the block, and it is a compile error, not a silent one.

**`SessionEvents` is not retyped; the session route gets its own summary method (F2.1).** The round-1
draft put `SessionEvents` on the summary too, and that is a blocker, not a style choice: its rows are
fed to `analyze.AnalyzeSession` (`consumer.go:244`), and `ruleCacheInvalidatedByTools` reads
`prev.ReqBody`/`cur.ReqBody` (`rules.go:306-327`, `:310`, `:320`) — the exact field the summary exists
to remove. Three `Store` interfaces declare the method (`api.go:58`, `consumer.go:39`,
`jsonlogs.go:35`), so retyping the concrete return value stops `*store.Store` satisfying the latter two
and the build fails at the composition root before any rule runs. `SessionEvents` therefore **keeps
returning `[]*store.Event`**, and the session *route* — whose only use of a row is `c.ID`
(`api.go:557`, `:565`) — is bounded by a **sibling method**, `SessionEventsSummary` (the same `SELECT`
minus `summaryOmittedColumns`), which it alone calls. This is the split D1 already draws for the list
path (`ListEvents` summary / `ListEventsFull` full), applied to the session path;
`publishing_store.go`'s promoted `SessionEvents` is untouched.

The reason for a distinct **type** rather than a `store.EventFilter` flag is the failure mode. With a
flag, a caller that forgets it silently reads bodies it did not ask for, and — defaulted off — silently
gets nil where it needed bytes (the `sources.go:39-42` ambiguity the earlier draft invoked, which on the
wire is total: `store.Event` has no JSON tags, so a nil `[]byte` marshals as `"ReqBody":null` and a
body-less `jsonl` row is byte-identical to an unselected proxy row, `types.go:68-76`, F1.2). With a
distinct return type, a caller that needs a body cannot even name the field: reaching one is a
**compile error**, not a silent default. The `EventFilter` body-flag design is **withdrawn**.

**Why not zero the fields in the API handler.** That fixes the wire size and leaves the read cost. The
dashboard is a local tool and the blob read is real work.

**The callers that do need full rows, and the rest (F1.1/F3.1).** The earlier draft claimed
`ListEvents` had "exactly two callers, both metadata-only". That was false: the full non-test
inventory is nine call sites, and four of them must keep a **full** row. They stay on `ListEventsFull`
(a sibling method returning `[]*Event`, the existing `SELECT`+`scanEvent`), named at the call site so
opting in is visible:

- `internal/cli/serve.go:261` — `checkRedaction`, the boot-time credential-leak self-test. It reads
  `ev.ReqHeaders` (`:266-270`) and hands it to `proxy.RedactCheck`. Miss this and it `continue`s on
  every row and reports zero findings, always, with no error and no log — a security control disabled
  by default.
- `internal/cli/export.go:90,131` — `clens export`'s two reads. Its JSON form is documented as the
  complete dump *including the captured bodies* (`export.go:18-21`), so it needs the full path and both
  reads stay on it.
- `internal/api/replay.go:246` — `awaitReplayRow`'s **return value is not reduced to `.ID`** (F3.1).
  `api/replay.go:142` hands the row to `replay.OutcomeOf(*store.Event, …)`, so the row must stay a
  full `*store.Event`; `newestReplay` (`:224`) shares the method family and stays on the full path
  too. Both keep their `*store.Event` signatures, which is why `internal/replay/replay.go` (the
  `OutcomeOf` signature, `replay.go:36`) and `internal/cli/replay.go` (`:245`, the second `OutcomeOf`
  caller, fed by `getEvent` → `st.GetEvent` → `*store.Event`) are **unchanged**. The poll reads one row
  per `replayPollInterval`, not 50 rows per tab switch, so the blob cost the summary exists to avoid is
  not reintroduced.
- `internal/cli/ls.go:55-62` — `clens ls --json` encodes the whole row (`enc.Encode(ev)` per row), so
  its output today is the full `store.Event`, bodies included. On the summary type those four keys
  would vanish, which is the same silent contract change the story exists to fix (F5.2) — so the
  `--json` branch reads through `ListEventsFull` and emits what it always has. The **table** path
  (`:66-97`) renders id/time/model/tokens/cost only and needs no bodies, so it stays on the summary.
  This is the one caller that would *notice* the retype and must not; `clens export` is kept full for
  the same reason (`export.go:18-21`, above).

The rest take the summary and do not notice:

- `internal/cli/tail.go:49,72` (`printNew` is retyped to `[]*store.EventSummary`)
- `internal/cli/stats.go:162`
- `internal/cli/purge.go:138`
- `internal/quota/quota.go:65`
- the API list route itself (`api.go:350-363`) and the session route (`api.go:557`, on
  `SessionEventsSummary` rather than the full method — F2.1)

**How the split is kept from drifting (F1.15/F3.3).** Two column lists (a summary `SELECT` and the full
`eventSelectColumns`) plus two positional scans is the cost of this shape; `eventSelectColumns` is
shared by four read paths (`store.go:194`, `:205`, `:230`, `merge.go:81`) and `scanEvent` is a
positional `row.Scan` of 45 destinations (`store.go:996-1020`), so a column added to one list and not
the other is a silent wrong-column read, not an error. The exclusion is a **named list**,
`summaryOmittedColumns` — the four header/body columns today, and bead 06 extends it with
`transcript_content` and `transcript_role`, because a transcript reconstruction is body-class payload
the list path must not read (the BLOB is the read cost D1 exists to avoid).
`TestSummaryColumnsAreTheFullSetMinusBodies` asserts the summary list equals `eventSelectColumns`
**minus `summaryOmittedColumns`** — so the assertion tracks the list exactly, and after bead 06 it
demands **six** columns be absent. It cannot pass while the summary quietly selects a transcript BLOB,
and bead 06 extends one list instead of editing an assertion.

**What this does not change.** The detail route keeps returning full bodies. It is fetched once per
drill-down, for one row. `SessionEvents` keeps returning full events (F2.1), so the analyzer's session
pass and the two narrow `Store` interfaces that feed it are untouched.

### D2 — Response bodies are decoded on the read path, in one place, and never on the hot path

**Where.** `decode.Body` itself, called from both places that render a body — the API detail route and
`clens show --body` — so a body reads the same in the UI and the terminal. It takes the stored
`resp_headers` (which still carry `Content-Encoding`) plus the stored body. There is no second
wrapper: D2's completeness signal (below) is added *to* `Body`, where the one-byte-past-the-cap read
can actually see it, rather than inferred by a caller that cannot (F2.2).

**Why not in the browser.** §2.4: brotli is not available to `DecompressionStream`, and the
no-build-step rule rules out a library.

**The cap is injected, because `internal/api` cannot read config (F1.4).** The read-path cap is
`BodyCapBytes` — the same value the proxy tees with and the consumer already decodes with
(`serve.go:105`, `consumer.go:95-98`). But `internal/api` may not import `internal/config`:
`importguard_test.go:31-38` bans `/internal/config` (and `/internal/ingest`, `/internal/secret`) from
`internal/api` and `internal/web`, and `serve_test.go` asserts the same two edges. So the cap arrives
as a seam, `SetBodyCapBytes(n int)`, wired from `internal/cli/serve.go` exactly as `SetBodyDecoding` is
on the consumer side and as `SetSourceHealth`/`SetAccounts` are on the read side (`api.go:137-145`).
The detail response also carries the cap, because D3's marker has to *name* it and the browser has no
other way to learn it.

**The unwired cap is a documented supported state, and the zero is guarded (F4.4).** `internal/api` cannot
read the cap on its own: the configured `BodyCapBytes` lives in `internal/config`, which the guard above
bans, and the cap's default is **unexported** (`defaultBodyCapBytes = 262144`,
`internal/consumer/consumer.go:24`), so the `internal/consumer` import `api` already holds still buys no
reachable fallback. `SetBodyCapBytes`
therefore **ignores a non-positive `n`** — an unset or zero cap leaves the seam unwired, exactly as an
unset `SetSourceHealth` is a supported state (`api.go:96-106`, `:137-145`) — and the detail route **calls
`decode.Body` only when the cap is positive**. Both guards matter because a zero cap is not benign:
`decode.Body(…, 0)` returns the raw body *and* an error (`decode.go:66-68`), so a wiring omission would
otherwise make D3 render *"response shown undecoded — it would not decompress"* for a body that decodes
fine. With the cap unwired the route does not decode: it serves `RespBodyDecoded = RespBody` (raw) with
`BodyCapBytes == 0`, and D3 keys the **cap-not-configured** line off `BodyCapBytes == 0` **before** it
consults `RespBodyCompleteness`, so `NotDecoded`'s wording is reachable only when a positive cap was
applied **and** the body was compressed and the bytes still would not decompress — never manufactured by
a missing cap, and never for a body with no `Content-Encoding`, which is `Complete` (F6.1). Wiring the cap is
the composition root's job (`serve.go`), and T1/T2/T4 set it explicitly on the handlers they build.

**Bounds.** `decode.Body` already takes a cap; the read path passes the same `BodyCapBytes`. A
compressed blob that expands past the cap is truncated, not unbounded — this is the decompression-bomb
guard, and it is the reason to reuse the existing function rather than write a new one.

**`decode.Body` must report completeness itself, and the round-1 wrap could not (F1.5/F2.2).** The
draft had a helper *infer* the completeness of `Body`'s return. That is not derivable, and two of
T4's four cases are unassertable under it. `Body` reads `limit+1` bytes and keeps the prefix when the
cap cut the stream, but it returns the error **only** when *nothing* decoded (`decode.go:93-100`:
`io.ReadAll(io.LimitReader(r, limit+1))`, `out = out[:limit]`, error iff `len(out) == 0`). So an error
means "nothing decoded" — never "cap hit" or "corrupt tail after a clean prefix" — and a wrapper sees
no signal for either. The draft's cap test, `len(decoded) == limit`, is worse than useless: it is a
false positive for a body whose true decoded length is exactly `limit` (`decode.go:91-92` reads one
byte past the cap precisely so the two are not confused). The wrap is therefore **withdrawn** and the
signature changes so the read path is handed the signal the function already computes:

```go
type Completeness int

const (
    Complete       Completeness = iota // fully available: no Content-Encoding, or EOF reached within the cap
    TruncatedAtCap                     // more than limit bytes decoded; the prefix is returned
    PartialCorrupt                     // a non-empty prefix, then a read error (the tail is corrupt)
    NotDecoded                         // a compressed body that produced no decoded bytes, or an encoding this package cannot read
)

func Body(h http.Header, body []byte, limit int) ([]byte, http.Header, Completeness, error)
```

`TruncatedAtCap` is `len(out) > limit` **before** the truncation, so a body that exactly fills the cap
is `Complete` and **no false positive remains** — this replaces the cap detection outright, and it is
the case the withdrawn `len(decoded) == limit` test would have mislabelled. `PartialCorrupt` is the
non-empty-prefix-plus-read-error case `Body` currently swallows; it is now reachable and assertable
(T4), which is exactly what F2.2 showed the wrap could not make it. **The consumer's behaviour is
unchanged**: its one call site (`consumer.go:299`) takes the new result as `_` and keys off `err`
exactly as today (`err == nil` → use the prefix), because a degraded parse still beats none
(`decode.go:53-56`). So `err` keeps its current meaning (non-nil only when nothing decoded or the
encoding is unsupported) and the new value carries the rest; only the two read-path callers look at
`Completeness`.

**A body with no `Content-Encoding` is `Complete`, not `NotDecoded` (F6.1).** `Body` returns **early**
for an unencoded body — ahead of its cap logic and before any read (`decode.go:63-65`:
`if len(encs) == 0 { return body, h, nil }`) — and this is the common case for a plain or
SSE-streamed response, the tool's central case. Because the early return decodes nothing, it is easily
misread as `NotDecoded`; the mapping is therefore **pinned here** rather than left to the implementer:
the no-`Content-Encoding` early return yields `Complete`, the bytes are already plaintext, and the read
path draws **no** marker — `RespBodyDecoded` equals `RespBody` and the detail section renders normally.
`NotDecoded` therefore always means a *compressed* body that produced no decoded bytes (or an encoding
this package cannot read), never a body that had nothing to decode. This is the same invariant the
cap-not-configured guard enforces from the other side (F4.4): a body that a reader can read — plain, or
successfully decoded — is never labelled *"would not decompress"*.

**Fail-open.** A **compressed** body that does not decode at all comes back **raw** with `NotDecoded`
(and an encoding this package cannot read keeps `Body`'s existing "return body unchanged" behaviour) —
an unencoded body never reaches this path; it is `Complete` (F6.1) — so the
marker is rendered rather than the request failing. A malformed or unexpectedly-encoded body must not
turn a working detail view into a 500. This mirrors the fail-open posture everywhere else in the
repo.

**Replay is unaffected.** Replay is unaffected because decoding is **display-only and never rewrites
the stored bytes**: `replay.go:86-96` and `:166-178` send `orig.ReqBody` / `orig.ReqHeaders` straight
from the row, so whatever a display-path decode does to the copy it prints has no route back to the
replay. (The earlier draft justified this with a universal — "request bodies are never compressed,
because clients do not compress request bodies" — that is both unnecessary to the argument and
unverified; it is dropped. `clens show --body` still prints the request half **raw** (`show.go:106`),
and this story adds no request-half decode: no compressed request body has been observed, and inventing
behaviour for an unobserved case is scope this story does not need — F1.14.)

**Layering.** `internal/proxy` gains no import. Decode stays where it already is — cold path and
read path. `TestNoBufferingSSE` must stay green, which it will, because nothing on the hot path
changes.

### D3 — The call detail renders headers and bodies, collapsed, escaped, and honest about absence

Extends the GI-5 two-mode detail (`setCallDetail`, `app.js:113-116`, `198-207`) rather than adding a
new view. Inside the existing `<h2>Call {id}</h2>` block, three new sections:

- **Request headers** / **Response headers** — the stored JSON blobs parsed into the existing `kv`
  table shape. `[redacted]` renders as `[redacted]`; it is the redactor's output and should be visible
  as such.
- **Request body** / **Response body** — inside `<details>`, **collapsed by default**, so a 166 KB
  body is not *rendered* until the user expands it. (F3.7: a collapsed `<details>` still parses and
  retains its children — the collapse buys paint, not DOM bytes; the byte-size saving is D1's fetch,
  since the body arrives in the single-row detail fetch either way.) Byte count in the `<summary>`.
  The **response** half renders `RespBodyDecoded` — the decoded bytes, the exact field D2's read path
  lands — which equals the raw `RespBody` when a compressed body could not be decoded
  (`RespBodyCompleteness == NotDecoded`), and also for a body with no `Content-Encoding`, which is
  `Complete` and draws **no** marker — so showing the same bytes as `RespBody` does not by itself mean
  "undecoded" (F6.1); the **request**
  half renders `ReqBody` (the request half is not decoded — D2). Those field names are the contract
  beads 03 (api) and 04 (web) share, so neither can land against a different shape (F3.4).
- **Capture-incomplete marker (F2.4)** — when `CaptureComplete` is false, a visible line reading
  *"incomplete (truncated, or the stream ended early)"*, the CLI's own wording
  (`internal/cli/show.go:78-82`), because `CaptureComplete` is false for **either** cause
  (`types.go:64-66`, `sink.go:41-44`): a body cut at the cap *or* a stream that ended without
  `message_stop`. The marker must not claim the cap unconditionally — it names the cap only when the
  stored body's length equals it (the one case where the cap is knowably the cause) and otherwise
  says the cause is not recorded on the row. Today nothing surfaces this at all, so a truncated
  capture and a complete one look identical.
- **Read-path markers (F1.5/F2.2)** — `decode.Body`'s `Completeness` result, three lines distinct
  from the capture marker above: `TruncatedAtCap` → *"response truncated at the read cap of N
  bytes"*; `PartialCorrupt` → *"response decoded only partially — its tail was corrupt"*; and
  `NotDecoded` → *"response shown undecoded — it would not decompress"*. A fourth line covers the
  unwired cap (F4.4): when `BodyCapBytes == 0` the section reads *"response shown raw — read cap not
  configured"*, selected **before** `RespBodyCompleteness` is consulted, so a decodable body is never
  labelled "would not decompress" merely because the cap was not wired. A partial or undecoded body is
  never presented as complete, because a partial decode that looks complete is a lie about the
  response.

**Escaping.** Bodies are arbitrary bytes from a remote endpoint rendered into `innerHTML`. Everything
goes through the existing `esc()`. A body containing `</script>` or `<img onerror=…>` is a live
injection path into a page that also holds a replay button. This is the one place in the story where
a mistake is a real vulnerability rather than a cosmetic bug, and it gets its own test (T6).

**Empty is not absent.** A `jsonl`-sourced row has no bodies because transcripts have none, not
because capture failed. The detail says *"not captured — transcript source"*, and does not render
empty boxes. Same distinction `sources.go` draws for health.

### D4 — Non-proxy bodies: separate columns, explicit provenance, a migration, and no headers

**This is the largest fork in the story — include it, scoped as its own bead so it can be cut without
disturbing the rest.** Bead 06 is expensive for two named reasons (F1.8, F1.9): it is the only change
that needs a schema migration, and the only one that has to touch the cross-source merge. Both are
spelled out below so the cut-point is explicit rather than discovered mid-implementation.

**What is possible.** §2.5 shows the transcript carries `message.content` with `text`, `thinking`,
`tool_use` and `tool_result` blocks. Extending `internal/jsonlogs`' `message` struct (`dedup.go:25-29`)
with `Content json.RawMessage` and persisting it would give transcript rows real, useful content.

**What is not possible, and must not be faked.** A transcript carries **no headers**. Not redacted —
absent. There is no request line, no status code, no `Content-Encoding`. Any header-shaped UI on a
transcript row would be inventing data.

**Why not reuse `req_body`/`resp_body`.** A transcript excerpt is not a wire capture. It is one
assistant message, not the request that produced it (which is the whole conversation prefix); it
excludes the system prompt, the tool schemas, and everything the proxy sees. Writing it into `req_body`
would make a reconstruction indistinguishable from a capture in the same column — and the schema's
provenance discipline (`first_source`, `source_refs`, the cross-source merge which *prefers* a non-empty
body) exists precisely to keep those apart. A merge would silently prefer a real capture over a
reconstruction, or worse, the reverse.

**The columns.** Two new nullable columns — `transcript_content BLOB` and `transcript_role TEXT` — plus
the collector change, the merge rules, and a migration. Additive and nullable: an existing row reads
NULL, which means "no transcript reconstruction", never a faked zero.

**The migration — there is no mechanism today, so this bead builds one (F1.8).** `Open` runs the
embedded `schema.sql` and nothing else (`store.go:74-77`); that file is `CREATE TABLE IF NOT EXISTS`
only (`schema.sql:1-3`) and never adds a column to a live database. GI-1 recorded "Migrations | None in
v1" as a *decision*, not an omission (`GI-1-claude-lens-v1.md:1116`) — this story is the first to need
one, so it specifies it:

- A `PRAGMA user_version`-keyed migration runner in `internal/store/store.go`.
- **The two columns get ONE SQL home, and the fresh path cannot double-apply the ALTER (F2.3/F3.6).**
  The round-1 draft put the columns in `schema.sql`'s `CREATE TABLE` *and* left the runner applying
  every migration above `user_version` — which is `0` on a brand-new file, so a fresh install would run
  `ALTER TABLE events ADD COLUMN transcript_content BLOB` against a table that already had the column
  and fail with `duplicate column name`. Resolved by giving the columns one SQL home (schema.sql's
  `CREATE TABLE`), leaving the runner the **only** `ALTER`, and letting the version comparison — never
  the schema exec — decide whether the upgrade runs (F3.6):
  - `schema.sql`'s `CREATE TABLE events` carries `transcript_content BLOB` and `transcript_role TEXT`
    — a fresh install gets them whole. The file carries **no** `PRAGMA user_version` line.
  - **`Open` always runs `db.Exec(schemaSQL)` (F3.6).** Every statement in it is `IF NOT EXISTS`, so
    the exec is a no-op against an existing database and *heals* a partial one: a first `Exec` that
    died mid-file (disk full, power loss between statements — the exec is not atomic) leaves missing
    tables and indexes, and the unconditional exec is what today repairs them on the next open. This is
    why the round-2 "skip the schema when `events` exists" mechanism is **withdrawn**: skipping it on a
    database whose `events` table exists but whose `sessions`/`prices`/`ingest_state` tables do not
    would strand every missing table behind `no such table` on every boot, silently.
  - **The probe decides only whether to stamp, not whether to heal; the fresh stamp is written *before*
    the exec (F4.1).** Before the exec, `Open` probes whether `events` already exists. **Absent** → a
    fresh file: `Open` writes `PRAGMA user_version = <current>` **first**, *then* runs `db.Exec(schemaSQL)`,
    which creates the whole current shape (columns included), so the runner applies nothing. The order is
    load-bearing, not cosmetic: the exec is not atomic, and a first exec on a **new** file can die between
    `CREATE TABLE events` (which now carries `transcript_content`/`transcript_role`) and a later
    `CREATE TABLE`. With the stamp already written, `user_version` reads the current version when the
    process dies, so the next boot's runner applies nothing and the always-run exec heals the missing
    table. Seeded *after* a successful exec instead, `user_version` would still be `0` at the crash; the
    next boot would see `events` present (not fresh → no stamp), heal the missing table, then read `0`
    and apply migration 1 — `ALTER TABLE events ADD COLUMN transcript_content BLOB` against a table that
    already has it — `duplicate column name`, returned by `Open` (`store.go:74-77` returns before
    anything else can run) on **every** boot thereafter, with no recovery but deleting the database.
    Seeding early is also safe in the other direction: if the opening stamp write landed but the very
    first `CREATE TABLE` did not, the next boot's probe still reads `events` absent, so it re-seeds and
    re-runs the exec. **Present** → the exec is a no-op for `events`, `user_version` still reads `0` (a
    pre-change database), and the runner applies migration 1 and stamps `1`. The exec always runs; only
    the starting version differs.
  - The runner sits **before `Open` returns the `*Store`**, so no caller observes a database with the
    schema but not the migration. This is the arrangement in which a fresh install, a pre-change install,
    and a partially-created database of **either** kind (missing a later table, or created *with* the
    new columns before a crash) all end with the two columns exactly once, and a partial database is
    healed rather than stranded.
- The migration itself — the **upgrade** path, and now its only home: `ALTER TABLE events ADD COLUMN
  transcript_content BLOB` and `ALTER TABLE events ADD COLUMN transcript_role TEXT`, as migration
  number `1`, in its own transaction, with `user_version` bumped to `1` after it. Both nullable, both
  additive — no table rewrite, which is the point on the live 41 MB / 81k-row store.
- `eventWriteColumns`/`eventWriteArgs` (`merge.go:14-25`, `:27-41`) and `eventSelectColumns`/`scanEvent`
  (`store.go:983-1020`) gain the two columns, and `Event` (`types.go`) gains the two fields.
  `summaryOmittedColumns` gains both names, so the list `SELECT` excludes them and the list path stays
  transcript-BLOB-free (F3.3).

**The merge rule, and the ordering that is broken without it (F1.9).** `mergeEvents` builds the result
as a **copy of the existing row** (`merge.go:145`) and copies a column from the incoming side only *per
explicit rule* (`:246-266`). A new column with no rule is silently dropped from the incoming side. The
common ordering here is proxy-first: the capture is written live, the transcript reconstruction arrives
minutes later as the incoming side — and with no rule its content is discarded, which is exactly what
T6 as first written would have asserted does *not* happen. So the two new columns get backfill rules
alongside the existing body backfills (`merge.go:261-266`, the `if len(existing.X) == 0` precedent):

```go
if len(existing.TranscriptContent) == 0 {
    merged.TranscriptContent = incoming.TranscriptContent
}
merged.TranscriptRole = preferNonEmpty(existing.TranscriptRole, incoming.TranscriptRole)
```

`transcript_content` is a BLOB, so it takes the same `len(...) == 0` form the body backfills already
use (`merge.go:261-266`); `preferNonEmpty` is `func(string, string) string` (`merge.go:271-276`) and
applies to the TEXT `transcript_role` only. The round-1 draft's symmetric `preferNonEmpty` pair did
not compile against the BLOB (F2.5).

**When a capture and a transcript reconstruction disagree.** The two columns are structurally
transcript-only: only `internal/jsonlogs` ever writes them, so a proxy row's value is always empty and
"disagreement" is not reachable for *these* columns. The rule is the merge's general one — the existing
(first-written) side keeps its value and only an empty existing cell is backfilled — so a reconstruction
never overwrites a reconstruction, and a capture (which has no value here) never competes. Stated so a
reader does not have to re-derive it, and so a future column that *is* written by both sides is a
deliberate decision rather than an accident of the copy.

**Provenance in the UI.** The detail view renders transcript content under an explicit *"reconstructed
from transcript — not a wire capture"* label, and never as "request body".

**Cost and ceiling, stated honestly.** A schema migration runner (new machinery), a collector change, a
merge rule, a UI branch, and a new fidelity concept in the docs. The ceiling: transcript content is
per-message, so "the request" for a transcript row is a reconstruction of intent, not a thing that
exists on the wire. If the user would rather have a clean "proxy gives bodies, transcripts give tokens"
split, this is the bead to drop — and dropping it is a defensible answer, not a gap. If it is dropped,
the migration runner is dropped with it, because no other bead changes the schema.

### D5 — The mode indicator reports configured **and** observed, because the disagreement is the signal

**What "mode" means.** Two independent facts:

| | Question | Source |
|---|---|---|
| **Configured** | Does Claude Code's `ANTHROPIC_BASE_URL` point at this process's `ProxyAddr`? **Three-valued** — match, mismatch, or **unknown** when `settings.json` has no `ANTHROPIC_BASE_URL` at all (F3.5). | `settings.json` via `readSettingsBaseURL` (`doctor.go:252-266`) |
| **Observed** | Has a proxy-sourced row arrived recently? | a store read of the newest `events.started_at` where `source='proxy'` |

**The observed source does not exist today (F1.6).** `/api/sources` reports three collectors and never
`proxy` (§2.6), so the signal is built, not reused. The candidate is a store read —
`SELECT MAX(started_at) FROM events WHERE source='proxy'` — surfaced as a store method and exposed
through the mode seam. The consumer's `LastWriteAt` on `/api/health` (`api.go:622`) is **not**
acceptable: it counts any source, so a transcript-only install would read as "receiving".

**"Recently" has a number (F1.6).** `proxyRecentWindow = 5 * time.Minute`. Observed is true iff a
`source='proxy'` row's `started_at` is within that window of now. Without a stated window the badge has
no boundary and the "receiving" state is untestable.

**Why both.** Neither alone catches the case that produced this story. A configured-only indicator
says "pointed correctly" while the proxy is dead. An observed-only indicator says "capturing" while
the client is pointed elsewhere and the user wonders why their latest session is missing. The
interesting state is **disagreement**, and today's real incident — proxy healthy, client pointed at
port 8787 — is exactly `configured ✗ / observed ✗`.

**The comparison is normalized, or it fails on the tool's own output (F1.7).** `clens serve` prints
`export ANTHROPIC_BASE_URL=http://127.0.0.1:8797` (`serve.go:335`), while `ProxyAddr` is the bare
`127.0.0.1:8797` (`config.go:54`) and `readSettingsBaseURL` returns the raw settings string with no
parsing (`doctor.go:252-266`). A string equality check therefore reports the bad state for a
correctly-pointed client — the one thing D5 exists to get right. The comparison normalizes both sides
before comparing:

- strip a leading scheme (`http://`, `https://`) if present;
- strip any path and trailing slash;
- compare host:port;
- decide `localhost` explicitly: normalize the host `localhost` to `127.0.0.1` before comparing, so the
  two spellings of loopback match. (A bare host with no port is a mismatch — the port is the signal.)

**Where it renders.** A badge in the dashboard header, present on every tab, so the answer does not
depend on the user knowing to look at the Sources tab. **Configured has three values, not two
(F3.5).** `readSettingsBaseURL` returns `("", false)` for an absent/unparseable `settings.json` *or* an
absent `ANTHROPIC_BASE_URL` (`doctor.go:252-266`), and the tool's own onboarding instructs the shell
form — `clens serve` prints `export ANTHROPIC_BASE_URL=http://127.0.0.1:8797` (`serve.go:335`), which
`settings.json` never sees. That case is **unknown**, not "pointed elsewhere": a user who followed the
banner runs a healthy proxy and must never be told *"client elsewhere"*. **"Client elsewhere" is
reserved for a definite mismatch** — a base URL that is present and does not match. The grid:

| Configured | Observed | Badge |
|---|---|---|
| match | ✓ | `proxy: active` |
| match | ✗ | `proxy: configured, not receiving` |
| mismatch | ✓ | `proxy: receiving, client elsewhere` |
| mismatch | ✗ | `proxy: off` |
| unknown | ✓ | `proxy: receiving — base URL not set in settings.json` |
| unknown | ✗ | `proxy: not receiving — base URL not set in settings.json` |

**The seam, and why (F1.13).** `internal/api` cannot reach `readSettingsBaseURL` — not because a guard
forbids `internal/cli` (it does not: `importguard_test.go:31-38` bans `secret`, `config` and `ingest`,
not `cli`), but because the edge is a **build cycle**: `internal/cli/serve.go:14` already imports
`internal/api`, so the reverse import does not compile. The settings read is therefore injected.
`SetSourceHealth` (`api.go:140`) is the **shape** precedent — a function-value seam set at the
composition root — not the *constraint* precedent; its own constraint is the `internal/ingest` import
ban. A `SetProxyMode(func(ctx) (ProxyMode, error))` injected from `internal/cli/serve.go` follows that
shape. `ProxyMode` carries the **three-valued** configured result (match / mismatch / unknown —
F3.5) alongside the observed bool, so the badge can render the unknown row without the API package
ever reaching `readSettingsBaseURL`.

**The badge label is computed server-side, so it is testable in Go (F4.2).** The six-row grid above is
a pure function of `(configured, observed)`, so `ProxyMode` carries the rendered `Badge` **label** — the
mode route returns it and the browser prints the string it is handed. The state→label mapping lives in
`internal/api/mode.go`, not in `app.js`: `app.js` has no runtime in this toolchain, so a mapping there
would be assertable only by a source-shape guard (the ceiling T6 names) and T7 would assert a string
nothing could produce. With the mapping in Go, T7's six cases are behavioural — each
`(configured, observed)` fixture asserts the exact `ProxyMode.Badge` string, the two `unknown` rows
reading `not set in settings.json` and never `client elsewhere`. The browser's only job is to render the
label it is given, which the T6-style source-shape guard on `app.js` covers alongside the escaping rule.

**Why not just make `client_config` fail in doctor.** Because `doctor` is a CLI invocation and the
dashboard is where a user actually sits. The check should be surfaced where it is looked at. Whether
`doctor` should also stop reporting `[PASS]` unconditionally is a fair question and is noted in §8.

### D6 — What is deliberately not changing

- **Redaction.** Headers stay redacted before the tee. Bodies are not redacted, by design — full
  bodies *are* the asset this product protects (CLAUDE.md). Nothing here widens what is stored; it
  widens what is *rendered*, and the bytes were already reachable via the API and `clens show`.
- **The 256 KB cap.** Unchanged. It becomes *visible* (D3's truncation marker) rather than changed.
- **The hot path.** No parsing, no decompression, no new work on the client's goroutine.
- **Listeners.** Proxy loopback, dashboard as configured. `~/.clens/config.toml` is the user's
  documented choice and is not touched.
- **Retention and `clens purge`.** Untouched.

---

## 4. Files changed

| File | Change |
|---|---|
| `internal/store/types.go` | `EventSummary` struct (new; the scalar block, **no** header/body/transcript fields — F3.3); `Event` **embeds** `EventSummary` plus the four header/body fields and D4's two transcript fields (one home for the ~40 scalars — F3.2); `Event` gains `TranscriptContent`/`TranscriptRole` — D4 |
| `internal/store/store.go` | `summarySelectColumns` + `scanEventSummary`; `summaryOmittedColumns` (the named exclusion list, extended by bead 06 — F3.3); `ListEvents` returns `[]*EventSummary`; new `ListEventsFull` (the four full-row callers — F3.1/F5.2); new `SessionEventsSummary` (the session route only — `SessionEvents` itself stays full, F2.1); the newest-proxy-row read for the mode seam — D5; the `PRAGMA user_version` migration runner in `Open`, with `db.Exec(schemaSQL)` **always** run, the fresh-path stamp written **before** that exec, and the pre-exec probe deciding only whether to stamp — D4/F3.6/F4.1 |
| `internal/api/api.go` | List route encodes `[]*EventSummary`; session route's `Calls` becomes `[]*store.EventSummary` and calls the new `SessionEventsSummary` — D1/F2.1; the package `Store` interface drops `SessionEvents` (nothing else in the package uses it) and gains `SessionEventsSummary`, **gains `ListEventsFull`, and retypes `ListEvents` to `[]*store.EventSummary`** — both replay-poll callers go through the interface (`newestReplay` `api/replay.go:224`, `awaitReplayRow` `:249`, which §4 moves to `ListEventsFull`), so the interface must declare the full method too — F2.1/F5.1; detail route decodes response bodies and `eventDetail` gains the fields D3 renders — `RespBodyDecoded []byte` (the decoded response bytes), `RespBodyCompleteness decode.Completeness` (its `NotDecoded` value means the bytes are the raw `RespBody`), and `BodyCapBytes int` (its `0` value means the read cap is unwired, never a zero-byte cap) — D2/F2.8/F3.4; `SetBodyCapBytes` seam, which **ignores a non-positive `n`** and is called on the decode path **only for a positive cap**, so an unwired or zero cap serves the raw body under an explicit cap-not-configured state and can never produce the `NotDecoded` "would not decompress" line for a decodable body — D2/F4.4 |
| *(unchanged — D1/F2.1)* | `internal/analyze/analyze.go`, `internal/analyze/rules.go` — both keep `SessionEvents` returning `[]*store.Event`, so nothing here moves: `ruleCacheInvalidatedByTools` (`rules.go:306-327`) keeps reading `prev.ReqBody`/`cur.ReqBody`, and the `SessionRule`/`Store` interfaces (`consumer.go:39`, `:141`; `jsonlogs.go:35`, `:46`) are untouched. Named explicitly because the round-1 draft would have retyped `SessionEvents` and cascaded into all four files |
| `internal/replay/replay.go` *(unchanged — D1/F3.1)* | `OutcomeOf` keeps its `*store.Event` parameter (`replay.go:36`), because `newestReplay`/`awaitReplayRow` stay on `ListEventsFull`. The **production** file does not move; its own `replay_test.go` literals that set promoted scalars (`:26`, `:64`, `:69`, `:79`) do take the mechanical wrap, while `edit_test.go` (no `store.Event` literal) does not — §4 names the file so the contract is complete rather than implied (F4.5) |
| `internal/cli/replay.go` | `displayModel(orig)` becomes `displayModel(&orig.EventSummary)` (`:162`) — F3.2; `replayOutcome`/`getEvent`/`OutcomeOf` unchanged — F3.1 |
| `internal/consumer/consumer.go`, `internal/jsonlogs/jsonlogs.go`, `internal/cli/stats.go` | the three production `&store.Event{…}` literals that set now-promoted scalar fields get the mechanical `EventSummary: store.EventSummary{…}` wrap the embed needs — `consumer.go:398`, `jsonlogs.go:381`, `stats.go:80` — F3.2. No signature moves |
| `internal/api/mode.go` *(new)* | `ProxyMode` type, the `SetProxyMode` seam, the mode route, and the six-state → badge-**label** mapping (server-side, so T7 asserts it in Go — F4.2) — D5 |
| `internal/decode/decode.go` | `Body` gains the `Completeness` result (`Complete`/`TruncatedAtCap`/`PartialCorrupt`/`NotDecoded`), computed where the `limit+1` read can distinguish the cases; the one other **non-test** call site (`consumer.go:299`) ignores it. No new wrapper — D2/F2.2 |
| `internal/decode/decode_test.go` | the ten `Body(…)` call sites (`:71`, `:95`, `:112`, `:132`, `:153`, `:168`, `:173`, `:180`, `:190`, `:202`) take the new fourth result — the signature change breaks every one, so the file moves with `decode.go` (F4.3); `:202`'s corrupt-tail case is the natural home for the `PartialCorrupt` unit assertion, alongside T4's API-level coverage |
| `internal/cli/show.go` | `--body` decodes the response half via `decode.Body`; prints the undecoded/truncated marker — D2; its `displayModel`/`statusCell` calls pass `&ev.EventSummary` (`:61-62`) — F3.2 |
| `internal/cli/serve.go` | `checkRedaction` switches to `ListEventsFull` — D1; wire `SetProxyMode`, `SetBodyCapBytes` — D2/D5 |
| `internal/cli/export.go` | Both reads (`:90`, `:131`) switch to `ListEventsFull` — D1 |
| `internal/cli/ls.go`, `tail.go`, `stats.go`, `purge.go`, `format.go` | take `[]*store.EventSummary` in their **table** paths (metadata-only) — D1; **`ls.go`'s `--json` branch is the exception and reads through `ListEventsFull`** (`:55-62` encodes the whole row, so its documented output — every `store.Event` key, the four header/body fields included — must not change with the list retype) — D1/F5.2; `statusCell` (`ls.go:105`) and `displayModel` (`format.go:230`) take `*store.EventSummary` so one definition serves both halves, and `printNew` (`tail.go:84`) is retyped with the page — F3.2 |
| `internal/quota/quota.go` | takes `[]*store.EventSummary` (metadata-only) — D1 |
| `internal/api/replay.go` | `newestReplay` / `awaitReplayRow` stay on `ListEventsFull` and keep their `*store.Event` signatures — the row flows to `replay.OutcomeOf` at `api/replay.go:142` — D1/F3.1 |
| `internal/web/app.js` | Header tables, collapsed bodies, truncation/read-path markers, cap-not-configured marker, "not captured" state, and rendering the **server-supplied** mode label (the mapping itself is in `mode.go` — F4.2) — D3/D5 |
| `internal/web/index.html` | Badge element — D5 |
| `internal/web/style.css` | `details`/badge styles — D3 |
| `internal/web/assets_test.go` | T6's source-shape guard |
| the `internal/api` test files | T1 (list route) and T2 (session route) — the payload-size guards; `replay_test.go` moves with the retype — its `capture` (`:80`) and `recordedReplay` (`:129`) are declared `*store.Event` and take `rows[0]` from `f.st.ListEvents` (`:97`, `:131`), and those rows are read as **bodies** (`row.ReqBody` at `:179`, `:198`), so both move to `ListEventsFull`/`*store.EventSummary` — F5.1 |
| `internal/store/store_test.go` | T1's `TestSummaryColumnsAreTheFullSetMinusBodies` — both column lists are `package store` internals (`store.go:983-994`), so this is the only package it can live in (F2.6) — and T9's migration cases (**fresh + pre-change + partial + partial-new-schema**, F3.6/F4.1) — D4/F2.3 |
| the `store.Event{…}` composite literals across the test fixtures | the mechanical `EventSummary: store.EventSummary{…}` wrap the embed needs, in `rules_test` (whose **elided** `[]*store.Event{…}` elements count too), `api_test`, `broker_test`, `cli_test`, `additions_test`, `session_test`, `quota_test`, `reconcile_test`, `store_test`, `internal/replay/replay_test.go`, and `internal/cli/replay_test.go` — F3.2/F4.5. `analyze_test` (literals are `&store.Event{}`/`&store.Event{ReqBody: …}` only) and `jsonlogs_test`'s own literal (a bare `map[string]*store.Event{}`, no field list) are **not** in the composite-literal list: neither sets a promoted scalar, so neither *literal* moves (F4.5 — but see the next row: `jsonlogs_test`'s **helper** does move). A fixture that sets only one of the six direct columns (`&store.Event{ReqBody: …}`) is unchanged |
| the `ListEvents`-derived **test helpers** | the three helpers that declare their type *from* `ListEvents` and therefore move even where no literal does: `internal/consumer/consumer_test.go`'s `waitForEvents` (`:536`, `[]*store.Event` returned from `st.ListEvents` `:540`), `internal/jsonlogs/jsonlogs_test.go`'s `eventsByRequestID` (`:37`, `map[string]*store.Event` from `st.ListEvents` `:39` — its **literal** is unchanged, its **return type** is not), and `internal/api/replay_test.go`'s `capture`/`recordedReplay` (named in the api test row above). Each takes `[]*store.EventSummary`/`*store.EventSummary`, and the two that read a body (`capture`/`recordedReplay`, via `row.ReqBody`) move to `ListEventsFull` — F5.1. Stated because the round-4 §4 sentence called `jsonlogs_test` a file that "does not move": true of the literal, false of the helper |
| the `internal/cli` test files | T3 (redaction, plus `TestLsJSONKeepsBodyFields`) and T5 |
| **D4 only** | |
| `internal/store/schema.sql` | `transcript_content BLOB`, `transcript_role TEXT` in `CREATE TABLE events` (the fresh shape), and the file **always** runs on every `Open` — its `IF NOT EXISTS` DDL heals a partial database (F3.6). **Not** a `PRAGMA user_version` line and **not** the `ALTER` statements: the version is written by the runner (and seeded on the fresh path), so each piece of SQL has one home — F2.3/F3.6 |
| `internal/store/merge.go` | `eventWriteColumns`/`eventWriteArgs` and the `mergeEvents` backfill rules gain the two transcript columns — the BLOB via the `len(...) == 0` form, the TEXT role via `preferNonEmpty` (D4/F2.5) |
| `internal/jsonlogs/dedup.go` | `message.Content json.RawMessage` |
| `internal/jsonlogs/jsonlogs.go` | Persist content on the transcript path |
| **Docs** | |
| `docs/context/dashboard.md` | The Calls detail D3 rewrites, the `esc()` rule, the mode badge — **Phase 5.6** (generated tree) |
| `docs/context/api-surface.md` | `/api/requests` projection change, the new `/api/sessions/{id}` projection, the mode route — **Phase 5.6** |
| `docs/context/storage-schema.md` | The two new columns and the migration runner (D4 only) — **Phase 5.6** |
| `docs/context/INDEX.md` | `:40`'s "838 lines" for `app.js`, stale the moment D3/D5 land — **Phase 5.6** |
| `README.md` | The Calls-tab row (`:151`) and the bodies claim (`:241-244`). **Outside** the generated tree — nothing else repairs it, so it is named explicitly |
| `docs/planning/GI-7-header-and-body-visibility.md` | New §10: the recorded manual run |

No new dependency. No new build step. No change to `internal/proxy`.

Phase 5.6's REFRESH pass owns the four `docs/context/` rows — that tree is generated. `README.md` and
this plan doc are **outside** the generated tree, which is exactly why the refresh cannot reach them
and why they are named here (F1.11).

---

## 5. Test strategy

**T1 — the list path selects no body columns, and the payload proves it.** `TestListRouteOmitsBodies`
(`internal/api`): seed 50 rows each carrying a 1 MB body (50 MB stored), and assert the encoded
`GET /api/requests?limit=50` response is **under 64 KB and contains no `ReqBody`/`RespBody`/`ReqHeaders`/
`RespHeaders` key**. `EventSummary` has no such field, so the JSON cannot carry the key; the size bound
is the regression guard for a future "just add it back", and the fixture is sized so the bound is not
vacuous — a leaked body would blow 50 MB past 64 KB. A companion `TestSummaryColumnsAreTheFullSet-
MinusBodies` (`internal/store`) asserts the summary `SELECT` list equals `eventSelectColumns` **minus
the named `summaryOmittedColumns` list** — four columns today, **six** once bead 06 adds the two
transcript columns (F3.3) — so the assertion tracks the list instead of being edited by bead 06, and a
summary that quietly selected a transcript BLOB fails here rather than mis-scanning positionally at
runtime. (This test, not T1's size bound, is the guard for the transcript columns: T1's fixture seeds
request/response bodies, not transcript content, so it would not notice a transcript BLOB on the list
path.)

**T2 — the session route is bounded the same way (F1.3/F2.7).** `TestSessionRouteOmitsBodies`
(`internal/api`): a fixture of **N = 50 calls, each carrying a 1 MB body** (the same 50 MB fixture
T1 uses), fetch `/api/sessions/{id}`, and assert **both** halves: `calls[]` carries no
`ReqBody`/`RespBody`/`ReqHeaders`/`RespHeaders` key, and `len(calls) == 50` **and** the encoded
response is **under 64 KB**. The length assertion is the half F2.7 caught: with `Calls` typed as
`EventSummary` the "no body key" half is guaranteed by the type checker and catches nothing new, so
the size bound is the only live guard — and it must be paired with a non-empty `calls[]` so an empty
array cannot satisfy it vacuously. The same defect gets the same guard as T1, for the same reason:
the session route is the larger per-call payload and must not be left unbounded beside the fixed
list route.

**T3 — the redaction self-test still sees headers, and `ls --json` keeps its bodies (F1.1/F5.2).**
`TestCheckRedactionStillSeesHeaders`
(`internal/cli`): seed one row whose `req_headers` carries an un-redacted credential, run
`checkRedaction`, and assert it reports the finding. `checkRedaction` now reads through `ListEventsFull`;
on the summary path it would `continue` on every row and report zero findings, always, silently — this
is what keeps that from being the default. Companion `TestLsJSONKeepsBodyFields` (`internal/cli`): seed
a proxy row with non-empty `req_headers`/`resp_headers`/`req_body`/`resp_body`, run `runLs(["--json"], w)`,
decode the emitted JSON, and assert all four keys are present and equal to the stored values. `ls --json`
is the one caller whose whole-row encode would *silently* lose the four fields on the summary type
(`ls.go:55-62`) — preserving its output is exactly why its `--json` branch stays on `ListEventsFull`
(D1), and this test is what keeps that from regressing back to the summary.

**T4 — the detail route decodes a compressed response, and falls back on a corrupt one (F1.5/F2.2).**
`TestDetailDecodesResponseBody`, `TestDetailFallsBackToRawOnCorruptBody`, and the completeness cases
`TestDetailMarksCapTruncatedBody` / `TestDetailMarksCorruptTailBody` / `TestDetailUnencodedBodyIsComplete`
(`internal/api`): a brotli-encoded
stored body comes back plaintext with `Completeness == Complete`; a body that decodes to **exactly**
`BodyCapBytes` comes back `Complete`, **not** `TruncatedAtCap` (the case the withdrawn
`len(decoded) == cap` test would have mislabelled — F2.2); a body that expands past the cap comes back
`TruncatedAtCap` with its marker; a body whose tail is corrupt after a clean prefix comes back
`PartialCorrupt` with its own marker — reachable now that `decode.Body` returns the signal it used to
discard; and a body that is not valid brotli comes back raw with `NotDecoded`, a marker, and a 200
rather than a 500. Plus the **unencoded** case (F6.1): a stored plain body with **no**
`Content-Encoding` and a positive `BodyCapBytes` comes back `Complete` — `RespBodyDecoded` equal to
`RespBody`, **no** read-path marker, and **never** the *"would not decompress"* text — so the tool's
central case, a plain or SSE-streamed response, cannot render a decode-failure marker. Plus the
**unwired-cap** case (F4.4): a handler with no `SetBodyCapBytes` (and one
whose cap was set to `0`) returns the raw response body with `BodyCapBytes == 0` and the
cap-not-configured marker, and **never** the `NotDecoded` *"would not decompress"* text — the guard
against `decode.Body(…, 0)`'s error (`decode.go:66-68`) being surfaced as a decode failure.

**T5 — `clens show --body` prints a decoded response.** `internal/cli`: the CLI and the API agree on
the same stored row, including the cap-truncated marker.

**T6 — a body cannot inject into the dashboard (a source-shape assertion, not behavioural).**
`internal/web/assets_test.go`, alongside GI-5's `TestAssetsTheCallDetailReplacesTheList`: slice the
body-rendering region out of `app.js` (the `funcBody`/anchor technique the file already uses) and assert
it wraps the body in `esc(`, with the found-and-non-trivial vacuity guard first (the slice must be
non-empty and contain the body variable), plus the mutation check: delete the `esc(` and confirm this
test, and only this test, fails. **State the ceiling:** there is no JS runtime in the toolchain — no
dependency is a JS engine, and the third-party modules the code actually imports are Go libraries
(`modernc.org/sqlite`, `klauspost/compress`, `andybalholm/brotli`);
`assets_test.go` is regex/text over the embedded bytes), so this proves the escaping *call is present in
the source*, not that the rendered pixels are safe. That is the strongest guarantee the repo's
no-build-step, no-browser-automation posture allows, and it is named rather than implied (F1.10).

**T7 — the mode matrix (F4.2).** Six cases over the seam — configured ∈ {match, mismatch, **unknown**} ×
observed ∈ {✓, ✗} — the four original badge rows plus the two unknown-configured rows F3.5 adds. Each
case asserts the exact `ProxyMode.Badge` **label** the route returns, so this is a behavioural test in
`internal/api`, not a string with no producer: the state→label mapping is server-side (F4.2), and no JS
runtime is needed. The "match" fixture is the exact string `clens serve` prints — `http://` + `ProxyAddr`
(`serve.go:335`), i.e. `http://127.0.0.1:8797` — so the normalization is exercised on the tool's own
output, not on a hand-written pair that happens to match (F1.7). The "unknown" fixture is a
`settings.json` with no `ANTHROPIC_BASE_URL`; the assertion is that the label reads the `not set in
settings.json` line and **never** *"client elsewhere"*, which is reserved for a definite mismatch. Also
assert the store-read window: a proxy row older than `proxyRecentWindow` reads as not-receiving.

**T8 (D4 only) — transcript content lands in its own columns and never in `req_body`, in both orderings.**
A jsonl fixture with content, plus a merge case **both ways**: JSONL-first (the reconstruction is
`existing`, the proxy capture arrives as `incoming`) and proxy-first (the capture is `existing`, the
reconstruction arrives as `incoming`) — the second being the common ordering and the one the design as
first written silently dropped (F1.9). Assert the two new columns fill from whichever side has them and
neither side's content is lost.

**T9 (D4 only) — the migration runner, four paths (F1.8/F2.3/F3.6/F4.1).** `internal/store`:
(a) **fresh** — `Open` a path that does not exist and assert it succeeds with no `duplicate column
name` error, that the `events` table carries both columns, and that `user_version` is the current
version. This is the case the round-1 draft would have failed, because its runner would have
re-`ALTER`ed a table `schema.sql` had already created with the columns. (b) **pre-change** — open a
file carrying the pre-migration `schema.sql` (no transcript columns, `user_version == 0`), run `Open`,
and assert the two columns now exist and `user_version` was bumped — the "existing database" case
`schema.sql`'s `CREATE TABLE IF NOT EXISTS` cannot satisfy (F1.8). (c) **partial** (F3.6) — a database
whose `events` table exists but which is missing another table (a first `Exec` that died mid-file),
run `Open`, and assert the missing table is created (the schema exec still heals) **and** the two
columns are still added exactly once (the pre-change version path still runs) — the case the
withdrawn "skip the schema when `events` exists" mechanism would have stranded.
(d) **partial-new-schema** (F4.1) — the case the fresh-stamp ordering exists for, and the one case (c)
does not cover: a database built by executing the *current* `schema.sql` up to and including
`CREATE TABLE events` — so `events` already carries `transcript_content`/`transcript_role` — with a
later table absent and `user_version` left at the seeded current version. Run `Open` and assert it
**succeeds, with no `duplicate column name`**, that the missing table now exists, and that the two
columns were not re-added. Built by hand (exec the current schema's `events` DDL into a temp file)
rather than by racing a real mid-file crash, so it is deterministic. Seeded the other way — the fresh
stamp written only after a successful exec — this is the database `Open` could never repair; case (c)
passes either way, because its `events` lacks the columns and the `ALTER` fixes it.

**T10 — the existing gate stays green.** `TestNoBufferingSSE` (`internal/proxy`) and the full suite.
No hot-path change is proposed, so a failure here means the change reached somewhere it should not
have.

---

## 6. Risk areas

| Risk | Mitigation |
|---|---|
| **HTML injection through a body** — the story's one true security surface | T6's source-shape guard; `esc()` on every rendered field; the ceiling (no JS runtime) is named, not implied |
| Decompression bomb on the read path | Reuse `decode.Body`'s existing cap; fail-open to raw; `TruncatedAtCap` on cap-hit (T4) |
| An unwired body cap manufactures a false "would not decompress" marker | `SetBodyCapBytes` ignores a non-positive `n`; the detail route calls `decode.Body` only for a positive cap, and the cap-not-configured line is keyed off `BodyCapBytes == 0` **before** `RespBodyCompleteness`, so `decode.Body(…, 0)`'s error (`decode.go:66-68`) never becomes a decode-failure marker (F4.4, T4) |
| The summary projection silently breaks a caller that wanted a body | The four full-row callers (`checkRedaction`, `clens export`, `clens ls --json`'s whole-row encode, the replay poll's `awaitReplayRow` → `replay.OutcomeOf`) are explicit `ListEventsFull` (F3.1/F5.2); every other caller takes `EventSummary` and cannot name a body field — a forgotten body is a compile error, not a silent nil; the redaction self-test and the `ls --json` keys each have their own test (T3). `SessionEvents` is **not** narrowed — it stays full so the analyzer's body-reading session rule keeps working (F2.1) |
| Two column lists drift (`eventSelectColumns` vs the summary list) | `TestSummaryColumnsAreTheFullSetMinusBodies` asserts the summary list is the full list minus the named `summaryOmittedColumns` — four columns today, **six** after bead 06 (F3.3); a mismatch fails the build rather than mis-scanning positionally, and a transcript BLOB cannot slip onto the list path |
| The `Event`↔`EventSummary` embed reshapes composite literals | Mechanical and compile-checked: three production sites (`consumer.go:398`, `jsonlogs.go:381`, `stats.go:80`) and the test fixtures take a one-line `EventSummary: store.EventSummary{…}` wrap (F3.2); a forgotten one is a build failure, not a silent behaviour change |
| A mispointed badge on the tool's own onboarding path | Configured is three-valued; `unknown` (no `ANTHROPIC_BASE_URL` in `settings.json`) never reads *"client elsewhere"*, which requires a definite mismatch (F3.5, T7) |
| Detail view becomes unusable for a large body | Collapsed `<details>` — the body is not *rendered* until expanded (its children are still parsed into the DOM, F3.7); byte count in the summary |
| Decoding is mistaken for a rewrite of stored bytes | Decode is display-only; replay reads `orig.ReqBody`/`orig.ReqHeaders` straight from the row; stated in D2 |
| **D4's migration on a 41 MB / 81k-row store** | `PRAGMA user_version`-keyed `ALTER TABLE ADD COLUMN`, nullable and additive — no table rewrite; the schema exec **always** runs (healing a partial database) and only the version stamp is fresh-path-only, written **before** the exec so a first exec that dies after `CREATE TABLE events` still leaves the version stamped and the next boot heals instead of re-`ALTER`ing — a fresh install, a pre-change install, and a partially-created new-schema database each end with the columns exactly once (F1.8/F2.3/F3.6/F4.1, T9 a–d); the runner is new machinery and this is the repo's first schema change |
| **D4's merge silently drops the reconstruction** | Explicit backfill rules in `mergeEvents` for the two new columns, both orderings asserted by T8 (F1.9) — the other reason bead 06 is expensive |
| Scope creep across deliverables | D4 is its own bead and is the one to cut; cutting it drops the migration runner too, because no other bead changes the schema |

---

## 7. Self-review

**As a senior engineer.** The work is three small changes with one shared theme, not one large one:
a query projection, a decode call moved to the read path, and a view. The design deliberately reuses
`decode.Body`, the `SetSourceHealth` seam pattern, and GI-5's detail view rather than introducing new
machinery. The one structural question — D1's flag-versus-type trade-off — was resolved in the v2
rewrite: the flag is withdrawn for a distinct `EventSummary` type, because a forgotten body should be
a compile error rather than a silent nil (F1.2/F1.15). The cases that survived the earlier rewrites and
were still wrong are now closed: `SessionEvents` keeps its full signature and gains a separate summary
sibling (F2.1); the decode completeness signal lives in `decode.Body` where it is actually computable
(F2.2); `EventSummary` is the single home for the scalar block — `Event` embeds it, so the two halves
cannot drift — and names no non-scalar column, transcript BLOBs included (F3.2/F3.3); the replay poll
(`awaitReplayRow`) stays on the full path because its row feeds `replay.OutcomeOf` (F3.1), and
`clens ls --json` stays on it because its whole-row encode would otherwise lose the four body/header
keys silently (F5.2); and the schema exec always runs so a partial database heals while the version probe decides only the stamp
(F3.6).

**As a QA engineer.** The gaps that matter are the ones where an absence is indistinguishable from a
failure — a truncated body, a body-less transcript row, a compressed body that will not decode. Each
gets an explicit, visible state rather than an empty box, and each has a test. The payload-size
assertions (T1 and its T2 twin) are unusual but are the only thing that catches a regression that has
no functional symptom.

**As a security engineer.** Bodies are untrusted remote input rendered into `innerHTML`, in a page
holding a replay button that spends money. Escaping is the control and T6 is its proof. Redaction is
unchanged and headers stay redacted. The dashboard binds `0.0.0.0` on this machine by the user's
documented choice; rendering bodies makes existing exposure *more convenient to reach*, not newly
reachable — the API already served them, and that is worth stating plainly rather than leaving
implicit.

---

## 8. Out of scope

- **Changing `client_config` from PASS-only to a real check in `doctor`.** D5 surfaces the signal
  where it is looked at; whether `doctor` should also stop reporting `[PASS]` for a mispointed client
  is a separate, smaller question about CLI semantics, noted here so it is not lost.
- **Redacting bodies.** Deliberate non-goal: full bodies are the product.
- **A body-diff or search view.** Not asked for.
- **Streaming the body to the browser** rather than fetching it whole. The collapsed `<details>` is
  sufficient at these sizes; a range endpoint is machinery this story does not need.
- **Any change to the hot path, the cap, retention, listeners, or `internal/proxy`'s imports.**

---

## 9. Bead sketch (Phase 3 formalises this)

| # | Bead | Depends on |
|---|---|---|
| 01 | `store`+`cli`: `EventSummary` + the `Event` embed (F3.2), `ListEvents` projection, `ListEventsFull` (four full-row callers — `checkRedaction`, `export`, `ls --json`, the replay poll among them; plus the API `Store` interface gains it and retypes `ListEvents` — F3.1/F5.1/F5.2), new `SessionEventsSummary` (`SessionEvents` itself stays full — F2.1), `checkRedaction`/`export`/`ls --json`/`awaitReplayRow` switched to it, `statusCell`/`displayModel` on the summary, the composite-literal wrap, the `ListEvents`-derived test helpers, the column-drift test + T3 | — |
| 02 | `decode`: `Body` returns the `Completeness` result (cap-hit / partial-corrupt / not-decoded made distinct), and the ten `decode_test.go` call sites take the fourth value (F4.3) — F2.2 | — |
| 03 | `api`/`cli`: list + session routes encode summaries; detail route decodes via `decode.Body` and `eventDetail` gains `RespBodyDecoded`/`RespBodyCompleteness`/`BodyCapBytes` (F3.4); `SetBodyCapBytes` with the unwired-cap guard (F4.4); `clens show --body` uses it + T1/T2/T4/T5 | 01, 02 |
| 04 | `web`: header tables, collapsed bodies, truncation/read-path/cap-not-configured markers, "not captured" state + T6 | 03 |
| 05 | `api`/`cli`: `ProxyMode` seam (three-valued configured — F3.5), the newest-proxy-row store read, the mode route returning the server-side badge label (F4.2), the header badge with the `unknown` row + T7 | 01 |
| 06 | *(D4)* the always-run schema exec + the `user_version` runner with the fresh stamp written **before** the exec (the probe decides only the stamp — F3.6/F4.1) + the two columns + the `summaryOmittedColumns` extension (F3.3) + `jsonlogs` content capture + `mergeEvents` rules + T8/T9 (a–d) | 01 |

Beads 01–05 are the story as asked for. Bead 02 is a dependency of 03 because 03's detail route decodes
through `decode.Body` — the earlier sketch had this edge **inverted**, with 03 declared a dependency of
02 while 02's own outcome needed it (F1.12). **06 is the one to cut** if transcript reconstruction is
not wanted; nothing else depends on it. It is expensive for two named reasons — the schema migration
runner (F1.8) and the merge rule (F1.9) — and cutting it removes both, because no other bead changes
the schema or the merge.

---

## 10. Recorded manual run

Performed 2026-09-20 against the branch head, on a **copy** of the live store (`~/.clens/lens.db`,
82.7 MB, 84,765 rows, `user_version = 0`, 45 columns) served on `127.0.0.1:8897/8898` so the running
instance on `8797/8798` was never touched. Where a state needed a controlled input the run used a
redirected `CLAUDE_CONFIG_DIR` — which the tool honours for **both** `settings.json` and the
`~/.claude/projects` transcript root — rather than editing the user's real config.

**The migration, on a real store.** This is the case no unit test can be: the copy is a genuine
pre-change database at production size. `Open` brought it from `user_version 0` to `1`, `events` from
45 to 47 columns, both transcript columns present, and the row count unchanged at 84,765. The ALTERs
are additive, so the migration took the instant the schema exec did.

**The Calls list fetch.** Fifty proxy rows carry 15,325,964 bytes of stored blobs; base64 those and
the pre-projection payload is 20,259,652 bytes. The projected route returns **47,766 bytes** — 424×
smaller, and larger than the ~12 MB the plan estimated, so the projection matters more than it
claimed. The payload is computed as `measured + base64(blobs)` rather than measured against a
pre-change binary; the blobs are the exact set the projection omits. The session drill-down for the
busiest session returns 52,090 bytes on the same projection.

**A real captured call.** A `POST /v1/messages` with a deliberately invalid key went through the
proxy of the copy, so upstream refused it at no cost: row 84766, status 401, `CaptureComplete` true,
`BodyCapBytes` 262144, `RespBodyCompleteness` 0 (`Complete`), no read-path marker — correct, the 401
carries no `Content-Encoding`. Its stored `X-Api-Key` is `["[redacted]"]` and the raw key appears
nowhere in the served row. Both bodies render readable: the request JSON in full, and the response
through the same `bodySection` the page uses, with its byte counts and no marker.

**A truncated capture.** With `-body-cap-bytes 512` and a local stub upstream returning a 2,711-byte
body, row 84770 came back `CaptureComplete` false, `BodyCapBytes` 512, stored response exactly 512
bytes. The page shows *"incomplete (truncated, or the stream ended early) — the stored body is
exactly the 512-byte read cap, so the cap is the cause on this row"*, and draws **no** read-path
marker: the response had no `Content-Encoding`, so it is `Complete`, and the two markers are
correctly independent.

**A transcript row, both states.** `CLAUDE_CONFIG_DIR` was redirected at a synthetic
`projects/demo/demo.jsonl` and `clens ingest` run against the copy, so the rows come from the real
collector rather than an insert. Two assistant lines, one with `message.content` and one without:
row 84771 stores 258 bytes of content with role `assistant` and a NULL `req_body`; row 84772 stores
neither. The page renders the first as *"reconstructed from transcript — not a wire capture — 258
bytes"* with the content and no header tables, and the second as *"not captured — transcript
source"* with no boxes at all. 84,630 pre-existing jsonl rows are in the second state.

**`checkRedaction` at boot.** A row seeded with an un-redacted `X-Api-Key` into the newest 500 rows
made the startup self-test fire, and only that row: `serve: proxy: RedactCheck: header "X-Api-Key" is
not redacted (holds 26 bytes that should not be there)`. This is the check bead 01's retyping of the
read path could have silently disarmed.

**The badge, all six states.** Driven by `CLAUDE_CONFIG_DIR` (three settings.json shapes: pointing at
8897, pointing back at **8787**, and absent) against both the populated copy and a fresh empty
database for `Observed` false:

| configured | observed | badge |
|---|---|---|
| match | yes | `proxy: active` |
| match | no | `proxy: configured, not receiving` |
| mismatch (8787) | yes | `proxy: receiving, client elsewhere` |
| mismatch (8787) | no | `proxy: off` |
| unknown | yes | `proxy: receiving — base URL not set in settings.json` |
| unknown | no | `proxy: not receiving — base URL not set in settings.json` |

The unknown row reads *"not set in settings.json"* in both its variants and never *"client
elsewhere"*, which is the property F3.5 exists for.

### What the run found that no bead covers

**A request body cut at the read cap does not clear `CaptureComplete`.** Row 84769 was sent with a
1,753-byte request body under `-body-cap-bytes 512`: the stored `req_body` is 512 bytes, and the row
reports `capture_complete = 1`. The response-side cut on row 84770 correctly reports false. The cause
is `internal/proxy/proxy.go:79`, which submits `!respBuf.truncated` — the response buffer's flag
only. The request buffer keeps its own `truncated` flag (`proxy.go:249`, set at `:260` and `:265`)
and it is never read.

So the D3 capture marker is reachable for a truncated **response** and unreachable for a truncated
**request**: the page shows a 512-byte prefix of a 1,753-byte request with its byte count and no
marker, which is precisely the "a truncated capture and a complete one look identical" defect this
story exists to close, surviving in the one half the run could not exercise from the response side.

This is out of every bead's file list — `internal/proxy/proxy.go` appears in none of them, and the
package is the hot path CLAUDE.md fences off. It is also not a one-line change in effect: widening
`CaptureComplete` to include request truncation would change when `analyze`'s incomplete-stream rule
fires (`rules.go:176`) and how the cross-source merge prefers one row over another
(`merge.go:159-181`). Recorded here rather than fixed, because which of those two is wanted is a
design decision the plan does not make, and the run's job is to report the state as it is.

## Change History

| Version | Date | Change |
|---|---|---|
| v1 | 2026-09-20 | Initial draft from Phase 1 intake; not yet cross-reviewed |
| v2 | 2026-09-20 | Round 1 review. Adopt `EventSummary` and withdraw the `EventFilter` body flag — a forgotten body becomes a compile error (F1.1/F1.2/F1.15, D1); full nine-caller inventory and a redaction-sees-headers test (F1.1); session-route projection (F1.3); `SetBodyCapBytes` seam (F1.4); decode completeness signal (F1.5); observed-mode source and a numeric recency window (F1.6); base-URL normalization and the tool's own string as T7's fixture (F1.7); `PRAGMA user_version` migration runner, `ALTER TABLE` columns, and `mergeEvents` rules, with both orderings asserted (F1.8/F1.9); T6 restated as a source-shape guard (F1.10); docs rows (F1.11); bead-graph inversion fixed (F1.12); seam reason restated as the import cycle (F1.13); D2 replay framing corrected (F1.14); one mode-route file (F1.16) |
| v3 | 2026-09-20 | Round 2 review. `SessionEvents` keeps its full signature and the session route gets a separate `SessionEventsSummary` (F2.1 — the analyzer's session rule reads `ReqBody`); `decode.Body` returns a `Completeness` result instead of a wrapper inferring one, making the corrupt-tail case reachable and removing the exact-length-at-cap false positive (F2.2); the transcript columns get one home — `schema.sql` creates them whole and stamps `user_version = 1`, the runner ALTERs only a pre-change database (F2.3); D3 mirrors the CLI's "truncated, or the stream ended early" wording (F2.4); the merge snippet uses the `len(...) == 0` BLOB form (F2.5); the column-drift test is placed in `internal/store` (F2.6); T2 names its 64 KB bound (F2.7); `eventDetail`'s response-shape change is named in §4 (F2.8); the column count is corrected to 45 (F2.9) |
| v4 | 2026-09-20 | Round 3 review. The replay poll (`newestReplay`/`awaitReplayRow`) stays on `ListEventsFull` and `internal/replay/replay.go` + `internal/cli/replay.go` are unchanged — the returned row feeds `replay.OutcomeOf(*store.Event, …)`, so retyping it was wrong (F3.1); `Event` **embeds** `EventSummary` so one definition of the scalar block serves both halves, with `statusCell`/`displayModel` on `*store.EventSummary` and the composite-literal wrap named (F3.2); `EventSummary` names no non-scalar column and the exclusion is the named `summaryOmittedColumns` list — six columns after bead 06 (F3.3); `eventDetail`'s decoded-body field is named (`RespBodyDecoded`/`RespBodyCompleteness`/`BodyCapBytes`) (F3.4); the badge's configured half is three-valued with an `unknown` row that never reads "client elsewhere" (F3.5); the schema exec always runs and the probe decides only the stamp, healing a partial database (F3.6); the collapsed-`<details>` reason is corrected — paint, not DOM bytes (F3.7) |
| v5 | 2026-09-20 | Round 4 review. The fresh-path `PRAGMA user_version` seed is pinned **before** `db.Exec(schemaSQL)` (a partial first exec of the new schema would otherwise leave `user_version` at 0 and next boot's runner would re-`ALTER` a table that already has the transcript columns — `duplicate column name` on every boot), and T9 gains case (d) for that partial-new-schema database (F4.1); the badge label moves server-side into `internal/api/mode.go` so T7 asserts the six labels behaviourally in Go rather than against a string no test can produce (F4.2); `internal/decode/decode_test.go` is added to §4 with its ten `Body(…)` call sites taking the new fourth result (F4.3); the unwired/zero `SetBodyCapBytes` state is documented like every other `internal/api` seam and guarded so `decode.Body(…, 0)` is never called, adding a cap-not-configured marker and a T4 case (F4.4); §4's fixture list drops `analyze_test`/`jsonlogs_test` (neither moves) and names `internal/replay/replay_test.go` + `internal/cli/replay_test.go`, with the `internal/replay` row narrowed (F4.5) |
| v6 | 2026-09-20 | Round 5 review. §4 completes the `ListEvents` → `[]*EventSummary` ripple through the interface — `api.Store` gains `ListEventsFull` and its `ListEvents` retypes (the replay poll calls it through the interface at `api/replay.go:224`, `:249`) — F5.1; §4 names `internal/api/replay_test.go` (`capture`/`recordedReplay` are `*store.Event` and read `row.ReqBody` at `:179`/`:198`), `internal/consumer/consumer_test.go` (`waitForEvents`) and `internal/jsonlogs/jsonlogs_test.go` (`eventsByRequestID` — its **literal** is unchanged, its **return type** moves), correcting the round-4 "neither moves" sentence — F5.1; `clens ls --json` is kept on the full read path (`ListEventsFull`) so its whole-row JSON does not lose the four body/header keys — added to D1's caller inventory, §4's `ls.go` row, the §6 risk row and bead 01, with `TestLsJSONKeepsBodyFields` under T3 — F5.2; T6's `go.mod` parenthetical is reworded to the claim actually made (no dependency is a JS engine; the imported third-party modules are Go libraries) — F5.3 |
| v7 | 2026-09-20 | Round 6 review. D2 pins the `Completeness` value for a response body with **no** `Content-Encoding`: `Body`'s early return (`decode.go:63-65`, ahead of the cap logic) yields `Complete`, not `NotDecoded` — `RespBodyDecoded` equals `RespBody` and **no** read-path marker is drawn, so the tool's central case (a plain or SSE-streamed response) can never show the *"would not decompress"* text (F6.1); the `Complete`/`NotDecoded` enum comments are reworded (the `NotDecoded` comment now names a **compressed** body that produced no decoded bytes) so neither can be read as covering the unencoded case (F6.1); the fail-open sentence and the `NotDecoded`-marker invariant are qualified the same way, and D3's "equals the raw `RespBody`" clause no longer reads as an equivalence (F6.1); T4 gains the plain/unencoded case (`TestDetailUnencodedBodyIsComplete` — `Complete`, `RespBodyDecoded == RespBody`, never the marker) alongside the cap-truncated, corrupt-tail and unwired-cap cases (F6.1) |
| v8 | 2026-09-20 | Round 7 review. D2's rationale for injecting the cap no longer asserts a false import edge: `internal/api` does **not** fail to import `internal/consumer` — `api.go:37` carries that import in production code (`consumer *consumer.Consumer`, `api.go:73`) and the guard's ban list is exactly `secret`/`config`/`ingest`, never consumer. The clause is reworded to the two real facts: the configured `BodyCapBytes` lives in `internal/config`, which the guard bans, and the cap's default is an **unexported** constant (`defaultBodyCapBytes`, `consumer.go:24`), so the `consumer` import `api` already holds still buys no reachable value — the cap can only arrive as the injected `SetBodyCapBytes` seam (F7.1) |
| converged | 2026-09-20 | Round 8 review returned `NO_FURTHER_FINDINGS` with zero findings: the round-7 fix (F7.1) verified against source, and no new BLOCKER/MAJOR/MINOR raised. Cross-review loop closed after 8 rounds; plan v8 is the converged plan |
| Phase 5 | 2026-09-20 | §10 filled in with the manual run against a copy of the live store, on the branch head. All eight of §10's items reproduced; one state did not — a request body cut at the read cap leaves `CaptureComplete` true (`proxy.go:79` reads only the response buffer's flag), so the capture marker is unreachable for that half. Recorded there, not fixed: no bead owns `internal/proxy`, and widening the flag changes the analyze and merge behaviour it feeds |
