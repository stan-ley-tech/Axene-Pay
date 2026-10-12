//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stan-ley-tech/axene-pay/internal/platform/ids"
	"github.com/stan-ley-tech/axene-pay/internal/platform/testdb"
	"github.com/stan-ley-tech/axene-pay/internal/store"
)

var ctx = context.Background()

func ptr[T any](v T) *T { return &v }

type world struct {
	pool     *pgxpool.Pool
	q        *store.Queries
	merchant store.Merchant
	payment  store.Payment
	attempt  store.PaymentAttempt
}

func newWorld(t *testing.T) world {
	t.Helper()
	pool := testdb.New(t)
	q := store.New(pool)
	m, err := q.CreateMerchant(ctx, store.CreateMerchantParams{PublicID: ids.New(ids.Merchant), Name: "M", Status: "approved"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := q.CreateSettlementDestination(ctx, store.CreateSettlementDestinationParams{
		PublicID: ids.New(ids.SettlementDestination), MerchantID: m.ID, Environment: "sandbox",
		Type: "mpesa_till", Identifier: "123456",
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := q.InsertPayment(ctx, store.InsertPaymentParams{
		PublicID: ids.New(ids.Payment), MerchantID: m.ID, Environment: "sandbox", AmountMinor: 1000,
		Currency: "KES", Method: "mpesa", PhoneNumber: "254700000000", SettlementDestinationID: d.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := q.InsertAttempt(ctx, store.InsertAttemptParams{PublicID: ids.New(ids.Attempt), PaymentID: p.ID, Provider: "testprovider", RoutingReason: "test", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return world{pool: pool, q: q, merchant: m, payment: p, attempt: a}
}

func TestCallbackDeduplication(t *testing.T) {
	w := newWorld(t)
	orig := store.InsertOriginalCallbackParams{
		PublicID: ids.New(ids.Callback), Provider: "testprovider", ParseStatus: "parsed",
		DedupKey: ptr("n-1"), ProviderReference: ptr("ref"), ClaimedOutcome: ptr("succeeded"),
		AttemptID: w.attempt.ID, RawBody: []byte(`{}`), RequestID: "req-1",
	}
	first, err := w.q.InsertOriginalCallback(ctx, orig)
	if err != nil {
		t.Fatal(err)
	}
	orig.PublicID = ids.New(ids.Callback)
	if _, err := w.q.InsertOriginalCallback(ctx, orig); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second original: err = %v, want ErrNoRows", err)
	}
	dup, err := w.q.InsertDuplicateCallback(ctx, store.InsertDuplicateCallbackParams{
		PublicID: ids.New(ids.Callback), Provider: "testprovider", DedupKey: "n-1",
		ProviderReference: ptr("ref"), ClaimedOutcome: ptr("succeeded"), AttemptID: w.attempt.ID,
		RawBody: []byte(`{}`), RequestID: "req-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	all, err := w.q.ListCallbacksForAttempt(ctx, w.attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[1].ID != dup.ID || all[1].DuplicateOf != first.ID {
		t.Fatalf("callbacks = %+v", all)
	}
}

func TestIdempotencyKeyHasExactlyOneOwner(t *testing.T) {
	w := newWorld(t)
	params := store.InsertIdempotencyKeyParams{
		MerchantID: w.merchant.ID, Environment: "sandbox", Key: "k1", RequestMethod: "POST",
		RequestPath: "/v1/payments", RequestFingerprint: make([]byte, 32), CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	const n = 8
	results := make(chan int64, n)
	for i := 0; i < n; i++ {
		go func() {
			rows, err := w.q.InsertIdempotencyKey(ctx, params)
			if err != nil {
				t.Error(err)
			}
			results <- rows
		}()
	}
	owners := int64(0)
	for i := 0; i < n; i++ {
		owners += <-results
	}
	if owners != 1 {
		t.Fatalf("%d owners, want 1", owners)
	}
}

func TestClaimDueVerificationsLeasesAndSkipsLocked(t *testing.T) {
	w := newWorld(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := w.pool.Exec(ctx, `UPDATE payment_attempts SET status = 'SUBMITTED', next_verification_at = $2 WHERE id = $1`,
		w.attempt.ID, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	lease := now.Add(time.Minute)
	got, err := w.q.ClaimDueVerifications(ctx, store.ClaimDueVerificationsParams{LeaseUntil: lease, Now: now, MaxRows: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].NextVerificationAt == nil || !got[0].NextVerificationAt.Equal(lease) {
		t.Fatalf("claimed %+v", got)
	}
	// Leased: not due again until the lease expires.
	again, err := w.q.ClaimDueVerifications(ctx, store.ClaimDueVerificationsParams{LeaseUntil: lease, Now: now, MaxRows: 10})
	if err != nil || len(again) != 0 {
		t.Fatalf("reclaimed during lease: %v %v", again, err)
	}
	later := lease.Add(time.Second)
	expired, err := w.q.ClaimDueVerifications(ctx, store.ClaimDueVerificationsParams{LeaseUntil: later, Now: later, MaxRows: 10})
	if err != nil || len(expired) != 1 {
		t.Fatalf("not reclaimed after lease: %v %v", expired, err)
	}

	// A row locked by another transaction is skipped, not waited for.
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM payment_attempts WHERE id = $1 FOR UPDATE`, w.attempt.ID); err != nil {
		t.Fatal(err)
	}
	far := later.Add(time.Hour)
	skipped, err := w.q.ClaimDueVerifications(ctx, store.ClaimDueVerificationsParams{LeaseUntil: far, Now: far, MaxRows: 10})
	if err != nil || len(skipped) != 0 {
		t.Fatalf("locked row claimed: %v %v", skipped, err)
	}
}

func TestClaimAttemptSubmissionOnlyOnce(t *testing.T) {
	w := newWorld(t)
	now := time.Now()
	first, err := w.q.ClaimAttemptSubmission(ctx, store.ClaimAttemptSubmissionParams{ID: w.attempt.ID, Now: now})
	if err != nil || first != 1 {
		t.Fatalf("first claim = %d, %v", first, err)
	}
	second, err := w.q.ClaimAttemptSubmission(ctx, store.ClaimAttemptSubmissionParams{ID: w.attempt.ID, Now: now})
	if err != nil || second != 0 {
		t.Fatalf("second claim = %d, %v", second, err)
	}
}

func TestReconExceptionInsertedOncePerAttempt(t *testing.T) {
	w := newWorld(t)
	for i := 0; i < 3; i++ {
		_, err := w.q.InsertReconException(ctx, store.InsertReconExceptionParams{
			PublicID: ids.New(ids.ReconException), Type: "verification_exhausted",
			PaymentID: w.payment.ID, AttemptID: w.attempt.ID, CallbackID: pgtype.UUID{}, Detail: []byte(`{}`),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	ex, err := w.q.ListReconExceptionsForPayment(ctx, w.payment.ID)
	if err != nil || len(ex) != 1 {
		t.Fatalf("exceptions = %d, %v", len(ex), err)
	}
}
