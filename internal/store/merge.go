package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// eventWriteColumns lists every events column in the fixed order
// insertEventTx and updateEventTx bind — id is excluded (assigned by
// SQLite on insert, unchanged on update).
const eventWriteColumns = `
	request_id, source, source_refs, first_source,
	started_at, ended_at,
	auth_kind, account, billing_mode,
	model_requested, model_resolved,
	input_tokens, output_tokens, cache_write_5m_tokens, cache_write_1h_tokens, cache_read_tokens, thinking_tokens, total_prompt_tokens,
	service_tier, speed, effort, inference_geo,
	stop_reason, stop_category,
	is_sidechain, session_id, project, git_branch, client_version, cli_entrypoint,
	cost_usd, api_equivalent_cost_usd, cost_source,
	prefix_hash, replay_of, replay_edits, capture_complete,
	method, path, status, req_tool_names, req_headers, resp_headers, req_body, resp_body,
	transcript_content, transcript_role`

func eventWriteArgs(ev *Event) []any {
	return []any{
		ev.RequestID, ev.Source, joinList(ev.SourceRefs), ev.FirstSource,
		unixNano(ev.StartedAt), nullableTime(ev.EndedAt),
		ev.AuthKind, ev.Account, ev.BillingMode,
		ev.ModelRequested, ev.ModelResolved,
		ev.InputTokens, ev.OutputTokens, ev.CacheWrite5mTokens, ev.CacheWrite1hTokens, ev.CacheReadTokens, ev.ThinkingTokens, ev.TotalPromptTokens,
		ev.ServiceTier, ev.Speed, ev.Effort, ev.InferenceGeo,
		ev.StopReason, ev.StopCategory,
		boolToInt(ev.IsSidechain), ev.SessionID, ev.Project, ev.GitBranch, ev.ClientVersion, ev.CliEntrypoint,
		nullableFloat(ev.CostUSD), nullableFloat(ev.ApiEquivalentCostUSD), ev.CostSource,
		nullableString(ev.PrefixHash), ev.ReplayOf, ev.ReplayEdits, boolToInt(ev.CaptureComplete),
		nullEmptyString(ev.Method), nullEmptyString(ev.Path), nullZeroInt(ev.Status), reqToolNamesArg(ev), nullEmptyString(ev.ReqHeaders), nullEmptyString(ev.RespHeaders), ev.ReqBody, ev.RespBody,
		ev.TranscriptContent, nullEmptyString(ev.TranscriptRole),
	}
}

