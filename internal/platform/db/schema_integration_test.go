//go:build integration

package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stan-ley-tech/axene-pay/internal/platform/db"
	"github.com/stan-ley-tech/axene-pay/internal/platform/ids"
	"github.com/stan-ley-tech/axene-pay/internal/platform/testdb"
)

var ctx = context.Background()

func TestMigrationsUpDownUp(t *testing.T) {
	u := testdb.NewURL(t)
	applied, err := db.Migrate(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) == 0 {
		t.Fatal("no migrations applied")
	}
	again, err := db.Migrate(ctx, u)
	if err != nil || len(again) != 0 {
		t.Fatalf("second Up applied %v, err %v", again, err)
	}
	if err := db.MigrateDownAll(ctx, u); err != nil {
		t.Fatal(err)
	}
	st, err := db.Status(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range st {
		if s.Applied {
			t.Fatalf("%s still applied after DownTo(0)", s.Path)
		}
	}
	if _, err := db.Migrate(ctx, u); err != nil {
		t.Fatalf("Up after Down: %v", err)
	}
}

func TestMigrateRejectsBadURL(t *testing.T) {
	if _, err := db.Migrate(ctx, "::not a url::"); err == nil {
		t.Fatal("expected error")
	}
}

// fixture inserts a merchant, a destination, and a payment with one attempt.
type fixture struct {
	merchant, destination, payment, attempt string // uuids as text
}

func seed(t *testing.T, pool *pgxpool.Pool) fixture {
	t.Helper()
	var f fixture
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(pool.QueryRow(ctx, `INSERT INTO merchants (public_id, name) VALUES ($1, 'M') RETURNING id::text`,
		ids.New(ids.Merchant)).Scan(&f.merchant))
	must(pool.QueryRow(ctx, `INSERT INTO settlement_destinations (public_id, merchant_id, environment, type, identifier)
		VALUES ($1, $2, 'sandbox', 'mpesa_paybill', '600000') RETURNING id::text`,
		ids.New(ids.SettlementDestination), f.merchant).Scan(&f.destination))
	must(pool.QueryRow(ctx, `INSERT INTO payments (public_id, merchant_id, environment, amount_minor, currency, method, phone_number, settlement_destination_id)
		VALUES ($1, $2, 'sandbox', 1000, 'KES', 'mpesa', '254700000000', $3) RETURNING id::text`,
		ids.New(ids.Payment), f.merchant, f.destination).Scan(&f.payment))
	must(pool.QueryRow(ctx, `INSERT INTO payment_attempts (public_id, payment_id, provider, routing_reason)
		VALUES ($1, $2, 'testprovider', 'test') RETURNING id::text`,
		ids.New(ids.Attempt), f.payment).Scan(&f.attempt))
	return f
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestAppendOnlyTablesRejectMutation(t *testing.T) {
	pool := testdb.New(t)
	f := seed(t, pool)
	var transitionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO payment_transitions (payment_id, from_status, to_status, evidence, actor, request_id)
		VALUES ($1, 'CREATED', 'PENDING_PROVIDER', 'submission', 'test', 'req') RETURNING id`, f.payment).Scan(&transitionID); err != nil {
		t.Fatal(err)
	}
	var callbackID string
	if err := pool.QueryRow(ctx, `INSERT INTO provider_callbacks (public_id, provider, parse_status, raw_body, request_id)
		VALUES ($1, 'testprovider', 'rejected', '\x00', 'req') RETURNING id::text`, ids.New(ids.Callback)).Scan(&callbackID); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []struct {
		sql string
		arg any
	}{
		{`UPDATE payment_transitions SET reason = 'x' WHERE id = $1`, transitionID},
		{`DELETE FROM payment_transitions WHERE id = $1`, transitionID},
		{`UPDATE provider_callbacks SET parse_error = 'x' WHERE id = $1`, callbackID},
		{`DELETE FROM provider_callbacks WHERE id = $1`, callbackID},
	} {
		_, err := pool.Exec(ctx, stmt.sql, stmt.arg)
		if pgCode(err) != "42501" {
			t.Errorf("%q: err = %v, want insufficient_privilege", stmt.sql, err)
		}
	}
}

func TestOnlyOneOpenAttemptPerPayment(t *testing.T) {
	pool := testdb.New(t)
	f := seed(t, pool)
	_, err := pool.Exec(ctx, `INSERT INTO payment_attempts (public_id, payment_id, provider, routing_reason)
		VALUES ($1, $2, 'other', 'test')`, ids.New(ids.Attempt), f.payment)
	if pgCode(err) != "23505" {
		t.Fatalf("second open attempt: err = %v, want unique_violation", err)
	}
	// Once the first attempt is resolved, a new one is allowed.
	if _, err := pool.Exec(ctx, `UPDATE payment_attempts SET status = 'CONFIRMED_FAILURE' WHERE id = $1`, f.attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO payment_attempts (public_id, payment_id, provider, routing_reason)
		VALUES ($1, $2, 'other', 'test')`, ids.New(ids.Attempt), f.payment); err != nil {
		t.Fatalf("attempt after resolution: %v", err)
	}
}

func TestPaymentCannotUseAnotherMerchantsDestination(t *testing.T) {
	pool := testdb.New(t)
	f := seed(t, pool)
	var other string
	if err := pool.QueryRow(ctx, `INSERT INTO merchants (public_id, name) VALUES ($1, 'Other') RETURNING id::text`,
		ids.New(ids.Merchant)).Scan(&other); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO payments (public_id, merchant_id, environment, amount_minor, currency, method, phone_number, settlement_destination_id)
		VALUES ($1, $2, 'sandbox', 1000, 'KES', 'mpesa', '254700000000', $3)`, ids.New(ids.Payment), other, f.destination)
	if pgCode(err) != "23503" {
		t.Fatalf("cross-merchant destination: err = %v, want foreign_key_violation", err)
	}
	// Same merchant but wrong environment is also refused.
	_, err = pool.Exec(ctx, `INSERT INTO payments (public_id, merchant_id, environment, amount_minor, currency, method, phone_number, settlement_destination_id)
		VALUES ($1, $2, 'live', 1000, 'KES', 'mpesa', '254700000000', $3)`, ids.New(ids.Payment), f.merchant, f.destination)
	if pgCode(err) != "23503" {
		t.Fatalf("cross-environment destination: err = %v, want foreign_key_violation", err)
	}
}

func TestConstraintsRejectBadValues(t *testing.T) {
	pool := testdb.New(t)
	f := seed(t, pool)
	cases := []string{
		`UPDATE payments SET amount_minor = 0 WHERE id = '` + f.payment + `'`,
		`UPDATE payments SET currency = 'kes' WHERE id = '` + f.payment + `'`,
		`UPDATE payments SET status = 'SUCCESS' WHERE id = '` + f.payment + `'`,
		`UPDATE payments SET phone_number = '0712345678' WHERE id = '` + f.payment + `'`,
		`UPDATE payment_attempts SET status = 'DONE' WHERE id = '` + f.attempt + `'`,
		`UPDATE settlement_destinations SET owner = 'axene' WHERE id = '` + f.destination + `'`,
		`UPDATE merchants SET public_id = 'mer_bad' WHERE id = '` + f.merchant + `'`,
	}
	for _, c := range cases {
		_, err := pool.Exec(ctx, c)
		if pgCode(err) != "23514" {
			t.Errorf("%s: err = %v, want check_violation", c, err)
		}
	}
}
