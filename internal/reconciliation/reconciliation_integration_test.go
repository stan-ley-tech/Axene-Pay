//go:build integration

package reconciliation_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stan-ley-tech/axene-pay/internal/accounting"
	"github.com/stan-ley-tech/axene-pay/internal/audit"
	"github.com/stan-ley-tech/axene-pay/internal/fees"
	"github.com/stan-ley-tech/axene-pay/internal/idempotency"
	"github.com/stan-ley-tech/axene-pay/internal/ledger"
	"github.com/stan-ley-tech/axene-pay/internal/merchant"
	"github.com/stan-ley-tech/axene-pay/internal/payments"
	"github.com/stan-ley-tech/axene-pay/internal/platform/ids"
	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
	"github.com/stan-ley-tech/axene-pay/internal/platform/testdb"
	"github.com/stan-ley-tech/axene-pay/internal/providers"
	"github.com/stan-ley-tech/axene-pay/internal/providers/testprovider"
	"github.com/stan-ley-tech/axene-pay/internal/reconciliation"
	"github.com/stan-ley-tech/axene-pay/internal/store"
)

// Rates and provider fees here are TEST FIXTURES.

var ctx = context.Background()

const grace = 72 * time.Hour

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	q     *store.Queries
	pay   *payments.Service
	fees  *fees.Service
	rec   *reconciliation.Service
	clock *clock
	pr    merchant.Principal
	mpub  string
	dpub  string
}

