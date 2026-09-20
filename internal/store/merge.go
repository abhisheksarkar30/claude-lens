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
	method, path, status, req_headers, resp_headers, req_body, resp_body,
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
		nullEmptyString(ev.Method), nullEmptyString(ev.Path), nullZeroInt(ev.Status), nullEmptyString(ev.ReqHeaders), nullEmptyString(ev.RespHeaders), ev.ReqBody, ev.RespBody,
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

	result, mismatch := mergeEvents(existing, ev)
	if err := updateEventTx(ctx, tx, result); err != nil {
		return 0, "", false, fmt.Errorf("merge: update: %w", err)
	}
	if mismatch {
		w := Warning{
			Kind:      "source_mismatch",
			Severity:  "error",
			Detail:    fmt.Sprintf("sources %s and %s disagree on token counts for request_id %s", existing.Source, ev.Source, ev.RequestID),
			CreatedAt: time.Now(),
		}
		if err := upsertWarningsTx(ctx, tx, result.ID, []Warning{w}); err != nil {
			return 0, "", false, fmt.Errorf("merge: attach source_mismatch: %w", err)
		}
	}
	return result.ID, result.SessionID, true, nil
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

	winner := existing
	switch {
	case existing.CaptureComplete && incoming.CaptureComplete:
		winner = incoming
		mismatch = tokensDiffer
	case !existing.CaptureComplete && incoming.CaptureComplete:
		winner = incoming
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
	merged.CaptureComplete = existing.CaptureComplete || incoming.CaptureComplete

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
	if len(existing.ReqBody) == 0 {
		merged.ReqBody = incoming.ReqBody
	}
	if len(existing.RespBody) == 0 {
		merged.RespBody = incoming.RespBody
	}
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

func preferNonEmpty(existing, incoming string) string {
	if existing != "" {
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
