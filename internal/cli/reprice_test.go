package cli

import (
	"bytes"
	"context"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/pricing"
	"github.com/abhisheksarkar30/claude-lens/internal/store"
)

// repriceSeedRow is a proxy-shaped row whose stored cost is the known-wrong
// zero the pre-GI-11 per-class cent rounding produced: 300 tokens in each of
// input and output, which is $0.003 per class and so priced at exactly $0.00.
func repriceSeedRow(requestID string) *store.Event {
	zero := 0.0
	return &store.Event{EventSummary: store.EventSummary{
		RequestID:       requestID,
		ModelResolved:   "claude-sonnet-5",
		StartedAt:       time.Now(),
		SessionID:       "s_reprice",
		InputTokens:     300,
		OutputTokens:    300,
		CostUSD:         &zero,
		CostSource:      "shipped",
		CaptureComplete: true,
		ServiceTier:     "standard",
	}}
}

// TestRepriceDryRunChangesNothing: `--dry-run` reports what `--yes` would
// change and writes nothing -- neither the in-scope row's cost columns nor its
// owning session's totals, which are materialized and would otherwise be left
// describing the pre-fix value.
func TestRepriceDryRunChangesNothing(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	ctx := context.Background()

	const session = "s_reprice"
	if err := st.UpsertSession(ctx, session, "", time.Now()); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	id := seedEvent(t, st, repriceSeedRow("req-cli-reprice-dry"))
	if err := st.ReconcileSession(ctx, session); err != nil {
		t.Fatalf("ReconcileSession: %v", err)
	}

	before, err := st.GetSession(ctx, session)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	var buf bytes.Buffer
	if err := runReprice([]string{"--dry-run"}, &buf); err != nil {
		t.Fatalf("runReprice --dry-run: %v\noutput:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "would reprice 1 row(s)") {
		t.Fatalf("dry-run output missing the would-reprice count: %s", buf.String())
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.CostUSD == nil || *got.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want the stored 0 untouched by a dry run", got.CostUSD)
	}
	if got.CostSource != "shipped" {
		t.Errorf("CostSource = %q, want the stored label untouched by a dry run", got.CostSource)
	}

	after, err := st.GetSession(ctx, session)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !sameFloat(before.TotalCostUSD, after.TotalCostUSD) {
		t.Errorf("session total = %v, want %v untouched by a dry run", after.TotalCostUSD, before.TotalCostUSD)
	}
	if !sameFloat(before.TotalApiEquivalentCostUSD, after.TotalApiEquivalentCostUSD) {
		t.Errorf("session api-equivalent total = %v, want %v untouched by a dry run",
			after.TotalApiEquivalentCostUSD, before.TotalApiEquivalentCostUSD)
	}
}

func sameFloat(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// TestRepriceWithoutYesRefuses: neither flag, no rewrite. The default is the
// opposite of destructive, as it is for purge and rekey.
func TestRepriceWithoutYesRefuses(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)

	id := seedEvent(t, st, repriceSeedRow("req-cli-reprice-refuse"))

	var buf bytes.Buffer
	if err := runReprice(nil, &buf); err == nil {
		t.Fatalf("runReprice with no flags returned no error\noutput:\n%s", buf.String())
	} else if !strings.Contains(err.Error(), "--yes") || !strings.Contains(err.Error(), "--dry-run") {
		t.Errorf("refusal %q should name both flags", err)
	}

	got, err := st.GetEvent(context.Background(), id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.CostUSD == nil || *got.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want the stored 0 untouched by a refused run", got.CostUSD)
	}
}

// TestRepriceUsesTheConfiguredOffPeakCalendar pins the shell's wiring: the
// configured off-peak dates have to reach the table the store prices with. A
// shell that dropped cfg.PeakOffPeakDates -- the nil-dates form -- would store
// the shipped-calendar figure in both halves of this test and never notice.
func TestRepriceUsesTheConfiguredOffPeakCalendar(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	ctx := context.Background()

	// 2026-09-25 is a Friday and one of the shipped off-peak dates, inside the
	// DeepSeek peak window's [01:00,04:00) UTC span.
	at := time.Date(2026, 9, 25, 2, 0, 0, 0, time.UTC)
	if got := at.Weekday(); got != time.Friday {
		t.Fatalf("fixture date 2026-09-25 is a %s, want Friday", got)
	}

	zero := 0.0
	id := seedEvent(t, st, &store.Event{EventSummary: store.EventSummary{
		RequestID:     "req-cli-reprice-calendar",
		ModelResolved: "deepseek-flash",
		StartedAt:     at,
		InputTokens:   1_000_000,
		CostUSD:       &zero,
		CostSource:    "shipped",
	}})

	// The `none` spelling: a non-nil empty list, so nothing is off peak and
	// the fixture instant is charged the peak 2x rate ($0.30/MTok).
	t.Setenv("CLENS_PEAK_OFF_PEAK_DATES", "none")
	var buf bytes.Buffer
	if err := runReprice([]string{"--yes"}, &buf); err != nil {
		t.Fatalf("runReprice --yes (none calendar): %v\noutput:\n%s", err, buf.String())
	}
	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.CostUSD == nil || *got.CostUSD != 0.30 {
		t.Fatalf("CostUSD = %v, want 0.30 at peak -- the configured dates did not reach the table", got.CostUSD)
	}

	// Unset: nil means "use the shipped calendar", under which the fixture
	// date is off peak and the same row reprices down to $0.15/MTok.
	t.Setenv("CLENS_PEAK_OFF_PEAK_DATES", "")
	buf.Reset()
	if err := runReprice([]string{"--yes"}, &buf); err != nil {
		t.Fatalf("runReprice --yes (shipped calendar): %v\noutput:\n%s", err, buf.String())
	}
	got, err = st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.CostUSD == nil || *got.CostUSD != 0.15 {
		t.Errorf("CostUSD = %v, want 0.15 off peak (the shipped calendar)", got.CostUSD)
	}
}

// TestRepriceModelFlagBackfillsOnlyNamedModel: --model is a narrowed pass.
// One flag value cannot name both the shipped subscription row and the
// from-scratch row, so each named model is its own runReprice. The four rows
// share one database; the second pass must not move what the first wrote.
func TestRepriceModelFlagBackfillsOnlyNamedModel(t *testing.T) {
	home := withHome(t)
	st := openTestStore(t, home)
	ctx := context.Background()

	const (
		namedShipped = "claude-sonnet-5"
		// Absent from ShippedTable on purpose: the dated Haiku 4.5 id is a
		// shipped key, so a partial override of it would inherit cache read.
		namedScratch  = "from-scratch-nil-cache-read"
		otherPriced   = "claude-opus-5"
		otherUnpriced = "claude-haiku-4-5"
		session       = "s_reprice_model"
	)
	if err := st.UpsertSession(ctx, session, "", time.Now()); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	kept := 1.25
	otherID := seedEvent(t, st, &store.Event{EventSummary: store.EventSummary{
		RequestID:     "req-other-priced",
		ModelResolved: otherPriced,
		SessionID:     session,
		InputTokens:   100,
		OutputTokens:  100,
		CostUSD:       &kept,
		CostSource:    "shipped",
		BillingMode:   "api",
	}})
	unpricedOtherID := seedEvent(t, st, &store.Event{EventSummary: store.EventSummary{
		RequestID:     "req-other-unpriced",
		ModelResolved: otherUnpriced,
		SessionID:     session,
		InputTokens:   100,
		OutputTokens:  100,
		CostSource:    "unpriced",
		BillingMode:   "api",
	}})
	subID := seedEvent(t, st, &store.Event{EventSummary: store.EventSummary{
		RequestID:       "req-named-subscription",
		ModelResolved:   namedShipped,
		SessionID:       session,
		InputTokens:     1_000_000,
		OutputTokens:    0,
		CacheReadTokens: 0,
		CostSource:      "unpriced",
		BillingMode:     "subscription",
	}})
	scratchID := seedEvent(t, st, &store.Event{EventSummary: store.EventSummary{
		RequestID:       "req-named-scratch",
		ModelResolved:   namedScratch,
		SessionID:       session,
		InputTokens:     100,
		OutputTokens:    50,
		CacheReadTokens: 10,
		CostSource:      "unpriced",
		BillingMode:     "api",
	}})

	overridePath := filepath.Join(home, ".clens", "prices.toml")
	perTok := func(usd int64) *big.Rat { return big.NewRat(usd, 1_000_000) }
	if err := pricing.SaveOverrides(overridePath, pricing.Table{
		namedScratch: {
			Model:      namedScratch,
			InputRate:  perTok(1),
			OutputRate: perTok(5),
		},
	}); err != nil {
		t.Fatalf("SaveOverrides: %v", err)
	}

	var buf bytes.Buffer
	if err := runReprice([]string{"--yes", "--model", namedShipped}, &buf); err != nil {
		t.Fatalf("runReprice --model %s: %v\n%s", namedShipped, err, buf.String())
	}
	if !strings.Contains(buf.String(), "`--model "+namedShipped+"` is backfilling previously-unpriced rows for this model only") {
		t.Fatalf("output missing the limited-pass sentence:\n%s", buf.String())
	}

	other := mustEvent(t, st, otherID)
	if other.CostSource != "shipped" || other.CostUSD == nil || *other.CostUSD != kept {
		t.Errorf("other in-scope row = source %q cost %v, want shipped %v", other.CostSource, other.CostUSD, kept)
	}
	unpricedOther := mustEvent(t, st, unpricedOtherID)
	if unpricedOther.CostSource != "unpriced" || unpricedOther.CostUSD != nil || unpricedOther.ApiEquivalentCostUSD != nil {
		t.Errorf("other unpriced row = source %q cost %v api %v, want unpriced and both nil", unpricedOther.CostSource, unpricedOther.CostUSD, unpricedOther.ApiEquivalentCostUSD)
	}
	sub := mustEvent(t, st, subID)
	if sub.ApiEquivalentCostUSD == nil || sub.CostUSD != nil || sub.CostSource == "unpriced" {
		t.Errorf("subscription row = source %q cost %v api %v, want a figure in api_equivalent_cost_usd and cost_usd nil", sub.CostSource, sub.CostUSD, sub.ApiEquivalentCostUSD)
	}

	buf.Reset()
	if err := runReprice([]string{"--yes", "--model", namedScratch}, &buf); err != nil {
		t.Fatalf("runReprice --model %s: %v\n%s", namedScratch, err, buf.String())
	}
	if !strings.Contains(buf.String(), "`--model "+namedScratch+"` is backfilling previously-unpriced rows for this model only") {
		t.Fatalf("output missing the limited-pass sentence:\n%s", buf.String())
	}
	scratch := mustEvent(t, st, scratchID)
	if scratch.CostSource != "unpriced" || scratch.CostUSD != nil || scratch.ApiEquivalentCostUSD != nil {
		t.Errorf("from-scratch row = source %q cost %v api %v, want unpriced and both nil", scratch.CostSource, scratch.CostUSD, scratch.ApiEquivalentCostUSD)
	}
	subAfter := mustEvent(t, st, subID)
	if subAfter.CostUSD != nil || subAfter.ApiEquivalentCostUSD == nil || *subAfter.ApiEquivalentCostUSD != *sub.ApiEquivalentCostUSD {
		t.Errorf("second pass moved the subscription row: cost %v api %v", subAfter.CostUSD, subAfter.ApiEquivalentCostUSD)
	}
}

func mustEvent(t *testing.T, st *store.Store, id int64) *store.Event {
	t.Helper()
	got, err := st.GetEvent(context.Background(), id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	return got
}