func setup(t *testing.T, providerFeeBearer string) *env {
	t.Helper()
	pool := testdb.New(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &clock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	fs, _ := fees.NewService(fees.Deps{Pool: pool, Policy: fees.DefaultPolicy(), Clock: c.Now, Logger: logger})
	reg := providers.NewRegistry(false)
	_ = reg.Register(testprovider.New("s"))
	policy := payments.DefaultPolicy()
	policy.MaxVerificationAttempts = 2
	pay, _ := payments.NewService(payments.Deps{Pool: pool, Providers: reg, Policy: policy, Clock: c.Now, Logger: logger,
		CallbackURL: func(string) string { return "https://example.test/cb" }, Effects: &accounting.Effects{Fees: fs}})
	rec, err := reconciliation.NewService(reconciliation.Deps{Pool: pool, Payments: pay, Fees: fs, SettlementGrace: grace, Clock: c.Now, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := merchant.NewService(pool, []byte(strings.Repeat("p", 32)))
	m, _ := ms.CreateMerchant(ctx, "M")
	d, _ := ms.CreateDestination(ctx, m.PublicID, merchant.Sandbox, "mpesa_paybill", "600123", "")
	_, _ = ms.CreateProviderConfig(ctx, m.PublicID, merchant.Sandbox, "mpesa", "testprovider", d.PublicID, 1)
	k, _ := ms.IssueAPIKey(ctx, m.PublicID, merchant.Sandbox)
	pr, _ := ms.Authenticate(ctx, k.Secret)
	sc, err := fs.CreateSchedule(ctx, fees.ScheduleInput{MerchantID: pgtype.UUID{}, Environment: "sandbox", Method: "mpesa", Currency: "KES",
		Terms: fees.Terms{PercentagePPM: 10_000, Rounding: money.RoundHalfUp}, FeePayer: "merchant", ProviderFeeBearer: providerFeeBearer,
		ReversalPolicy: "pending_approval", EffectiveFrom: c.Now().Add(-time.Hour), Description: "TEST FIXTURE"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Activate(ctx, sc.PublicID, "test-suite", "TEST-FIXTURE"); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, pool: pool, q: store.New(pool), pay: pay, fees: fs, rec: rec, clock: c, pr: pr, mpub: m.PublicID, dpub: d.PublicID}
}

// payment creates a payment and runs verification until it settles.
func (e *env) payment(phone string, minor int64) (store.Payment, store.PaymentAttempt) {
	e.t.Helper()
	a, _ := money.New(minor, money.MustCurrency("KES"))
	res, err := e.pay.Create(ctx, e.pr, idempotency.Key{Value: ids.New(ids.Event), Method: "POST", Path: "/v1/payments"},
		payments.CreateParams{Amount: a, Method: providers.MethodMpesa, PhoneNumber: phone}, "req")
	if err != nil {
		e.t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		e.clock.Advance(10 * time.Minute)
		if _, err := e.pay.VerifyDue(ctx, 10); err != nil {
			e.t.Fatal(err)
		}
	}
	p, _ := e.q.GetPaymentForMerchant(ctx, store.GetPaymentForMerchantParams{PublicID: res.Payment.ID, MerchantID: e.pr.MerchantID, Environment: "sandbox"})
	as, _ := e.q.ListAttemptsForPayment(ctx, p.ID)
	return p, as[len(as)-1]
}

func (e *env) importLines(ref string, lines ...string) store.SettlementBatch {
	e.t.Helper()
	body := strings.Join(reconciliation.StatementHeader, ",") + "\n" + strings.Join(lines, "\n") + "\n"
	b, err := e.rec.Import(ctx, "testprovider", "sandbox", ref, "ops-anna", strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func line(ref, receipt string, gross, providerFee, net int64, axeneFee string) string {
	return fmt.Sprintf("%s,%s,600123,KES,%d,%d,%d,2026-10-13T12:00:00Z,%s", ref, receipt, gross, providerFee, net, axeneFee)
}

func (e *env) balance(book ledger.Book, code string) int64 {
	e.t.Helper()
	b, err := ledger.Balance(ctx, e.q, book, code, money.MustCurrency("KES"))
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func (e *env) openExceptions(kind string) []store.ReconciliationException {
	e.t.Helper()
	all, err := e.rec.Exceptions(ctx, true, 100)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []store.ReconciliationException
	for _, x := range all {
		if x.Type == kind {
			out = append(out, x)
		}
	}
	return out
}

func (e *env) trialBalanced() {
	e.t.Helper()
	rows, _ := e.q.TrialBalance(ctx)
	for _, r := range rows {
		if r.Debits != r.Credits {
			e.t.Fatalf("%s %s unbalanced: %d vs %d", r.Book, r.Currency, r.Debits, r.Credits)
		}
	}
}

func TestMatchedLinePostsProviderFeeAndSettlement(t *testing.T) {
	e := setup(t, "merchant")
	p, a := e.payment("254700000000", 100000)
	if p.Status != "SUCCEEDED" {
		t.Fatalf("payment %s", p.Status)
	}
	e.importLines("stmt-1", line(*a.ProviderReference, "", 100000, 150, 99850, ""))
	res, err := e.rec.Run(ctx, "testprovider", "sandbox", "ops-anna")
	if err != nil || res.Matched != 1 || res.Exceptions != 0 {
		t.Fatalf("run %+v %v", res, err)
	}
	expected := "settlement_expected:" + e.mpub + ":" + e.dpub
	if e.balance(ledger.BookSettlementMemo, expected) != 0 {
		t.Fatalf("expected settlement not cleared: %d", e.balance(ledger.BookSettlementMemo, expected))
	}
	if e.balance(ledger.BookSettlementMemo, "provider_fees_withheld:"+e.mpub) != 150 ||
		e.balance(ledger.BookSettlementMemo, "settled:"+e.mpub+":"+e.dpub) != 99850 {
		t.Fatal("provider fee or settlement not posted")
	}
	// The Axene fee is still a receivable: the statement shows no collection.
	fee, _, _ := e.fees.ForPayment(ctx, p.ID)
	if fee.Status != "accrued" || e.balance(ledger.BookAxene, "fees_receivable:"+e.mpub) != 1000 {
		t.Fatalf("fee %s", fee.Status)
	}
	// Re-running finds nothing new.
	if res, _ := e.rec.Run(ctx, "testprovider", "sandbox", "ops-anna"); res.Matched != 0 || res.Exceptions != 0 {
		t.Fatalf("second run %+v", res)
	}
	e.trialBalanced()
}

func TestDuplicateImportRefused(t *testing.T) {
	e := setup(t, "merchant")
	e.importLines("stmt-1", line("tp_x", "", 100, 0, 100, ""))
	body := strings.Join(reconciliation.StatementHeader, ",") + "\n" + line("tp_x", "", 100, 0, 100, "") + "\n"
	if _, err := e.rec.Import(ctx, "testprovider", "sandbox", "stmt-1-again", "ops-anna", strings.NewReader(body)); !errors.Is(err, reconciliation.ErrDuplicateStatement) {
		t.Fatalf("err = %v", err)
	}
}

func TestDiscrepanciesBecomeExceptions(t *testing.T) {
	e := setup(t, "merchant")
	p1, a1 := e.payment("254700000000", 100000)
	_, a2 := e.payment("254700000000", 50000)
	e.importLines("stmt-1",
		line(*a1.ProviderReference, "", 99999, 0, 99999, ""), // amount mismatch
		line("tp_nobody", "", 500, 0, 500, ""),               // unmatched
		line(*a2.ProviderReference, "", 50000, 0, 50000, ""), // matched
		line(*a2.ProviderReference, "", 50000, 0, 50000, ""), // duplicate of the previous line
	)
	res, err := e.rec.Run(ctx, "testprovider", "sandbox", "ops-anna")
	if err != nil || res.Matched != 1 || res.Exceptions != 3 {
		t.Fatalf("run %+v %v", res, err)
	}
	for _, kind := range []string{"amount_mismatch", "unmatched_statement_line", "duplicate_statement_line"} {
		if n := len(e.openExceptions(kind)); n != 1 {
			t.Errorf("%s exceptions = %d", kind, n)
		}
	}
	if x := e.openExceptions("amount_mismatch"); x[0].PaymentID != p1.ID {
		t.Error("amount mismatch not linked to its payment")
	}
	e.trialBalanced()
}

func TestStatementResolvesUnresolvedPaymentThroughOperator(t *testing.T) {
	e := setup(t, "merchant")
	p, a := e.payment("254700000004", 100000) // status queries never conclusive
	if p.Status != "UNRESOLVED" {
		t.Fatalf("payment %s", p.Status)
	}
	e.importLines("stmt-1", line(*a.ProviderReference, "", 100000, 150, 99850, ""))
	if res, _ := e.rec.Run(ctx, "testprovider", "sandbox", "ops-anna"); res.Exceptions != 1 {
		t.Fatalf("run %+v", res)
	}
	x := e.openExceptions("status_mismatch")
	if len(x) != 1 {
		t.Fatal("no status_mismatch exception")
	}
	if _, err := e.rec.Resolve(ctx, x[0].PublicID, reconciliation.ActionPaymentSucceeded, "", "note"); !errors.Is(err, audit.ErrNoActor) {
		t.Fatalf("resolve without operator: %v", err)
	}
	if _, err := e.rec.Resolve(ctx, x[0].PublicID, reconciliation.ActionPaymentSucceeded, "ops-anna", ""); !errors.Is(err, reconciliation.ErrInvalid) {
		t.Fatalf("resolve without note: %v", err)
	}
	resolved, err := e.rec.Resolve(ctx, x[0].PublicID, reconciliation.ActionPaymentSucceeded, "ops-anna", "statement stmt-1 shows settlement")
	if err != nil || resolved.Status != "resolved" {
		t.Fatalf("resolve: %+v %v", resolved, err)
	}
	p2, _ := e.q.GetPaymentByID(ctx, p.ID)
	if p2.Status != "SUCCEEDED" {
		t.Fatalf("payment after resolution %s", p2.Status)
	}
	trs, _ := e.q.ListTransitionsForPayment(ctx, p.ID)
	last := trs[len(trs)-1]
	if last.Evidence != "reconciliation" || last.Actor != "operator:ops-anna" {
		t.Fatalf("transition %+v", last)
	}
	// Success via reconciliation accrues the fee like any other success.
	if fee, ok, _ := e.fees.ForPayment(ctx, p.ID); !ok || fee.FeeMinor != 1000 {
		t.Fatalf("fee after resolution: %+v %v", fee, ok)
	}
	// A resolved exception cannot be resolved again.
	if _, err := e.rec.Resolve(ctx, x[0].PublicID, reconciliation.ActionRematch, "ops-anna", "again"); !errors.Is(err, reconciliation.ErrInvalid) {
		t.Fatalf("resolving a resolved exception: %v", err)
	}
	// The corrected payment's statement line is matched on the next run.
	if res, _ := e.rec.Run(ctx, "testprovider", "sandbox", "ops-anna"); res.Matched != 1 {
		t.Fatalf("rematch run %+v", res)
	}
	entries, _ := e.q.ListAuditForTarget(ctx, store.ListAuditForTargetParams{TargetType: "reconciliation_exception", TargetID: x[0].PublicID})
	if len(entries) != 1 || entries[0].Actor != "ops-anna" {
		t.Fatalf("audit %+v", entries)
	}
	e.trialBalanced()
}

func TestMissingSettlementAfterGrace(t *testing.T) {
	e := setup(t, "merchant")
	p, _ := e.payment("254700000000", 100000)
	if res, _ := e.rec.Run(ctx, "testprovider", "sandbox", "ops-anna"); res.Exceptions != 0 {
		t.Fatalf("reported missing before grace: %+v", res)
	}
	e.clock.Advance(grace + time.Hour)
	res, _ := e.rec.Run(ctx, "testprovider", "sandbox", "ops-anna")
	if res.Exceptions != 1 || len(e.openExceptions("missing_settlement")) != 1 {
		t.Fatalf("run %+v", res)
	}
	// Reported once, however often reconciliation runs.
	if res, _ := e.rec.Run(ctx, "testprovider", "sandbox", "ops-anna"); res.Exceptions != 0 {
		t.Fatalf("reported twice: %+v", res)
	}
	x := e.openExceptions("missing_settlement")[0]
	if x.PaymentID != p.ID {
		t.Fatal("wrong payment")
	}
	if _, err := e.rec.Resolve(ctx, x.PublicID, reconciliation.ActionAcknowledge, "ops-anna", "settled in next statement"); err != nil {
		t.Fatal(err)
	}
}

func TestStatementEvidenceCollectsFee(t *testing.T) {
	e := setup(t, "merchant")
	p, a := e.payment("254700000000", 100000)
	_, a2 := e.payment("254700000000", 100000)
	e.importLines("stmt-1",
		line(*a.ProviderReference, "RCPT-1", 100000, 150, 98850, "1000"), // provider allocated exactly the accrued fee
		line(*a2.ProviderReference, "RCPT-2", 100000, 150, 98950, "900"), // wrong amount
	)
	res, err := e.rec.Run(ctx, "testprovider", "sandbox", "ops-anna")
	if err != nil || res.Matched != 2 {
		t.Fatalf("run %+v %v", res, err)
	}
	fee, _, _ := e.fees.ForPayment(ctx, p.ID)
	if fee.Status != "collected" || *fee.CollectionReference != "RCPT-1" || !strings.HasPrefix(*fee.CollectionEvidence, "settlement_line:stl_") {
		t.Fatalf("fee %+v", fee)
	}
	if len(e.openExceptions("fee_collection_mismatch")) != 1 {
		t.Fatal("fee mismatch not reported")
	}
	if e.balance(ledger.BookAxene, "fee_collections:provider_statement") != 1000 {
		t.Fatal("collection not posted")
	}
	expected := "settlement_expected:" + e.mpub + ":" + e.dpub
	if e.balance(ledger.BookSettlementMemo, expected) != 0 {
		t.Fatalf("expected settlement %d", e.balance(ledger.BookSettlementMemo, expected))
	}
	e.trialBalanced()
}

func TestAxeneAbsorbedProviderFee(t *testing.T) {
	e := setup(t, "axene")
	_, a := e.payment("254700000000", 100000)
	e.importLines("stmt-1", line(*a.ProviderReference, "", 100000, 150, 99850, ""))
	if res, _ := e.rec.Run(ctx, "testprovider", "sandbox", "ops-anna"); res.Matched != 1 {
		t.Fatalf("run %+v", res)
	}
	if e.balance(ledger.BookAxene, "provider_fee_expense") != 150 || e.balance(ledger.BookAxene, "provider_fee_reimbursement:"+e.mpub) != -150 {
		t.Fatal("absorbed provider fee not posted")
	}
	e.trialBalanced()
}
