//go:build integration

package merchant_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stan-ley-tech/axene-pay/internal/merchant"
	"github.com/stan-ley-tech/axene-pay/internal/platform/testdb"
)

var ctx = context.Background()

func newService(t *testing.T) *merchant.Service {
	t.Helper()
	svc, err := merchant.NewService(testdb.New(t), []byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestAPIKeyLifecycle(t *testing.T) {
	svc := newService(t)
	m, err := svc.CreateMerchant(ctx, "Duka Ltd")
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "pending_review" {
		t.Fatalf("new merchant status %q", m.Status)
	}

	// Pending merchants get sandbox keys but not live keys.
	key, err := svc.IssueAPIKey(ctx, m.PublicID, merchant.Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.IssueAPIKey(ctx, m.PublicID, merchant.Live); !errors.Is(err, merchant.ErrNotApproved) {
		t.Fatalf("live key for pending merchant: %v", err)
	}

	p, err := svc.Authenticate(ctx, key.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if p.MerchantPublicID != m.PublicID || p.Environment != merchant.Sandbox || p.KeyID != key.ID {
		t.Fatalf("principal %+v", p)
	}

	// Approve, issue a live key, then suspend: every key stops working.
	if _, err := svc.SetStatus(ctx, m.PublicID, merchant.StatusApproved); err != nil {
		t.Fatal(err)
	}
	live, err := svc.IssueAPIKey(ctx, m.PublicID, merchant.Live)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := svc.Authenticate(ctx, live.Secret); err != nil || p.Environment != merchant.Live {
		t.Fatalf("live auth: %+v %v", p, err)
	}
	if _, err := svc.SetStatus(ctx, m.PublicID, merchant.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{key.Secret, live.Secret} {
		if _, err := svc.Authenticate(ctx, k); !errors.Is(err, merchant.ErrSuspended) {
			t.Fatalf("suspended auth: %v", err)
		}
	}

	// Revocation is immediate.
	if _, err := svc.SetStatus(ctx, m.PublicID, merchant.StatusApproved); err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokeAPIKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, key.Secret); !errors.Is(err, merchant.ErrUnauthenticated) {
		t.Fatalf("revoked key: %v", err)
	}
	if err := svc.RevokeAPIKey(ctx, key.ID); !errors.Is(err, merchant.ErrNotFound) {
		t.Fatalf("double revoke: %v", err)
	}
}

func TestAuthenticateRejectsUnknownAndMalformedKeys(t *testing.T) {
	svc := newService(t)
	for _, s := range []string{"", "sk_test_short", "sk_test_" + strings.Repeat("A", 43)} {
		if _, err := svc.Authenticate(ctx, s); !errors.Is(err, merchant.ErrUnauthenticated) {
			t.Errorf("%q: %v", s, err)
		}
	}
}

func TestKeysDoNotWorkAcrossPeppers(t *testing.T) {
	pool := testdb.New(t)
	a, _ := merchant.NewService(pool, []byte(strings.Repeat("a", 32)))
	b, _ := merchant.NewService(pool, []byte(strings.Repeat("b", 32)))
	m, _ := a.CreateMerchant(ctx, "M")
	key, err := a.IssueAPIKey(ctx, m.PublicID, merchant.Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Authenticate(ctx, key.Secret); !errors.Is(err, merchant.ErrUnauthenticated) {
		t.Fatalf("key accepted under another pepper: %v", err)
	}
}

func TestDestinationsAndProviderConfigs(t *testing.T) {
	svc := newService(t)
	m, _ := svc.CreateMerchant(ctx, "M")
	other, _ := svc.CreateMerchant(ctx, "Other")

	d, err := svc.CreateDestination(ctx, m.PublicID, merchant.Sandbox, "mpesa_paybill", "600123", "Main paybill")
	if err != nil {
		t.Fatal(err)
	}
	if d.Owner != "merchant" {
		t.Fatalf("owner %q", d.Owner)
	}
	if _, err := svc.CreateDestination(ctx, m.PublicID, merchant.Sandbox, "mpesa_paybill", "600123", ""); !errors.Is(err, merchant.ErrConflict) {
		t.Fatalf("duplicate destination: %v", err)
	}
	for _, bad := range []struct{ typ, ident string }{{"bank", "1"}, {"mpesa_till", "12a"}, {"mpesa_till", ""}} {
		if _, err := svc.CreateDestination(ctx, m.PublicID, merchant.Sandbox, bad.typ, bad.ident, ""); !errors.Is(err, merchant.ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}

	if _, err := svc.CreateProviderConfig(ctx, m.PublicID, merchant.Sandbox, "mpesa", "testprovider", d.PublicID, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateProviderConfig(ctx, m.PublicID, merchant.Sandbox, "mpesa", "testprovider", d.PublicID, 10); !errors.Is(err, merchant.ErrConflict) {
		t.Fatalf("duplicate config: %v", err)
	}
	// Another merchant cannot route into this merchant's destination.
	if _, err := svc.CreateProviderConfig(ctx, other.PublicID, merchant.Sandbox, "mpesa", "testprovider", d.PublicID, 10); !errors.Is(err, merchant.ErrNotFound) {
		t.Fatalf("cross-merchant destination: %v", err)
	}
	// Environments do not mix.
	if _, err := svc.CreateProviderConfig(ctx, m.PublicID, merchant.Live, "mpesa", "other", d.PublicID, 10); !errors.Is(err, merchant.ErrInvalid) {
		t.Fatalf("cross-environment destination: %v", err)
	}
	if _, err := svc.CreateProviderConfig(ctx, m.PublicID, merchant.Sandbox, "mpesa", "Bad Name", d.PublicID, 10); !errors.Is(err, merchant.ErrInvalid) {
		t.Fatalf("bad provider name: %v", err)
	}
	if _, err := svc.CreateProviderConfig(ctx, m.PublicID, merchant.Sandbox, "card", "x", d.PublicID, 10); !errors.Is(err, merchant.ErrInvalid) {
		t.Fatalf("unsupported method: %v", err)
	}
}

func TestUnknownMerchant(t *testing.T) {
	svc := newService(t)
	if _, err := svc.IssueAPIKey(ctx, "mer_00000000000000000000000000", merchant.Sandbox); !errors.Is(err, merchant.ErrNotFound) {
		t.Fatalf("unknown merchant: %v", err)
	}
	if _, err := svc.IssueAPIKey(ctx, "not-an-id", merchant.Sandbox); !errors.Is(err, merchant.ErrInvalid) {
		t.Fatalf("malformed id: %v", err)
	}
	if _, err := svc.SetStatus(ctx, "mer_00000000000000000000000000", merchant.StatusApproved); !errors.Is(err, merchant.ErrNotFound) {
		t.Fatalf("set status unknown: %v", err)
	}
	if _, err := svc.CreateMerchant(ctx, "  "); !errors.Is(err, merchant.ErrInvalid) {
		t.Fatalf("blank name: %v", err)
	}
}
