package store

import (
	"context"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/abhisheksarkar30/claude-lens/internal/pricing"
)

// These cases price through the real pricing package. The import guard in
// importguard_test.go skips _test.go files deliberately: the *store* must not
// import internal/pricing, and TestStoreDoesNotImportPricing is what says so,
// but a test that pins the store's output against the real table is the
// strongest available check and adds no production dependency.

func usdPerMTok(usd int64) *big.Rat { return big.NewRat(usd, 1_000_000) }

// repriceTable is the table most cases price against: one model at $10/MTok
// for input and output, $1/MTok for cache reads, no cache-write fee.
func repriceTable(model, source string) pricing.Table {
	return pricing.Table{
		model: {
			Model:            model,
			InputRate:        usdPerMTok(10),
			OutputRate:       usdPerMTok(10),
			CacheWrite5mRate: big.NewRat(0, 1),
			CacheWrite1hRate: big.NewRat(0, 1),
			CacheReadRate:    usdPerMTok(1),
			Source:           source,
		},
	}
}

// repriceRow is a proxy-shaped row whose stored cost is the known-wrong zero
// the pre-GI-11 per-class rounding produced: 300 tokens in each of input and
// output, which is $0.003 per class and so priced at exactly $0.00.
func repriceRow(requestID string) *Event {
	ev := fullEvent(requestID)
	ev.ModelRequested = "test-model"
	ev.ModelResolved = "test-model"
	ev.InputTokens = 300
	ev.OutputTokens = 300
	ev.CacheWrite5mTokens = 0
	ev.CacheWrite1hTokens = 0
	ev.CacheReadTokens = 0
	ev.CostSource = "shipped"
	ev.CostUSD = f64(0)
	ev.ApiEquivalentCostUSD = nil
	return ev
}

