//go:build integration

package accounting_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stan-ley-tech/axene-pay/internal/accounting"
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
	"github.com/stan-ley-tech/axene-pay/internal/store"
)

// Every rate in this file is a TEST FIXTURE, not Axene pricing.

var ctx = context.Background()

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
	ms    *merchant.Service
	clock *clock
	pr    merchant.Principal
	mpub  string
	sim   *fees.SimulatedCollector
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testdb.New(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &clock{t: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)}
	sim := &fees.SimulatedCollector{}
	fs, err := fees.NewService(fees.Deps{Pool: pool, Collectors: []fees.Collector{sim}, Policy: fees.DefaultPolicy(), Clock: c.Now, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	reg := providers.NewRegistry(false)
	_ = reg.Register(testprovider.New("s"))
	pay, err := payments.NewService(payments.Deps{Pool: pool, Providers: reg, Policy: payments.DefaultPolicy(), Clock: c.Now, Logger: logger,
		CallbackURL: func(string) string { return "https://example.test/cb" }, Effects: &accounting.Effects{Fees: fs}})
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := merchant.NewService(pool, []byte(strings.Repeat("p", 32)))
	e := &env{t: t, pool: pool, q: store.New(pool), pay: pay, fees: fs, ms: ms, clock: c, sim: sim}
	m, _ := ms.CreateMerchant(ctx, "M")
	d, _ := ms.CreateDestination(ctx, m.PublicID, merchant.Sandbox, "mpesa_paybill", "600123", "")
	if _, err := ms.CreateProviderConfig(ctx, m.PublicID, merchant.Sandbox, "mpesa", "testprovider", d.PublicID, 1); err != nil {
		t.Fatal(err)
	}
	k, _ := ms.IssueAPIKey(ctx, m.PublicID, merchant.Sandbox)
	e.pr, _ = ms.Authenticate(ctx, k.Secret)
	e.mpub = m.PublicID
	return e
}

func ptr[T any](v T) *T { return &v }

// schedule creates and activates a TEST FIXTURE schedule.
func (e *env) schedule(merchantID pgtype.UUID, provider string, ppm, fixed int64, policy string, from time.Time) store.FeeSchedule {
	e.t.Helper()
	sc, err := e.fees.CreateSchedule(ctx, fees.ScheduleInput{
		MerchantID: merchantID, Environment: "sandbox", Method: "mpesa", Provider: provider, Currency: "KES",
		Terms:    fees.Terms{PercentagePPM: ppm, Fixed: fixed, Rounding: money.RoundHalfUp},
		FeePayer: "merchant", ProviderFeeBearer: "merchant", ReversalPolicy: policy, EffectiveFrom: from,
		Description: "TEST FIXTURE - not approved pricing",
	})
	if err != nil {
		e.t.Fatal(err)
	}
	sc, err = e.fees.Activate(ctx, sc.PublicID, "test-suite", "TEST-FIXTURE")
	if err != nil {
		e.t.Fatal(err)
	}
	return sc
}

func (e *env) succeed(phone string, minor int64) store.Payment {
	e.t.Helper()
	a, _ := money.New(minor, money.MustCurrency("KES"))
	res, err := e.pay.Create(ctx, e.pr, idempotency.Key{Value: ids.New(ids.Event), Method: "POST", Path: "/v1/payments"},
		payments.CreateParams{Amount: a, Method: providers.MethodMpesa, PhoneNumber: phone}, "req")
	if err != nil {
		e.t.Fatal(err)
	}
	e.clock.Advance(payments.DefaultPolicy().InitialVerificationDelay)
	if _, err := e.pay.VerifyDue(ctx, 10); err != nil {
		e.t.Fatal(err)
	}
	p, err := e.q.GetPaymentForMerchant(ctx, store.GetPaymentForMerchantParams{PublicID: res.Payment.ID, MerchantID: e.pr.MerchantID, Environment: "sandbox"})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) balance(book ledger.Book, code string) int64 {
	e.t.Helper()
	b, err := ledger.Balance(ctx, e.q, book, code, money.MustCurrency("KES"))
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func (e *env) trialBalanced() {
	e.t.Helper()
	rows, err := e.q.TrialBalance(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, r := range rows {
		if r.Debits != r.Credits {
			e.t.Fatalf("book %s %s unbalanced: %d vs %d", r.Book, r.Currency, r.Debits, r.Credits)
		}
	}
}

func pgCode(err error) string {
	var p *pgconn.PgError
	if errors.As(err, &p) {
		return p.Code
	}
	return ""
}

func TestScheduleLifecycleRules(t *testing.T) {
	e := setup(t)
	draft, err := e.fees.CreateSchedule(ctx, fees.ScheduleInput{Environment: "sandbox", Method: "mpesa", Currency: "KES",
		Terms: fees.Terms{PercentagePPM: 10_000, Rounding: money.RoundHalfUp}, FeePayer: "merchant", ProviderFeeBearer: "merchant",
		ReversalPolicy: "pending_approval", EffectiveFrom: e.clock.Now(), Description: "TEST FIXTURE"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.fees.Activate(ctx, draft.PublicID, "", ""); !errors.Is(err, fees.ErrInvalid) {
		t.Fatalf("activation without approval: %v", err)
	}
	active, err := e.fees.Activate(ctx, draft.PublicID, "finance-lead", "TEST-FIXTURE")
	if err != nil || active.Status != "active" || *active.ApprovedBy != "finance-lead" {
		t.Fatalf("activate: %+v %v", active, err)
	}
	// Terms are immutable once active.
	if _, err := e.pool.Exec(ctx, `UPDATE fee_schedules SET percentage_ppm = 1 WHERE id = $1`, active.ID); pgCode(err) != "42501" {
		t.Fatalf("active terms mutable: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `DELETE FROM fee_schedules WHERE id = $1`, active.ID); pgCode(err) != "42501" {
		t.Fatalf("active schedule deletable: %v", err)
	}
	// An overlapping active schedule for the same scope is rejected.
	dup, _ := e.fees.CreateSchedule(ctx, fees.ScheduleInput{Environment: "sandbox", Method: "mpesa", Currency: "KES",
		Terms: fees.Terms{PercentagePPM: 20_000, Rounding: money.RoundHalfUp}, FeePayer: "merchant", ProviderFeeBearer: "merchant",
		ReversalPolicy: "pending_approval", EffectiveFrom: e.clock.Now().Add(time.Hour), Description: "TEST FIXTURE"})
	if _, err := e.fees.Activate(ctx, dup.PublicID, "x", "y"); !errors.Is(err, fees.ErrOverlap) {
		t.Fatalf("overlap: %v", err)
	}
	// Ending the first schedule lets the new version take over.
	if _, err := e.fees.End(ctx, active.PublicID, e.clock.Now().Add(time.Hour), "test-suite"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.fees.Activate(ctx, dup.PublicID, "x", "y"); err != nil {
		t.Fatalf("new version after end: %v", err)
	}
	// Customer-borne fees cannot be activated without an approved flow.
	cust, _ := e.fees.CreateSchedule(ctx, fees.ScheduleInput{MerchantID: e.pr.MerchantID, Environment: "sandbox", Method: "mpesa", Currency: "KES",
		Terms: fees.Terms{Fixed: 100, Rounding: money.RoundHalfUp}, FeePayer: "customer", ProviderFeeBearer: "merchant",
		ReversalPolicy: "pending_approval", EffectiveFrom: e.clock.Now(), Description: "TEST FIXTURE"})
	if _, err := e.fees.Activate(ctx, cust.PublicID, "x", "y"); !errors.Is(err, fees.ErrNotApproved) {
		t.Fatalf("customer payer: %v", err)
	}
	for name, in := range map[string]fees.ScheduleInput{
		"bad payer":  {Environment: "sandbox", Method: "mpesa", Currency: "KES", Terms: fees.Terms{Rounding: money.RoundUp}, FeePayer: "bank", ProviderFeeBearer: "merchant", ReversalPolicy: "retain_fee", EffectiveFrom: e.clock.Now()},
		"bad policy": {Environment: "sandbox", Method: "mpesa", Currency: "KES", Terms: fees.Terms{Rounding: money.RoundUp}, FeePayer: "merchant", ProviderFeeBearer: "merchant", ReversalPolicy: "keep", EffectiveFrom: e.clock.Now()},
		"bad terms":  {Environment: "sandbox", Method: "mpesa", Currency: "KES", Terms: fees.Terms{PercentagePPM: -1, Rounding: money.RoundUp}, FeePayer: "merchant", ProviderFeeBearer: "merchant", ReversalPolicy: "retain_fee", EffectiveFrom: e.clock.Now()},
		"currency":   {Environment: "sandbox", Method: "mpesa", Currency: "XXX", Terms: fees.Terms{Rounding: money.RoundUp}, FeePayer: "merchant", ProviderFeeBearer: "merchant", ReversalPolicy: "retain_fee", EffectiveFrom: e.clock.Now()},
	} {
		if _, err := e.fees.CreateSchedule(ctx, in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestSuccessPostsSettlementMemoAndAccruesFeeOnce(t *testing.T) {
	e := setup(t)
	e.schedule(pgtype.UUID{}, "", 10_000, 0, "pending_approval", e.clock.Now().Add(-time.Hour))                               // platform default 1%
	merchantSc := e.schedule(e.pr.MerchantID, "testprovider", 15_000, 500, "pending_approval", e.clock.Now().Add(-time.Hour)) // merchant+provider wins

	p := e.succeed("254700000000", 100000)
	if p.Status != "SUCCEEDED" {
		t.Fatalf("payment %s", p.Status)
	}
	fee, ok, err := e.fees.ForPayment(ctx, p.ID)
	if err != nil || !ok {
		t.Fatalf("no fee: %v", err)
	}
	if fee.ScheduleID != merchantSc.ID || fee.FeeMinor != 2000 || fee.PercentageMinor != 1500 || fee.FixedMinor != 500 || fee.Status != "accrued" {
		t.Fatalf("fee %+v", fee)
	}
	if b := e.balance(ledger.BookAxene, "fees_receivable:"+e.mpub); b != 2000 {
		t.Fatalf("receivable %d", b)
	}
	if b := e.balance(ledger.BookAxene, "fee_revenue"); b != -2000 {
		t.Fatalf("revenue %d", b)
	}
	if b := e.balance(ledger.BookSettlementMemo, "processed_volume:"+e.mpub); b != -100000 {
		t.Fatalf("processed volume %d", b)
	}
	// Duplicate and late callbacks cannot accrue again.
	attempts, _ := e.q.ListAttemptsForPayment(ctx, p.ID)
	body, _ := json.Marshal(map[string]string{"notification_id": "dup", "reference": *attempts[0].ProviderReference, "result": "succeeded"})
	for i := 0; i < 3; i++ {
		if err := e.pay.IngestCallback(ctx, "testprovider", providers.CallbackRequest{Header: http.Header{testprovider.SecretHeader: {"s"}}, Body: body}, "r"); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := e.q.ListJournalEntriesForSource(ctx, store.ListJournalEntriesForSourceParams{SourceType: "fee", SourceID: fee.PublicID})
	if len(entries) != 1 {
		t.Fatalf("fee entries %d", len(entries))
	}
	e.trialBalanced()
}

func TestConcurrentVerificationAccruesOneFee(t *testing.T) {
	e := setup(t)
	e.schedule(pgtype.UUID{}, "", 10_000, 0, "pending_approval", e.clock.Now().Add(-time.Hour))
	a, _ := money.New(50000, money.MustCurrency("KES"))
	res, err := e.pay.Create(ctx, e.pr, idempotency.Key{Value: "k", Method: "POST", Path: "/v1/payments"},
		payments.CreateParams{Amount: a, Method: providers.MethodMpesa, PhoneNumber: "254700000000"}, "req")
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(payments.DefaultPolicy().InitialVerificationDelay)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.pay.VerifyDue(ctx, 10)
		}()
	}
	wg.Wait()
	var feesN, entries int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM axene_fees`).Scan(&feesN)
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE posting_reason IN ('fee_accrued','payment_succeeded')`).Scan(&entries)
	if feesN != 1 || entries != 2 {
		t.Fatalf("fees=%d entries=%d for %s", feesN, entries, res.Payment.ID)
	}
	e.trialBalanced()
}

func TestHistoricalPaymentsKeepTheirSchedule(t *testing.T) {
	e := setup(t)
	old := e.schedule(e.pr.MerchantID, "", 10_000, 0, "pending_approval", e.clock.Now().Add(-time.Hour))
	a, _ := money.New(100000, money.MustCurrency("KES"))
	res, _ := e.pay.Create(ctx, e.pr, idempotency.Key{Value: "k1", Method: "POST", Path: "/v1/payments"},
		payments.CreateParams{Amount: a, Method: providers.MethodMpesa, PhoneNumber: "254700000000"}, "req")
	// New pricing takes effect before the earlier payment is verified.
	switchAt := e.clock.Now().Add(time.Second)
	if _, err := e.fees.End(ctx, old.PublicID, switchAt, "test-suite"); err != nil {
		t.Fatal(err)
	}
	e.schedule(e.pr.MerchantID, "", 30_000, 0, "pending_approval", switchAt)
	e.clock.Advance(payments.DefaultPolicy().InitialVerificationDelay)
	if _, err := e.pay.VerifyDue(ctx, 10); err != nil {
		t.Fatal(err)
	}
	p, _ := e.q.GetPaymentForMerchant(ctx, store.GetPaymentForMerchantParams{PublicID: res.Payment.ID, MerchantID: e.pr.MerchantID, Environment: "sandbox"})
	fee, _, _ := e.fees.ForPayment(ctx, p.ID)
	if fee.ScheduleID != old.ID || fee.FeeMinor != 1000 {
		t.Fatalf("historical payment priced with %v at %d, want old schedule at 1000", fee.ScheduleID, fee.FeeMinor)
	}
	newP := e.succeed("254700000000", 100000)
	newFee, _, _ := e.fees.ForPayment(ctx, newP.ID)
	if newFee.FeeMinor != 3000 {
		t.Fatalf("new payment fee %d, want 3000", newFee.FeeMinor)
	}
}

func TestMissingScheduleOpensExceptionInsteadOfZeroFee(t *testing.T) {
	e := setup(t)
	p := e.succeed("254700000000", 100000)
	if _, ok, _ := e.fees.ForPayment(ctx, p.ID); ok {
		t.Fatal("fee accrued without a schedule")
	}
	ex, _ := e.q.ListReconExceptionsForPayment(ctx, p.ID)
	if len(ex) != 1 || ex[0].Type != "fee_schedule_missing" {
		t.Fatalf("exceptions %+v", ex)
	}
	// The memo posting still happened: the payment did succeed.
	if b := e.balance(ledger.BookSettlementMemo, "processed_volume:"+e.mpub); b != -100000 {
		t.Fatalf("memo %d", b)
	}
}

func TestCollectionRequiresAMechanismAndEvidence(t *testing.T) {
	e := setup(t)
	e.schedule(pgtype.UUID{}, "", 10_000, 0, "pending_approval", e.clock.Now().Add(-time.Hour))
	p := e.succeed("254700000000", 100000)

	// No mechanism configured: the fee stays an uncollected receivable.
	if n, _ := e.fees.CollectDue(ctx, 10); n != 0 {
		t.Fatalf("collected %d without a mechanism", n)
	}
	fee, _, _ := e.fees.ForPayment(ctx, p.ID)
	if fee.Status != "accrued" || fee.NextCollectionAt != nil {
		t.Fatalf("fee %+v", fee)
	}
	// Mechanisms the schema does not permit are refused.
	if err := e.fees.SetMechanism(ctx, e.pr.MerchantID, "sandbox", "provider_split", "test-suite"); !errors.Is(err, fees.ErrMechanismUnavailable) {
		t.Fatalf("provider_split: %v", err)
	}

	// The simulated collector fails once, is inconclusive once, then succeeds.
	calls := 0
	e.sim.Script = func(r fees.CollectRequest) fees.CollectOutcome {
		calls++
		switch calls {
		case 1:
			return fees.CollectOutcome{Class: fees.CollectFailed, Message: "simulated failure"}
		case 2:
			return fees.CollectOutcome{Class: fees.CollectUnknown}
		}
		return fees.CollectOutcome{Class: fees.CollectSucceeded, Reference: "SIM-REF-1"}
	}
	if err := e.fees.SetMechanism(ctx, e.pr.MerchantID, "sandbox", fees.MechanismSimulated, "test-suite"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := e.fees.CollectDue(ctx, 10); err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(2 * time.Hour)
	}
	fee, _, _ = e.fees.ForPayment(ctx, p.ID)
	if fee.Status != "collected" || *fee.CollectionReference != "SIM-REF-1" || *fee.CollectionEvidence != "collector:simulated:SIM-REF-1" || fee.CollectionAttempts != 3 {
		t.Fatalf("fee %+v", fee)
	}
	if calls != 3 {
		t.Fatalf("collector called %d times after success", calls)
	}
	if e.balance(ledger.BookAxene, "fees_receivable:"+e.mpub) != 0 || e.balance(ledger.BookAxene, "fee_collections:simulated") != 1000 {
		t.Fatal("collection not posted")
	}
	trs, _ := e.fees.Transitions(ctx, fee.ID)
	var path []string
	for _, tr := range trs {
		path = append(path, tr.ToStatus)
	}
	if strings.Join(path, ",") != "accrued,collection_pending,collection_failed,collection_pending,collected" {
		t.Fatalf("fee path %v", path)
	}
	e.trialBalanced()
}

func TestSimulatedCollectorRefusedInProduction(t *testing.T) {
	_, err := fees.NewService(fees.Deps{Pool: testdb.New(t), Collectors: []fees.Collector{fees.SimulatedCollector{}}, Production: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if !errors.Is(err, fees.ErrMechanismUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func reverse(t *testing.T, e *env, p store.Payment) fees.ReversalOutcome {
	t.Helper()
	var out fees.ReversalOutcome
	if err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		var err error
		out, err = e.fees.ApplyReversal(ctx, store.New(tx), p.ID, e.mpub, "rev")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReversalPolicies(t *testing.T) {
	e := setup(t)
	sc := e.schedule(pgtype.UUID{}, "", 10_000, 0, "refund_fee", e.clock.Now().Add(-time.Hour))
	refunded := e.succeed("254700000000", 100000)
	if out := reverse(t, e, refunded); out != fees.ReversalFeeRefunded {
		t.Fatalf("refund_fee: %s", out)
	}
	if b := e.balance(ledger.BookAxene, "fees_receivable:"+e.mpub); b != 0 {
		t.Fatalf("receivable after refund %d", b)
	}

	// Switch the platform policy to retain_fee.
	if _, err := e.fees.End(ctx, sc.PublicID, e.clock.Now(), "test-suite"); err != nil {
		t.Fatal(err)
	}
	e.schedule(pgtype.UUID{}, "", 10_000, 0, "retain_fee", e.clock.Now())
	e.clock.Advance(time.Second)
	retained := e.succeed("254700000000", 100000)
	if out := reverse(t, e, retained); out != fees.ReversalFeeRetained {
		t.Fatalf("retain_fee: %s", out)
	}
	f, _, _ := e.fees.ForPayment(ctx, retained.ID)
	if f.Status != "accrued" {
		t.Fatalf("retained fee %s", f.Status)
	}

	// A collected fee always needs review, whatever the policy.
	_ = e.fees.SetMechanism(ctx, e.pr.MerchantID, "sandbox", fees.MechanismSimulated, "test-suite")
	e.sim.Script = nil
	if _, err := e.fees.CollectDue(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if out := reverse(t, e, retained); out != fees.ReversalReviewRequired {
		t.Fatalf("collected fee reversal: %s", out)
	}
	e.trialBalanced()
}

func TestWaiveReversesAccrual(t *testing.T) {
	e := setup(t)
	e.schedule(pgtype.UUID{}, "", 10_000, 0, "pending_approval", e.clock.Now().Add(-time.Hour))
	p := e.succeed("254700000000", 100000)
	fee, _, _ := e.fees.ForPayment(ctx, p.ID)
	if _, err := e.fees.Waive(ctx, fee.PublicID, "", "", "r"); !errors.Is(err, fees.ErrInvalid) {
		t.Fatalf("waive without operator: %v", err)
	}
	w, err := e.fees.Waive(ctx, fee.PublicID, "ops-anna", "goodwill (test)", "r")
	if err != nil || w.Status != "waived" {
		t.Fatalf("waive: %+v %v", w, err)
	}
	if e.balance(ledger.BookAxene, "fees_receivable:"+e.mpub) != 0 || e.balance(ledger.BookAxene, "fee_revenue") != 0 {
		t.Fatal("waiver not reversed in ledger")
	}
	if _, err := e.fees.Waive(ctx, fee.PublicID, "ops-anna", "again", "r"); !errors.Is(err, fees.ErrInvalidTransition) {
		t.Fatalf("double waive: %v", err)
	}
	e.trialBalanced()
}

var _ = ptr[int]
