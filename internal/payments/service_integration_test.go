//go:build integration

package payments_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stan-ley-tech/axene-pay/internal/idempotency"
	"github.com/stan-ley-tech/axene-pay/internal/merchant"
	"github.com/stan-ley-tech/axene-pay/internal/payments"
	"github.com/stan-ley-tech/axene-pay/internal/platform/ids"
	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
	"github.com/stan-ley-tech/axene-pay/internal/platform/testdb"
	"github.com/stan-ley-tech/axene-pay/internal/providers"
	"github.com/stan-ley-tech/axene-pay/internal/providers/testprovider"
	"github.com/stan-ley-tech/axene-pay/internal/store"
)

var ctx = context.Background()

const secret = "callback-secret"

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

// noStatusQuery wraps the simulated provider without the status_query capability.
type noStatusQuery struct{ *testprovider.Provider }

func (noStatusQuery) Name() string { return "noquery" }
func (noStatusQuery) Capabilities() providers.CapabilitySet {
	return providers.CapabilitySet{providers.CapCollect}
}

type env struct {
	t        *testing.T
	pool     *pgxpool.Pool
	q        *store.Queries
	svc      *payments.Service
	merchSvc *merchant.Service
	tp       *testprovider.Provider
	clock    *clock
	pr       merchant.Principal
	policy   payments.Policy
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testdb.New(t)
	tp := testprovider.New(secret)
	reg := providers.NewRegistry(false)
	if err := reg.Register(tp); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(noStatusQuery{testprovider.New(secret)}); err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	policy := payments.DefaultPolicy()
	policy.MaxVerificationAttempts = 4
	svc, err := payments.NewService(payments.Deps{
		Pool: pool, Providers: reg, Policy: policy, Clock: c.Now,
		CallbackURL: func(p string) string { return "https://example.test/callbacks/" + p },
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ms, err := merchant.NewService(pool, []byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, pool: pool, q: store.New(pool), svc: svc, merchSvc: ms, tp: tp, clock: c, policy: policy}
	e.pr = e.newMerchant("testprovider")
	return e
}

// newMerchant creates a merchant with a sandbox destination routed to provider.
func (e *env) newMerchant(provider string) merchant.Principal {
	e.t.Helper()
	m, err := e.merchSvc.CreateMerchant(ctx, "M")
	if err != nil {
		e.t.Fatal(err)
	}
	d, err := e.merchSvc.CreateDestination(ctx, m.PublicID, merchant.Sandbox, "mpesa_paybill", "600123", "")
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.merchSvc.CreateProviderConfig(ctx, m.PublicID, merchant.Sandbox, "mpesa", provider, d.PublicID, 10); err != nil {
		e.t.Fatal(err)
	}
	key, err := e.merchSvc.IssueAPIKey(ctx, m.PublicID, merchant.Sandbox)
	if err != nil {
		e.t.Fatal(err)
	}
	pr, err := e.merchSvc.Authenticate(ctx, key.Secret)
	if err != nil {
		e.t.Fatal(err)
	}
	return pr
}

func params(t *testing.T, phone string, minor int64) payments.CreateParams {
	t.Helper()
	a, err := money.New(minor, money.MustCurrency("KES"))
	if err != nil {
		t.Fatal(err)
	}
	return payments.CreateParams{Amount: a, Method: providers.MethodMpesa, PhoneNumber: phone, Description: "order 1"}
}

func key(v string) idempotency.Key {
	return idempotency.Key{Value: v, Method: "POST", Path: "/v1/payments"}
}

func (e *env) create(phone string) payments.View {
	e.t.Helper()
	res, err := e.svc.Create(ctx, e.pr, key(ids.New(ids.Event)), params(e.t, phone, 10000), "req-create")
	if err != nil {
		e.t.Fatal(err)
	}
	if res.Payment == nil {
		e.t.Fatal("no payment created")
	}
	return *res.Payment
}

func (e *env) get(id string) payments.View {
	e.t.Helper()
	v, err := e.svc.Get(ctx, e.pr, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

func (e *env) row(id string) (store.Payment, store.PaymentAttempt) {
	e.t.Helper()
	p, err := e.q.GetPaymentForMerchant(ctx, store.GetPaymentForMerchantParams{PublicID: id, MerchantID: e.pr.MerchantID, Environment: string(e.pr.Environment)})
	if err != nil {
		e.t.Fatal(err)
	}
	as, err := e.q.ListAttemptsForPayment(ctx, p.ID)
	if err != nil || len(as) == 0 {
		e.t.Fatalf("attempts: %v %v", as, err)
	}
	return p, as[len(as)-1]
}

func (e *env) callback(providerName, ref, notification, result string) error {
	body, _ := json.Marshal(map[string]any{"notification_id": notification, "reference": ref, "result": result})
	return e.svc.IngestCallback(ctx, providerName, providers.CallbackRequest{
		Header: http.Header{testprovider.SecretHeader: {secret}}, Body: body,
	}, "req-cb-"+notification)
}

// verifyUntilQuiet runs the worker's verification step, advancing the clock,
// until nothing is due.
func (e *env) verifyUntilQuiet() {
	e.t.Helper()
	for i := 0; i < 50; i++ {
		e.clock.Advance(e.policy.RetryMax + time.Second)
		n, err := e.svc.VerifyDue(ctx, 100)
		if err != nil {
			e.t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	e.t.Fatal("verification never settled")
}

func (e *env) transitions(id string) []string {
	e.t.Helper()
	p, _ := e.row(id)
	ts, err := e.q.ListTransitionsForPayment(ctx, p.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	out := make([]string, len(ts))
	for i, tr := range ts {
		out[i] = fmt.Sprintf("%s>%s:%s", tr.FromStatus, tr.ToStatus, tr.Evidence)
	}
	return out
}

func (e *env) events(id string) []store.OutboxEvent {
	e.t.Helper()
	evs, err := e.q.ListOutboxEventsForAggregate(ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return evs
}

func (e *env) exceptions(id string) []string {
	e.t.Helper()
	p, _ := e.row(id)
	ex, err := e.q.ListReconExceptionsForPayment(ctx, p.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	out := make([]string, len(ex))
	for i, x := range ex {
		out[i] = x.Type
	}
	return out
}

func equal(t *testing.T, what string, got, want any) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func TestSuccessRequiresVerificationAfterCallback(t *testing.T) {
	e := setup(t)
	v := e.create("254700000000")
	equal(t, "status after create", v.Status, payments.StatusPendingProvider)
	equal(t, "destination", v.Destination.Identifier, "600123")
	equal(t, "masked phone", v.PhoneNumberMasked, "25470****000")
	_, a := e.row(v.ID)
	equal(t, "attempt", a.Status, "SUBMITTED")

	if err := e.callback("testprovider", *a.ProviderReference, "n1", "succeeded"); err != nil {
		t.Fatal(err)
	}
	// A callback alone never confirms a payment.
	equal(t, "status after callback", e.get(v.ID).Status, payments.StatusPendingVerification)

	// Verification is due immediately after a callback.
	if n, err := e.svc.VerifyDue(ctx, 10); err != nil || n != 1 {
		t.Fatalf("VerifyDue = %d, %v", n, err)
	}
	final := e.get(v.ID)
	equal(t, "final status", final.Status, payments.StatusSucceeded)
	equal(t, "attempt status", final.Attempts[0].Status, payments.AttemptConfirmedSuccess)
	equal(t, "transitions", e.transitions(v.ID), []string{
		"CREATED>PENDING_PROVIDER:submission",
		"PENDING_PROVIDER>PENDING_VERIFICATION:callback",
		"PENDING_VERIFICATION>SUCCEEDED:status_query",
	})
	evs := e.events(v.ID)
	equal(t, "event count", len(evs), 3)
	equal(t, "last event", evs[2].EventType, "payment.succeeded")
	if strings.Contains(string(evs[2].Payload), "2547") {
		t.Fatalf("event payload contains phone number: %s", evs[2].Payload)
	}
	_, a = e.row(v.ID)
	if a.ProviderReceipt == nil || a.NextVerificationAt != nil || a.ResolvedAt == nil {
		t.Fatalf("attempt not resolved cleanly: %+v", a)
	}
}

func TestDuplicateCallbacksHaveNoSideEffects(t *testing.T) {
	e := setup(t)
	v := e.create("254700000000")
	_, a := e.row(v.ID)
	for i := 0; i < 3; i++ {
		if err := e.callback("testprovider", *a.ProviderReference, "same", "succeeded"); err != nil {
			t.Fatal(err)
		}
	}
	e.verifyUntilQuiet()
	// Re-delivery after the payment is final is also inert.
	if err := e.callback("testprovider", *a.ProviderReference, "same", "succeeded"); err != nil {
		t.Fatal(err)
	}
	equal(t, "transitions", len(e.transitions(v.ID)), 3)
	equal(t, "events", len(e.events(v.ID)), 3)
	cbs, _ := e.q.ListCallbacksForAttempt(ctx, a.ID)
	equal(t, "callbacks stored", len(cbs), 4)
	dups := 0
	for _, c := range cbs {
		if c.DuplicateOf.Valid {
			dups++
		}
	}
	equal(t, "duplicates", dups, 3)
	equal(t, "exceptions", len(e.exceptions(v.ID)), 0)
}

func TestDeclinedPaymentFailsOnlyAfterVerification(t *testing.T) {
	e := setup(t)
	v := e.create("254700000001")
	_, a := e.row(v.ID)
	if err := e.callback("testprovider", *a.ProviderReference, "n1", "declined"); err != nil {
		t.Fatal(err)
	}
	equal(t, "after callback", e.get(v.ID).Status, payments.StatusPendingVerification)
	e.verifyUntilQuiet()
	equal(t, "final", e.get(v.ID).Status, payments.StatusFailed)
}

func TestSubmissionTimeoutIsResolvedByStatusQueryWithoutResubmitting(t *testing.T) {
	e := setup(t)
	v := e.create("254700000002")
	equal(t, "status", v.Status, payments.StatusPendingProvider)
	_, a := e.row(v.ID)
	equal(t, "attempt", a.Status, "SUBMISSION_UNKNOWN")

	// Nothing is due before the initial delay.
	if n, _ := e.svc.VerifyDue(ctx, 10); n != 0 {
		t.Fatalf("verified %d before delay", n)
	}
	e.clock.Advance(e.policy.InitialVerificationDelay)
	if _, err := e.svc.VerifyDue(ctx, 10); err != nil {
		t.Fatal(err)
	}
	equal(t, "final", e.get(v.ID).Status, payments.StatusSucceeded)
	equal(t, "transitions", e.transitions(v.ID), []string{
		"CREATED>PENDING_PROVIDER:submission",
		"PENDING_PROVIDER>SUCCEEDED:status_query",
	})
	equal(t, "provider submissions", e.tp.SubmitCount(), 1)
}

func TestSynchronousRejectionFails(t *testing.T) {
	e := setup(t)
	v := e.create("254700000003")
	equal(t, "status", v.Status, payments.StatusFailed)
	equal(t, "attempt", v.Attempts[0].Status, payments.AttemptRejected)
	equal(t, "transitions", e.transitions(v.ID), []string{"CREATED>FAILED:submission_rejected"})
}

func TestInconclusiveVerificationBecomesUnresolvedNotFailed(t *testing.T) {
	e := setup(t)
	v := e.create("254700000004")
	_, a := e.row(v.ID)
	if err := e.callback("testprovider", *a.ProviderReference, "n1", "succeeded"); err != nil {
		t.Fatal(err)
	}
	e.verifyUntilQuiet()
	equal(t, "final", e.get(v.ID).Status, payments.StatusUnresolved)
	_, a = e.row(v.ID)
	equal(t, "verification count", a.VerificationCount, e.policy.MaxVerificationAttempts)
	equal(t, "exceptions", e.exceptions(v.ID), []string{"verification_exhausted"})
}

func TestStillPendingAtProviderBecomesUnresolvedWithoutCallback(t *testing.T) {
	e := setup(t)
	v := e.create("254700000006")
	e.verifyUntilQuiet()
	equal(t, "final", e.get(v.ID).Status, payments.StatusUnresolved)
	equal(t, "transitions", e.transitions(v.ID), []string{
		"CREATED>PENDING_PROVIDER:submission",
		"PENDING_PROVIDER>UNRESOLVED:verification_exhausted",
	})
}

func TestAmountMismatchIsUnresolved(t *testing.T) {
	e := setup(t)
	v := e.create("254700000005")
	_, a := e.row(v.ID)
	if err := e.callback("testprovider", *a.ProviderReference, "n1", "succeeded"); err != nil {
		t.Fatal(err)
	}
	e.verifyUntilQuiet()
	equal(t, "final", e.get(v.ID).Status, payments.StatusUnresolved)
	equal(t, "exceptions", e.exceptions(v.ID), []string{"evidence_mismatch"})
}

func TestCallbackContradictingStatusQueryIsUnresolved(t *testing.T) {
	e := setup(t)
	v := e.create("254700000001") // status query says declined
	_, a := e.row(v.ID)
	if err := e.callback("testprovider", *a.ProviderReference, "n1", "succeeded"); err != nil {
		t.Fatal(err)
	}
	e.verifyUntilQuiet()
	equal(t, "final", e.get(v.ID).Status, payments.StatusUnresolved)
	equal(t, "exceptions", e.exceptions(v.ID), []string{"evidence_conflict"})
}

func TestLateContradictingCallbackOpensExceptionAndKeepsTerminalState(t *testing.T) {
	e := setup(t)
	v := e.create("254700000001")
	e.verifyUntilQuiet()
	equal(t, "status", e.get(v.ID).Status, payments.StatusFailed)
	_, a := e.row(v.ID)
	if err := e.callback("testprovider", *a.ProviderReference, "late", "succeeded"); err != nil {
		t.Fatal(err)
	}
	equal(t, "still failed", e.get(v.ID).Status, payments.StatusFailed)
	equal(t, "exceptions", e.exceptions(v.ID), []string{"late_callback_conflict"})
}

func TestUnmatchedAndRejectedCallbacksAreStored(t *testing.T) {
	e := setup(t)
	if err := e.callback("testprovider", "tp_unknown", "n1", "succeeded"); err != nil {
		t.Fatal(err)
	}
	open, err := e.q.ListOpenReconExceptions(ctx, 10)
	if err != nil || len(open) != 1 || open[0].Type != "unmatched_callback" {
		t.Fatalf("exceptions = %+v, %v", open, err)
	}
	err = e.svc.IngestCallback(ctx, "testprovider", providers.CallbackRequest{
		Header: http.Header{testprovider.SecretHeader: {"forged"}}, Body: []byte(`{"notification_id":"x","reference":"r","result":"succeeded"}`),
	}, "req-forged")
	if !errors.Is(err, providers.ErrCallbackRejected) {
		t.Fatalf("forged callback err = %v", err)
	}
	var rejected int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM provider_callbacks WHERE parse_status = 'rejected'`).Scan(&rejected); err != nil || rejected != 1 {
		t.Fatalf("rejected callbacks stored = %d, %v", rejected, err)
	}
	if err := e.svc.IngestCallback(ctx, "nope", providers.CallbackRequest{}, "r"); !errors.Is(err, payments.ErrUnknownProvider) {
		t.Fatalf("unknown provider: %v", err)
	}
}

func TestProviderWithoutStatusQueryNeverConfirmsFromCallback(t *testing.T) {
	e := setup(t)
	e.pr = e.newMerchant("noquery")
	v := e.create("254700000000")
	_, a := e.row(v.ID)
	equal(t, "no verification scheduled", a.NextVerificationAt == nil, true)
	if err := e.callback("noquery", *a.ProviderReference, "n1", "succeeded"); err != nil {
		t.Fatal(err)
	}
	e.verifyUntilQuiet()
	equal(t, "final", e.get(v.ID).Status, payments.StatusUnresolved)
	equal(t, "exceptions", e.exceptions(v.ID), []string{"verification_exhausted"})
}

func TestIdempotentCreate(t *testing.T) {
	e := setup(t)
	k := key("order-42")
	p := params(t, "254700000000", 5000)
	first, err := e.svc.Create(ctx, e.pr, k, p, "r1")
	if err != nil || first.Payment == nil {
		t.Fatalf("first: %+v %v", first, err)
	}
	// Before completion, an identical request is in progress.
	if _, err := e.svc.Create(ctx, e.pr, k, p, "r2"); !errors.Is(err, payments.ErrIdempotencyInProgress) {
		t.Fatalf("in progress: %v", err)
	}
	if err := e.svc.CompleteIdempotency(ctx, first.Key, 201, []byte(`{"id":"`+first.Payment.ID+`"}`)); err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.Create(ctx, e.pr, k, p, "r3")
	if err != nil || again.Replay == nil || again.Payment != nil {
		t.Fatalf("replay: %+v %v", again, err)
	}
	equal(t, "replayed status", *again.Replay.ResponseStatus, 201)
	equal(t, "replayed body", string(again.Replay.ResponseBody), `{"id":"`+first.Payment.ID+`"}`)

	other := p
	other.Description = "different"
	if _, err := e.svc.Create(ctx, e.pr, k, other, "r4"); !errors.Is(err, payments.ErrIdempotencyMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	// Keys are scoped per merchant.
	e2 := e.newMerchant("testprovider")
	res, err := e.svc.Create(ctx, e2, k, p, "r5")
	if err != nil || res.Payment == nil || res.Payment.ID == first.Payment.ID {
		t.Fatalf("other merchant: %+v %v", res, err)
	}
	equal(t, "provider submissions", e.tp.SubmitCount(), 2)
}

func TestConcurrentCreatesWithOneKeyMakeOnePayment(t *testing.T) {
	e := setup(t)
	k := key("concurrent")
	p := params(t, "254700000000", 5000)
	const n = 10
	var wg sync.WaitGroup
	created := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := e.svc.Create(ctx, e.pr, k, p, "r")
			if err == nil && res.Payment != nil {
				created <- res.Payment.ID
			} else if err != nil && !errors.Is(err, payments.ErrIdempotencyInProgress) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	close(created)
	equal(t, "creators", len(created), 1)
	var count int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM payments`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("payments = %d, %v", count, err)
	}
	equal(t, "provider submissions", e.tp.SubmitCount(), 1)
}

func TestIdempotencyRecoversFromCrashedRequest(t *testing.T) {
	e := setup(t)
	k := key("crashed")
	p := params(t, "254700000000", 5000)
	first, err := e.svc.Create(ctx, e.pr, k, p, "r1")
	if err != nil {
		t.Fatal(err)
	}
	// The request never completed its key. After the recovery window the
	// client gets the payment's current state instead of 409 until expiry.
	e.clock.Advance(e.policy.IdempotencyRecoverAfter + time.Second)
	res, err := e.svc.Create(ctx, e.pr, k, p, "r2")
	if err != nil || res.Payment == nil || res.Payment.ID != first.Payment.ID {
		t.Fatalf("recovered: %+v %v", res, err)
	}
}

func TestNoEligibleProviderCreatesNothing(t *testing.T) {
	e := setup(t)
	e.pr = e.newMerchant("not_deployed")
	_, err := e.svc.Create(ctx, e.pr, key("k"), params(t, "254700000000", 5000), "r")
	if !errors.Is(err, payments.ErrNoEligibleProvider) {
		t.Fatalf("err = %v", err)
	}
	var payments_, keys int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM payments`).Scan(&payments_)
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys`).Scan(&keys)
	if payments_ != 0 || keys != 0 {
		t.Fatalf("payments=%d keys=%d", payments_, keys)
	}
	// Fractional shillings are not routable to M-Pesa providers.
	e.pr = e.newMerchant("testprovider")
	if _, err := e.svc.Create(ctx, e.pr, key("k2"), params(t, "254700000000", 1050), "r"); !errors.Is(err, payments.ErrNoEligibleProvider) {
		t.Fatalf("fractional: %v", err)
	}
}

func TestValidation(t *testing.T) {
	e := setup(t)
	bad := []func(*payments.CreateParams){
		func(p *payments.CreateParams) { p.PhoneNumber = "0712345678" },
		func(p *payments.CreateParams) { p.Method = "card" },
		func(p *payments.CreateParams) { p.Amount, _ = money.New(0, money.MustCurrency("KES")) },
		func(p *payments.CreateParams) { p.Amount, _ = money.New(-5, money.MustCurrency("KES")) },
		func(p *payments.CreateParams) { p.Description = strings.Repeat("x", 201) },
	}
	for i, mutate := range bad {
		p := params(t, "254700000000", 100)
		mutate(&p)
		if _, err := e.svc.Create(ctx, e.pr, key(fmt.Sprint("v", i)), p, "r"); !errors.Is(err, payments.ErrInvalid) {
			t.Errorf("case %d: err = %v", i, err)
		}
	}
}

func TestMerchantIsolation(t *testing.T) {
	e := setup(t)
	v := e.create("254700000000")
	other := e.newMerchant("testprovider")
	if _, err := e.svc.Get(ctx, other, v.ID); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("other merchant: %v", err)
	}
	live := e.pr
	live.Environment = merchant.Live
	if _, err := e.svc.Get(ctx, live, v.ID); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("other environment: %v", err)
	}
	if _, err := e.svc.Cancel(ctx, other, v.ID, "r"); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("cancel other merchant: %v", err)
	}
	if _, err := e.svc.Get(ctx, e.pr, "pay_garbage"); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("malformed id: %v", err)
	}
}

// insertUnsubmitted simulates a crash between creating a payment and
// submitting it.
func (e *env) insertUnsubmitted() (store.Payment, store.PaymentAttempt) {
	e.t.Helper()
	rows, err := e.q.ListRoutingCandidates(ctx, store.ListRoutingCandidatesParams{MerchantID: e.pr.MerchantID, Environment: "sandbox", Method: "mpesa"})
	if err != nil || len(rows) == 0 {
		e.t.Fatal(err)
	}
	p, err := e.q.InsertPayment(ctx, store.InsertPaymentParams{
		PublicID: ids.New(ids.Payment), MerchantID: e.pr.MerchantID, Environment: "sandbox", AmountMinor: 10000,
		Currency: "KES", Method: "mpesa", PhoneNumber: "254700000000", SettlementDestinationID: rows[0].DestinationID,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	a, err := e.q.InsertAttempt(ctx, store.InsertAttemptParams{PublicID: ids.New(ids.Attempt), PaymentID: p.ID, Provider: "testprovider", RoutingReason: "test", CreatedAt: e.clock.Now()})
	if err != nil {
		e.t.Fatal(err)
	}
	return p, a
}

func TestCancel(t *testing.T) {
	e := setup(t)
	p, _ := e.insertUnsubmitted()
	v, err := e.svc.Cancel(ctx, e.pr, p.PublicID, "r")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, "status", v.Status, payments.StatusCancelled)
	equal(t, "attempt", v.Attempts[0].Status, payments.AttemptAbandoned)
	// Cancelling again is a no-op.
	if _, err := e.svc.Cancel(ctx, e.pr, p.PublicID, "r"); err != nil {
		t.Fatal(err)
	}
	// The worker never submits an abandoned attempt.
	e.clock.Advance(time.Hour)
	if err := e.svc.SweepSubmissions(ctx, 10); err != nil {
		t.Fatal(err)
	}
	equal(t, "provider submissions", e.tp.SubmitCount(), 0)

	sent := e.create("254700000000")
	if _, err := e.svc.Cancel(ctx, e.pr, sent.ID, "r"); !errors.Is(err, payments.ErrNotCancellable) {
		t.Fatalf("cancel after submission: %v", err)
	}
}

func TestSweepSubmitsCrashedCreations(t *testing.T) {
	e := setup(t)
	p, _ := e.insertUnsubmitted()
	if err := e.svc.SweepSubmissions(ctx, 10); err != nil {
		t.Fatal(err)
	}
	equal(t, "not yet (grace period)", e.tp.SubmitCount(), 0)
	e.clock.Advance(e.policy.UnclaimedSubmissionGrace + time.Second)
	if err := e.svc.SweepSubmissions(ctx, 10); err != nil {
		t.Fatal(err)
	}
	equal(t, "submitted", e.tp.SubmitCount(), 1)
	equal(t, "status", e.get(p.PublicID).Status, payments.StatusPendingProvider)
}

func TestSweepNeverResubmitsAnInterruptedSubmission(t *testing.T) {
	e := setup(t)
	p, a := e.insertUnsubmitted()
	// A process claimed the attempt and died before recording an outcome.
	if _, err := e.q.ClaimAttemptSubmission(ctx, store.ClaimAttemptSubmissionParams{ID: a.ID, Now: e.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(e.policy.SubmissionLease + time.Second)
	if err := e.svc.SweepSubmissions(ctx, 10); err != nil {
		t.Fatal(err)
	}
	equal(t, "provider submissions", e.tp.SubmitCount(), 0)
	_, a = e.row(p.PublicID)
	equal(t, "attempt", a.Status, "SUBMISSION_UNKNOWN")
	equal(t, "payment", e.get(p.PublicID).Status, payments.StatusPendingProvider)
}

func TestConcurrentCallbacksAndVerificationProduceOneOutcome(t *testing.T) {
	e := setup(t)
	v := e.create("254700000000")
	_, a := e.row(v.ID)
	e.clock.Advance(e.policy.InitialVerificationDelay)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if err := e.callback("testprovider", *a.ProviderReference, fmt.Sprint("n", i%2), "succeeded"); err != nil {
				t.Error(err)
			}
		}(i)
		go func() {
			defer wg.Done()
			if _, err := e.svc.VerifyDue(ctx, 10); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	e.verifyUntilQuiet()
	equal(t, "final", e.get(v.ID).Status, payments.StatusSucceeded)
	ts := e.transitions(v.ID)
	succeeded := 0
	for _, tr := range ts {
		if strings.Contains(tr, ">SUCCEEDED") {
			succeeded++
		}
	}
	equal(t, "success transitions", succeeded, 1)
	equal(t, "one event per transition", len(e.events(v.ID)), len(ts))
}