func TestRepriceCostsPricesTheExactValue(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, _, err := st.InsertEvent(ctx, repriceRow("req-reprice-exact"))
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	counts, err := st.RepriceCosts(ctx, repriceTable("test-model", "shipped"), false)
	if err != nil {
		t.Fatalf("RepriceCosts: %v", err)
	}
	if counts.Moved != 1 || counts.Skipped != 0 {
		t.Errorf("counts = %+v, want Moved 1 Skipped 0", counts)
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	// 300 x $10/MTok for each of input and output, summed exactly.
	if got.CostUSD == nil || *got.CostUSD != 0.006 {
		t.Errorf("CostUSD = %v, want the exact 0.006", got.CostUSD)
	}
	if got.ApiEquivalentCostUSD != nil {
		t.Errorf("ApiEquivalentCostUSD = %v, want nil for an api row (invariant 5)", got.ApiEquivalentCostUSD)
	}

	// Idempotent: recomputing an already-correct row moves nothing.
	again, err := st.RepriceCosts(ctx, repriceTable("test-model", "shipped"), false)
	if err != nil {
		t.Fatalf("RepriceCosts (second run): %v", err)
	}
	if again.Moved != 0 || again.Unchanged != 1 {
		t.Errorf("second run counts = %+v, want Moved 0 Unchanged 1", again)
	}
}

// TestRepriceCostsIncludesUserRows pins that a user-overridden row is repriced
// against the effective table rather than skipped: "the user overrode it" is
// not a reason to skip, because the override replaces a *rate* and reprice
// prices with the same table the insert path would.
func TestRepriceCostsIncludesUserRows(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	ev := repriceRow("req-reprice-user")
	ev.CostSource = "user"
	ev.CostUSD = f64(0)
	id, _, err := st.InsertEvent(ctx, ev)
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	counts, err := st.RepriceCosts(ctx, repriceTable("test-model", "user"), false)
	if err != nil {
		t.Fatalf("RepriceCosts: %v", err)
	}
	if counts.Moved != 1 {
		t.Errorf("counts.Moved = %d, want 1 (a user row is in scope, not skipped)", counts.Moved)
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.CostSource != "user" {
		t.Errorf("CostSource = %q, want user", got.CostSource)
	}
	if got.CostUSD == nil || *got.CostUSD != 0.006 {
		t.Errorf("CostUSD = %v, want the effective table's exact 0.006", got.CostUSD)
	}

	// The override is a rate, not a per-row cost: a table whose rate differs
	// must move the same row again.
	override := repriceTable("test-model", "user")
	row := override["test-model"]
	row.InputRate, row.OutputRate = usdPerMTok(50), usdPerMTok(50)
	override["test-model"] = row
	if _, err := st.RepriceCosts(ctx, override, false); err != nil {
		t.Fatalf("RepriceCosts (overridden rate): %v", err)
	}
	got, err = st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.CostUSD == nil || *got.CostUSD != 0.03 {
		t.Errorf("CostUSD = %v, want the overridden table's exact 0.03", got.CostUSD)
	}
}

// TestRepriceCostsPreservesTheTTLUnknownLabel pins the reconstruction rule:
// usage.TTLUnknown is not a stored column, but the cost_source label is, and
// it is the key. A cache_ttl_unknown row is therefore priced like any other
// AND comes back carrying the same label.
func TestRepriceCostsPreservesTheTTLUnknownLabel(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	ev := repriceRow("req-reprice-ttl")
	ev.InputTokens = 0
	ev.OutputTokens = 0
	ev.CacheWrite5mTokens = 300
	ev.CostSource = cacheTTLUnknownSource
	ev.CostUSD = f64(0)
	id, _, err := st.InsertEvent(ctx, ev)
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	// A table that charges for the write class, so the corrected figure is a
	// real number rather than the fixture's zero.
	table := repriceTable("test-model", "shipped")
	row := table["test-model"]
	row.CacheWrite5mRate = usdPerMTok(10)
	table["test-model"] = row

	counts, err := st.RepriceCosts(ctx, table, false)
	if err != nil {
		t.Fatalf("RepriceCosts: %v", err)
	}
	if counts.Moved != 1 || counts.Skipped != 0 {
		t.Errorf("counts = %+v, want Moved 1 Skipped 0 (an approximate row is in scope)", counts)
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.CostSource != cacheTTLUnknownSource {
		t.Errorf("CostSource = %q, want %q preserved through the rewrite", got.CostSource, cacheTTLUnknownSource)
	}
	if got.CostUSD == nil || *got.CostUSD != 0.003 {
		t.Errorf("CostUSD = %v, want 300 cache-write tokens at $10/MTok = 0.003", got.CostUSD)
	}
}

// TestRepriceCostsUsesTheConfiguredOffPeakCalendar is the F6.1 case: the
// caller's configured off-peak dates have to reach the store's Compute. A
// nil-dates loader means "unset -> the shipped calendar", which is a state
// distinct from a non-nil list that excludes nothing, and the two price the
// same row differently -- so a shell that dropped cfg.PeakOffPeakDates would
// store the shipped figure here and never notice.
func TestRepriceCostsUsesTheConfiguredOffPeakCalendar(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	overrides := filepath.Join(t.TempDir(), "prices.toml")

	// 2026-09-25 is a Friday and one of the shipped off-peak dates, inside the
	// DeepSeek peak window's [01:00,04:00) UTC span.
	at := time.Date(2026, 9, 25, 2, 0, 0, 0, time.UTC)
	if got := at.Weekday(); got != time.Friday {
		t.Fatalf("fixture date 2026-09-25 is a %s, want Friday", got)
	}

	ev := repriceRow("req-reprice-calendar")
	ev.ModelResolved = "deepseek-flash"
	ev.InputTokens = 1_000_000
	ev.OutputTokens = 0
	ev.StartedAt = at
	ev.CostUSD = f64(0)
	id, _, err := st.InsertEvent(ctx, ev)
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	// nil dates: unset, so the shipped holiday list applies and the fixture
	// instant is off peak. deepseek-flash bills $0.15/MTok off peak.
	shipped := pricing.NewLoader(overrides, nil)
	if _, err := st.RepriceCosts(ctx, shipped, false); err != nil {
		t.Fatalf("RepriceCosts (shipped calendar): %v", err)
	}
	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.CostUSD == nil || *got.CostUSD != 0.15 {
		t.Fatalf("off-peak CostUSD = %v, want 0.15 (the shipped calendar's rate)", got.CostUSD)
	}

	// A non-nil list that does not contain the fixture date: the same instant
	// is now peak, at 2x.
	configured := pricing.NewLoader(overrides, []string{"2026-01-01"})
	counts, err := st.RepriceCosts(ctx, configured, false)
	if err != nil {
		t.Fatalf("RepriceCosts (configured calendar): %v", err)
	}
	if counts.Moved != 1 {
		t.Errorf("counts.Moved = %d, want 1 (the configured calendar prices the row differently)", counts.Moved)
	}
	got, err = st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.CostUSD == nil || *got.CostUSD != 0.30 {
		t.Errorf("peak CostUSD = %v, want 0.30 -- the configured dates did not reach Compute", got.CostUSD)
	}
}

// TestRepriceCostsSkipsUnresolvableRows: a row whose model_resolved no longer
// resolves keeps its stored cost AND its cost_source. --yes must not be
// destructive on exactly the rows it cannot price.
func TestRepriceCostsSkipsUnresolvableRows(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	ev := repriceRow("req-reprice-gone")
	ev.ModelResolved = "gone-model"
	ev.CostUSD = f64(0.5)
	id, _, err := st.InsertEvent(ctx, ev)
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	counts, err := st.RepriceCosts(ctx, repriceTable("test-model", "shipped"), false)
	if err != nil {
		t.Fatalf("RepriceCosts: %v", err)
	}
	if counts.Skipped != 1 || counts.Moved != 0 {
		t.Errorf("counts = %+v, want Skipped 1 Moved 0", counts)
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.CostUSD == nil || *got.CostUSD != 0.5 {
		t.Errorf("CostUSD = %v, want the stored 0.5 kept, never nulled", got.CostUSD)
	}
	if got.CostSource != "shipped" {
		t.Errorf("CostSource = %q, want the stored shipped kept", got.CostSource)
	}

	// An unpriced row is the same carve-out from the other side: no rate was
	// resolvable at insert, so there is no stored figure to correct and the
	// row is out of scope even though the model resolves now.
	unpriced := repriceRow("req-reprice-unpriced")
	unpriced.CostSource = "unpriced"
	unpriced.CostUSD = nil
	if _, _, err := st.InsertEvent(ctx, unpriced); err != nil {
		t.Fatalf("InsertEvent (unpriced): %v", err)
	}
	counts, err = st.RepriceCosts(ctx, repriceTable("test-model", "shipped"), false)
	if err != nil {
		t.Fatalf("RepriceCosts (unpriced present): %v", err)
	}
	if counts.Moved != 0 {
		t.Errorf("counts.Moved = %d, want 0: an unpriced row is never newly priced", counts.Moved)
	}
}

// TestRepriceCostsRoutesSubscriptionToApiEquivalent is invariant 5 in both
// directions: the new figure lands in the column the billing mode routes to
// and the other column is left NULL.
func TestRepriceCostsRoutesSubscriptionToApiEquivalent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	ev := repriceRow("req-reprice-sub")
	ev.BillingMode = "subscription"
	ev.CostUSD = nil
	ev.ApiEquivalentCostUSD = f64(0)
	if err := st.UpsertSession(ctx, ev.SessionID, "", ev.StartedAt); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	id, _, err := st.InsertEvent(ctx, ev)
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	if _, err := st.RepriceCosts(ctx, repriceTable("test-model", "shipped"), false); err != nil {
		t.Fatalf("RepriceCosts: %v", err)
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.ApiEquivalentCostUSD == nil || *got.ApiEquivalentCostUSD != 0.006 {
		t.Errorf("ApiEquivalentCostUSD = %v, want the exact 0.006", got.ApiEquivalentCostUSD)
	}
	if got.CostUSD != nil {
		t.Errorf("CostUSD = %v, want nil: a subscription figure never lands in the api column", got.CostUSD)
	}

	sess, err := st.GetSession(ctx, ev.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.TotalApiEquivalentCostUSD == nil || *sess.TotalApiEquivalentCostUSD != 0.006 {
		t.Errorf("session TotalApiEquivalentCostUSD = %v, want 0.006", sess.TotalApiEquivalentCostUSD)
	}
	if sess.TotalCostUSD != nil {
		t.Errorf("session TotalCostUSD = %v, want nil (invariant 5)", sess.TotalCostUSD)
	}
}

// TestRepriceCostsReducesTheOwningSessionTotal is the F1.1 rollup: sessions'
// cost columns are materialized, so correcting events without re-deriving them
// would leave `clens stats` and /api/sessions reporting the pre-fix totals.
func TestRepriceCostsReducesTheOwningSessionTotal(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	ev := repriceRow("req-reprice-rollup")
	ev.CostUSD = f64(0.5)
	if err := st.UpsertSession(ctx, ev.SessionID, "", ev.StartedAt); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if _, _, err := st.InsertEvent(ctx, ev); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if err := st.ReconcileSession(ctx, ev.SessionID); err != nil {
		t.Fatalf("ReconcileSession: %v", err)
	}

	before, err := st.GetSession(ctx, ev.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if before.TotalCostUSD == nil || *before.TotalCostUSD != 0.5 {
		t.Fatalf("pre-reprice session total = %v, want 0.5", before.TotalCostUSD)
	}

	if _, err := st.RepriceCosts(ctx, repriceTable("test-model", "shipped"), false); err != nil {
		t.Fatalf("RepriceCosts: %v", err)
	}

	after, err := st.GetSession(ctx, ev.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if after.TotalCostUSD == nil || *after.TotalCostUSD != 0.006 {
		t.Errorf("post-reprice session total = %v, want the row's own 0.006", after.TotalCostUSD)
	}
}

// TestRepriceCostsDryRunWritesNothing: the dry run's counts come from the same
// loop that does the repair, so the preview cannot drift from what --yes does
// -- and it must not write while reading.
func TestRepriceCostsDryRunWritesNothing(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	const session = "s_1"

	ev := repriceRow("req-reprice-dry")
	if err := st.UpsertSession(ctx, session, "", ev.StartedAt); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	id, _, err := st.InsertEvent(ctx, ev)
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if err := st.ReconcileSession(ctx, session); err != nil {
		t.Fatalf("ReconcileSession: %v", err)
	}

	counts, err := st.RepriceCosts(ctx, repriceTable("test-model", "shipped"), true)
	if err != nil {
		t.Fatalf("RepriceCosts (dry run): %v", err)
	}
	if counts.Moved != 1 {
		t.Errorf("dry-run counts.Moved = %d, want 1 (it reports what --yes would change)", counts.Moved)
	}

	got, err := st.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.CostUSD == nil || *got.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want the stored 0 untouched by a dry run", got.CostUSD)
	}
	if got.CostSource != "shipped" {
		t.Errorf("CostSource = %q, want the stored label untouched", got.CostSource)
	}

	sess, err := st.GetSession(ctx, session)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.TotalCostUSD == nil || *sess.TotalCostUSD != 0 {
		t.Errorf("session total = %v, want the stored 0 untouched by a dry run", sess.TotalCostUSD)
	}
}

// TestRepriceCostsErrorsOnAClosedStore is §7's named error path: a bulk
// rewrite must not apply partially, and the single BeginTx is what makes that
// impossible. Verified by reading the row back through a fresh Open of the
// same file.
func TestRepriceCostsErrorsOnAClosedStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	id, _, err := st.InsertEvent(ctx, repriceRow("req-reprice-closed"))
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := st.RepriceCosts(ctx, repriceTable("test-model", "shipped"), false); err == nil {
		t.Fatal("RepriceCosts on a closed store returned no error")
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	got, err := reopened.GetEvent(ctx, id)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if got.CostUSD == nil || *got.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want the stored 0: a failed reprice must leave nothing behind", got.CostUSD)
	}
}