func nullEmptyString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullZeroInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func insertEventTx(ctx context.Context, tx *sql.Tx, ev *Event) (int64, error) {
	args := eventWriteArgs(ev)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	res, err := tx.ExecContext(ctx, "INSERT INTO events ("+eventWriteColumns+") VALUES ("+placeholders+")", args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func updateEventTx(ctx context.Context, tx *sql.Tx, ev *Event) error {
	cols := strings.Split(eventWriteColumns, ",")
	var sets []string
	for _, c := range cols {
		c = strings.TrimSpace(c)
		sets = append(sets, c+" = ?")
	}
	args := eventWriteArgs(ev)
	args = append(args, ev.ID)
	_, err := tx.ExecContext(ctx, "UPDATE events SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	return err
}

func getEventByRequestIDTx(ctx context.Context, tx *sql.Tx, requestID string) (*Event, error) {
	return scanEvent(tx.QueryRowContext(ctx, eventSelectColumns+" FROM events WHERE request_id = ?", requestID))
}

func isUniqueConstraintError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}

// insertOrMerge inserts ev, or, on a request_id collision, merges it into
// the existing row (invariant 3) and reports whether a merge happened. A
// merge that finds two *complete* captures disagreeing on tokens attaches a
// source_mismatch warning to the merged row.
//
// sessionID is the session the surviving row belongs to, which is *not*
// always ev.SessionID: a merge never rewrites session_id, so the row keeps
// the session it was first written under (mergeEvents). Callers must
// re-derive that session rather than ev's — the plan's "exactly that one
// session is the re-derivation target" — because reconciling ev's session
// would aggregate a set the merged row is not in and leave the owner's
// totals at their pre-merge values, which is the drift the re-derive rule
// exists to prevent.
func insertOrMerge(ctx context.Context, tx *sql.Tx, ev *Event) (id int64, sessionID string, merged bool, err error) {
	id, err = insertEventTx(ctx, tx, ev)
	if err == nil {
		return id, ev.SessionID, false, nil
	}
	if !isUniqueConstraintError(err) {
		return 0, "", false, err
	}

	existing, gerr := getEventByRequestIDTx(ctx, tx, ev.RequestID)
	if gerr != nil {
		return 0, "", false, fmt.Errorf("merge: load existing request_id %s: %w", ev.RequestID, gerr)
	}

	result, err := applyMergeTx(ctx, tx, existing, ev)
	if err != nil {
		return 0, "", false, err
	}
	return result.ID, result.SessionID, true, nil
}

// applyMergeTx merges incoming into existing and persists the result --
// it performs no load of its own; the caller supplies existing already
// loaded by whichever key its own path resolves. insertOrMerge's own
// collision branch and internal/cli/rekey's collision path (br-GI-9-04)
// both call this one function, so the two cannot drift into two merge
// rules; the load is the one thing they cannot share (insertOrMerge loads
// by ev.RequestID, the rekey path by its target key) and it stays with
// each caller. On the rekey path, existing is the taker -- the row that
// already holds the target key -- so its request_id, id and session_id
// are the survivor's; nothing is re-keyed here.
func applyMergeTx(ctx context.Context, tx *sql.Tx, existing, incoming *Event) (*Event, error) {
	result, mismatch := mergeEvents(existing, incoming)
	if err := updateEventTx(ctx, tx, result); err != nil {
		return nil, fmt.Errorf("merge: update: %w", err)
	}
	if mismatch {
		w := Warning{
			Kind:      "source_mismatch",
			Severity:  "error",
			Detail:    fmt.Sprintf("sources %s and %s disagree on token counts for request_id %s", existing.Source, incoming.Source, existing.RequestID),
			CreatedAt: time.Now(),
		}
		if err := upsertWarningsTx(ctx, tx, result.ID, []Warning{w}); err != nil {
			return nil, fmt.Errorf("merge: attach source_mismatch: %w", err)
		}
	}
	return result, nil
}

// mergeEvents merges incoming into existing (existing.ID is preserved) and
// reports whether both sides were complete captures that disagree on
// tokens.
//
// The complete capture wins the token/cost/response-derived columns; an
// absent or partial side simply has nothing to contribute (a 0-vs-N
// difference is not a disagreement). source_refs gains the new source;
// first_source and session_id are never rewritten. The columns one source
// structurally cannot supply (proxy-only capture fields for a JSONL row,
// and the JSONL-only fields for a proxy row) are backfilled from whichever
// side actually has them.
func mergeEvents(existing, incoming *Event) (result *Event, mismatch bool) {
	merged := *existing
	merged.SourceRefs = unionStrings(existing.SourceRefs, append([]string{existing.Source}, incoming.Source))

	tokensDiffer := existing.InputTokens != incoming.InputTokens ||
		existing.OutputTokens != incoming.OutputTokens ||
		existing.CacheWrite5mTokens != incoming.CacheWrite5mTokens ||
		existing.CacheWrite1hTokens != incoming.CacheWrite1hTokens ||
		existing.CacheReadTokens != incoming.CacheReadTokens ||
		existing.ThinkingTokens != incoming.ThinkingTokens

	// The rule is "prefer the more complete record". Both halves of a capture
	// count: since br-GI-7-08 CaptureComplete is also false when only the
	// *request* body was cut at the read cap, a proxy row in that state no
	// longer wins this pick against a wholly-captured jsonl row for the same
	// request. That is deliberate and conservative -- with one body known to
	// be a prefix, the record that is whole is the safer one to quote -- and
	// it costs nothing in visibility, because a differing pair is still
	// reported through `mismatch` above. Pinned by a test so a later change
	// to it is a decision rather than a side effect.
	//
	// This comment governs the *pick* and nothing else. The surviving row's
	// own capture_complete flag is a separate question with a separate rule,
	// derived from the bodies the row ends up holding (see the derivation
	// after the body backfill below) -- the pick reading the two input flags
	// never decides what the merged row reports about its own bodies.
	winner := existing
	switch {
	case existing.CaptureComplete && incoming.CaptureComplete:
		winner = incoming
		// A disagreement needs two measurements. This function's own contract
		// says "a 0-vs-N difference is not a disagreement", and under
		// --body-policy off that is exactly the pair a merge produces: the
		// proxy row looked at nothing, so all six of its columns are zero, and
		// ungated this fired a source_mismatch at SeverityError on every
		// off-policy call that merged -- in the *ordinary* proxy-first
		// ordering, not a race. Reproduced before it was fixed.
		mismatch = tokensDiffer && usageObserved(existing) && usageObserved(incoming)
	case !existing.CaptureComplete && incoming.CaptureComplete:
		winner = incoming
	}

	// ...with one correction, which the completeness flag alone cannot make.
	// A row captured under --body-policy off reports CaptureComplete true --
	// nothing was narrowed, the body was simply never kept -- and carries no
	// usage at all, because usage is parsed out of the response body. Under the
	// rule above that row *wins* whenever it arrives second, zeroing the
	// populated tokens of the jsonl row it merged with, and the zero is not a
	// measurement: it means "never observed", the same distinction
	// cost-and-quota draws between an unpriced row and a $0.00 one. So a row
	// with no observed usage never takes the pick from a row that has some.
	//
	// Both-zero and both-nonzero keep the flag's decision untouched: the first
	// has nothing to lose, and the second is the ordinary two-captures case
	// this rule was written for. winner is only ever one of the two arguments,
	// so the identity test below is exact.
	if !usageObserved(winner) {
		loser := incoming
		if winner == incoming {
			loser = existing
		}
		if usageObserved(loser) {
			// No mismatch here either: the swap happens precisely *because*
			// one side never looked, which is an absence, not a conflict.
			winner = loser
		}
	}

	merged.InputTokens = winner.InputTokens
	merged.OutputTokens = winner.OutputTokens
	merged.CacheWrite5mTokens = winner.CacheWrite5mTokens
	merged.CacheWrite1hTokens = winner.CacheWrite1hTokens
	merged.CacheReadTokens = winner.CacheReadTokens
	merged.ThinkingTokens = winner.ThinkingTokens
	merged.TotalPromptTokens = merged.InputTokens + merged.CacheWrite5mTokens + merged.CacheWrite1hTokens + merged.CacheReadTokens
	merged.CostUSD = winner.CostUSD
	merged.ApiEquivalentCostUSD = winner.ApiEquivalentCostUSD
	merged.CostSource = winner.CostSource
	merged.StopReason = winner.StopReason
	merged.StopCategory = winner.StopCategory
	merged.ServiceTier = winner.ServiceTier
	merged.Speed = winner.Speed
	merged.ModelResolved = winner.ModelResolved
	// CaptureComplete is deliberately NOT assigned here. It is downstream of
	// the body backfill below, not of the winner pick, and the two can
	// legitimately disagree -- where they do, the bodies win. The `||` that
	// stood on this line turned a truncated proxy capture into a complete one
	// while the merged row kept the truncated body, which is the laundering
	// this whole story exists to remove.

	// first_source and session_id are never rewritten by a merge.
	merged.FirstSource = existing.FirstSource
	merged.SessionID = existing.SessionID

	merged.AuthKind = preferNonEmpty(existing.AuthKind, incoming.AuthKind)
	merged.Account = preferNonEmpty(existing.Account, incoming.Account)
	// billing_mode moves with the cost columns above -- it is not
	// preferNonEmpty like its neighbours. It is the one column a cross-source
	// merge is expected to contradict: the JSONL tailer resolves it per row by
	// model prefix, so re-ingesting a DeepSeek call flips it subscription ->
	// api. Since existing.BillingMode is non-empty for every row reachable
	// today, preferNonEmpty could never apply that flip, and the merge would
	// write the incoming priced cost_usd onto a row still marked subscription
	// -- exactly the pair invariant 5 forbids, silently, because
	// TestBillingModeInvariants covered the insert path only.
	//
	// Account above stays preferNonEmpty deliberately: it is a column the JSONL
	// tailer structurally cannot supply (the JSONL line carries no auth
	// signal), and on the ordinary ordering the proxy row is written live
	// (existing, holding a real account name) while the JSONL row arrives
	// minutes later, so a winner-based assignment would blank it.
	//
	// Known gap: a merge whose two sides genuinely disagree on the mode (a
	// credential resolving to subscription on a prefix-api model) now takes the
	// winner's mode and is not surfaced. auth_kind_anomaly fires on
	// api_key + subscription, which is a different case, and this change
	// removes the only production path that produced its trigger.
	merged.BillingMode = winner.BillingMode
	// The winner's capture could not classify its credential, so it hands over
	// no mode -- billing_modeForAuthKind returns "" for anything authkind.go
	// does not recognize. Assigning it unconditionally would blank a non-empty
	// stored mode, and an empty mode matches none of the three aggregates'
	// CASE WHEN billing_mode = 'api' / 'subscription' branches, so the row
	// would silently drop out of every cost total.
	//
	// Invariant 5 carries a figure's billing model in the column it lives in,
	// so derive the label from the column the winner actually priced. Both
	// cold-path pricers route cost by
	// `switch ev.BillingMode { case "subscription": ApiEquivalentCostUSD;
	// default: CostUSD }`, so an empty mode already means "the money went to
	// cost_usd". The label follows the money.
	//
	// Adopting the loser's mode instead -- the obvious-looking repair -- is
	// worse than the defect: it pairs the winner's CostUSD with a subscription
	// label, which is the very invariant-5 violation this function was changed
	// to remove, and it contributes 0 to both aggregates anyway because the
	// subscription sum reads the column the winner left NULL.
	if merged.BillingMode == "" {
		switch {
		case winner.CostUSD != nil:
			merged.BillingMode = "api"
		case winner.ApiEquivalentCostUSD != nil:
			merged.BillingMode = "subscription"
		default:
			// The winner priced nothing, so there is no cost column for an
			// adopted label to contradict -- the risk that rules the loser's
			// mode out above is absent here. Keep the stored mode rather than
			// blanking it: an empty mode drops the row out of every
			// billing_mode-keyed grouping for no gain, and a DeepSeek call the
			// proxy could not classify should keep the JSONL row's
			// prefix-derived api.
			merged.BillingMode = existing.BillingMode
		}
	}

	// The columns A (proxy) structurally cannot supply.
	merged.ClientVersion = preferNonEmpty(existing.ClientVersion, incoming.ClientVersion)
	merged.Project = preferNonEmpty(existing.Project, incoming.Project)
	merged.GitBranch = preferNonEmpty(existing.GitBranch, incoming.GitBranch)
	merged.CliEntrypoint = preferNonEmpty(existing.CliEntrypoint, incoming.CliEntrypoint)
	merged.IsSidechain = existing.IsSidechain || incoming.IsSidechain

	// The columns B (JSONL) structurally cannot supply.
	merged.Method = preferNonEmpty(existing.Method, incoming.Method)
	merged.Path = preferNonEmpty(existing.Path, incoming.Path)
	if existing.Status == 0 {
		merged.Status = incoming.Status
	}
	merged.ReqHeaders = preferNonEmpty(existing.ReqHeaders, incoming.ReqHeaders)
	merged.RespHeaders = preferNonEmpty(existing.RespHeaders, incoming.RespHeaders)
	// Which side supplies each retained body, read as the backfill decides it.
	// The two bodies are backfilled independently, so the request body's owner
	// need not be the response body's, and the capture_complete derivation
	// below needs to know which -- a single row-level bool cannot say *which*
	// body was cut.
	bodySides := 0
	if len(existing.ReqBody) == 0 {
		merged.ReqBody = incoming.ReqBody
		// req_tool_names is a function of whichever body the row ends up
		// holding (br-GI-13-07), not of whichever side merged := *existing
		// happened to carry over. Left to existing alone, a merge that
		// backfills a body from incoming would produce a row with a body and
		// a stale (or NULL) ToolNames -- the exact "NULL iff no body"
		// violation the column's contract forbids, and rowsWithRequestBody's
		// HasReqBody filter would then skip a body-bearing row and the tools
		// rule would silently decline on it.
		merged.ToolNames = incoming.ToolNames
		if len(incoming.ReqBody) > 0 {
			bodySides |= bodyFromIncoming
		}
	} else {
		bodySides |= bodyFromExisting
	}
	if len(existing.RespBody) == 0 {
		merged.RespBody = incoming.RespBody
		if len(incoming.RespBody) > 0 {
			bodySides |= bodyFromIncoming
		}
	} else {
		bodySides |= bodyFromExisting
	}
	merged.CaptureComplete = captureCompleteFromBodies(existing.CaptureComplete, incoming.CaptureComplete, bodySides)
	// The proxy-only prefix/replay columns the JSONL side structurally cannot
	// supply (br-GI-9-04, plan §3 F12.2). mergeEvents assigns none of them --
	// they ride `merged := *existing` -- so on the *live* ordering the proxy
	// row is written first with a computed PrefixHash and the JSONL arrival is
	// nil/empty for all three, a no-op. On the rekey collision the seats are
	// reversed: existing is the JSONL taker (nil/empty for all three) and
	// incoming is the proxy row carrying the hash the backfill just computed.
	// Without the carry the merge would drop it *durably* -- pass 3's re-ingest
	// is a JSONL row, nil for all three -- and the two hash-keyed session rules
	// `continue` on a nil hash (internal/analyze/rules.go), silently disabling
	// them on exactly the rows the backfill merges. Carry the incoming value
	// only when the survivor has none: `preferNonEmpty` for the string pair and
	// the nil test for the nullable hash (a non-nil "" there is a real value --
	// "the body did not parse as JSON" -- not an absence).
	merged.PrefixHash = preferNonEmptyPtr(existing.PrefixHash, incoming.PrefixHash)
	merged.ReplayOf = preferNonEmpty(existing.ReplayOf, incoming.ReplayOf)
	merged.ReplayEdits = preferNonEmpty(existing.ReplayEdits, incoming.ReplayEdits)
	// The transcript columns are written by one side only -- internal/jsonlogs.
	// A new column with no rule here would be silently dropped from the
	// incoming side, and the common ordering is proxy-first: the capture is
	// written live and the reconstruction arrives minutes later, so "no rule"
	// means the content is discarded exactly when it finally shows up.
	//
	// These two are structurally transcript-only, so "a capture and a
	// reconstruction disagree" is not reachable for them: a proxy row's value
	// is always empty and only an empty existing cell is backfilled. The rule
	// is the merge's general one -- the first-written side keeps its value --
	// stated here so a future column written by both sides is a decision rather
	// than an accident of the copy.
	if len(existing.TranscriptContent) == 0 {
		merged.TranscriptContent = incoming.TranscriptContent
	}
	merged.TranscriptRole = preferNonEmpty(existing.TranscriptRole, incoming.TranscriptRole)

	return &merged, mismatch
}

// The two sides a retained body can come from, as a bit set: a merged row can
// hold a request body from one side and a response body from the other, so the
// owners are a set and not a choice. No body at all is the zero value.
const (
	bodyFromExisting = 1 << iota
	bodyFromIncoming
)

// captureCompleteFromBodies derives the merged row's own flag from the bodies
// it holds, by the owner(s) that supplied them.
//
// A single row-level bool cannot say *which* body was cut, so an exact answer
// is impossible and the rule has to state which way it errs. It errs toward
// false deliberately, and the asymmetry is the whole argument: a spurious
// cc=0 is a visible, honest over-report -- an incomplete flag on a row that
// was whole -- while a spurious cc=1 is exactly the laundering this replaced,
// where the `||` turned a truncated proxy capture into a complete one while
// the row kept the truncated body.
//
// When the two retained bodies have different owners the row is complete only
// if BOTH contributing sides were, hence the `&&` and never the `||`. When the
// row holds no body at all it keeps existing's flag, which is what keeps the
// --body-policy off row (no bodies, flag true) honest.
func captureCompleteFromBodies(existing, incoming bool, bodySides int) bool {
	switch bodySides {
	case bodyFromExisting:
		return existing
	case bodyFromIncoming:
		return incoming
	case bodyFromExisting | bodyFromIncoming:
		return existing && incoming
	default:
		return existing
	}
}

// usageObserved reports whether a row carries any measured token count at all.
// It is the "was this ever observed" test, not a "was this expensive" one: a row
// captured under --body-policy off has no usage because usage is parsed out of
// the response body, and every one of its token columns is the zero value of
// never having looked. Reading that zero as a measurement is the same error as
// reading an unpriced API row's absent cost as $0.00.
func usageObserved(ev *Event) bool {
	return ev.InputTokens != 0 || ev.OutputTokens != 0 ||
		ev.CacheWrite5mTokens != 0 || ev.CacheWrite1hTokens != 0 ||
		ev.CacheReadTokens != 0 || ev.ThinkingTokens != 0
}

func preferNonEmpty(existing, incoming string) string {
	if existing != "" {
		return existing
	}
	return incoming
}

// preferNonEmptyPtr is preferNonEmpty for a nullable column: the survivor
// keeps its own non-nil value and only a NULL survivor takes the incoming
// side. A non-nil "" is a real value for PrefixHash (the body did not parse
// as JSON), not an absence, so it is kept rather than overwritten.
func preferNonEmptyPtr(existing, incoming *string) *string {
	if existing != nil {
		return existing
	}
	return incoming
}

func unionStrings(existing []string, add []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range existing {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range add {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
