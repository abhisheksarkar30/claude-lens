package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

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
