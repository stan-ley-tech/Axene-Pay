//go:build integration

package webhooks_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/stan-ley-tech/axene-pay/internal/webhooks"
)

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

// receiver is a merchant webhook endpoint that verifies signatures.
type receiver struct {
	srv      *httptest.Server
	mu       sync.Mutex
	secret   string
	status   atomic.Int32
	received []map[string]any
	headers  []http.Header
	badSig   int
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{}
	r.status.Store(http.StatusOK)
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		defer r.mu.Unlock()
		if err := webhooks.Verify(r.secret, req.Header.Get(webhooks.SignatureHeader), body, time.Now(), 24*365*time.Hour); err != nil {
			r.badSig++
		}
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		r.received = append(r.received, m)
		r.headers = append(r.headers, req.Header.Clone())
		w.WriteHeader(int(r.status.Load()))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.received) }

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	q     *store.Queries
	pay   *payments.Service
	wh    *webhooks.Service
	ms    *merchant.Service
	clock *clock
	pr    merchant.Principal
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testdb.New(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := &clock{t: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	reg := providers.NewRegistry(false)
	_ = reg.Register(testprovider.New("s"))
	pay, err := payments.NewService(payments.Deps{Pool: pool, Providers: reg, Policy: payments.DefaultPolicy(), Clock: c.Now, Logger: logger,
		CallbackURL: func(string) string { return "https://example.test/cb" }})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := webhooks.NewSealer(bytes.Repeat([]byte{1}, 32), 1)
	policy := webhooks.DefaultPolicy()
	policy.MaxAttempts = 3
	wh, err := webhooks.NewService(webhooks.Deps{Pool: pool, Sealer: s, Payments: pay, Policy: policy, AllowPrivateTargets: true, Clock: c.Now, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := merchant.NewService(pool, []byte(strings.Repeat("p", 32)))
	e := &env{t: t, pool: pool, q: store.New(pool), pay: pay, wh: wh, ms: ms, clock: c}
	e.pr = e.merchant()
	return e
}

func (e *env) merchant() merchant.Principal {
	e.t.Helper()
	m, _ := e.ms.CreateMerchant(ctx, "M")
	d, _ := e.ms.CreateDestination(ctx, m.PublicID, merchant.Sandbox, "mpesa_till", "1234", "")
	if _, err := e.ms.CreateProviderConfig(ctx, m.PublicID, merchant.Sandbox, "mpesa", "testprovider", d.PublicID, 1); err != nil {
		e.t.Fatal(err)
	}
	k, _ := e.ms.IssueAPIKey(ctx, m.PublicID, merchant.Sandbox)
	pr, err := e.ms.Authenticate(ctx, k.Secret)
	if err != nil {
		e.t.Fatal(err)
	}
	return pr
}

func (e *env) endpoint(r *receiver, types []string) webhooks.CreatedEndpoint {
	e.t.Helper()
	ep, err := e.wh.CreateEndpoint(ctx, e.pr, r.srv.URL+"/hooks", types, "test")
	if err != nil {
		e.t.Fatal(err)
	}
	r.secret = ep.Secret
	return ep
}

// payment creates a payment and returns its outbox event IDs.
func (e *env) payment(pr merchant.Principal) (string, []string) {
	e.t.Helper()
	a, _ := money.New(10000, money.MustCurrency("KES"))
	res, err := e.pay.Create(ctx, pr, idempotency.Key{Value: ids.New(ids.Event), Method: "POST", Path: "/v1/payments"},
		payments.CreateParams{Amount: a, Method: providers.MethodMpesa, PhoneNumber: "254700000000"}, "req")
	if err != nil {
		e.t.Fatal(err)
	}
	evs, err := e.q.ListOutboxEventsForAggregate(ctx, res.Payment.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		out = append(out, ev.PublicID)
	}
	return res.Payment.ID, out
}

func TestFanOutAndSignedDelivery(t *testing.T) {
	e := setup(t)
	r := newReceiver(t)
	all := e.endpoint(r, nil)
	onlySucceeded, err := e.wh.CreateEndpoint(ctx, e.pr, r.srv.URL+"/other", []string{"payment.succeeded"}, "")
	if err != nil {
		t.Fatal(err)
	}
	disabled, _ := e.wh.CreateEndpoint(ctx, e.pr, r.srv.URL+"/off", nil, "")
	if _, err := e.wh.DisableEndpoint(ctx, e.pr, disabled.Endpoint.PublicID); err != nil {
		t.Fatal(err)
	}
	otherMerchant := e.merchant()
	if _, err := e.wh.CreateEndpoint(ctx, otherMerchant, r.srv.URL+"/foreign", nil, ""); err != nil {
		t.Fatal(err)
	}

	payID, events := e.payment(e.pr)
	if len(events) != 1 {
		t.Fatalf("events = %v", events)
	}
	n, err := e.wh.FanOut(ctx, events[0])
	if err != nil || n != 1 {
		t.Fatalf("fan-out created %d, %v (want 1: only the all-events endpoint)", n, err)
	}
	// Re-running fan-out (broker redelivery or sweep) creates nothing new.
	if n, err := e.wh.FanOut(ctx, events[0]); err != nil || n != 0 {
		t.Fatalf("second fan-out created %d, %v", n, err)
	}

	if got, err := e.wh.DeliverDue(ctx, 10); err != nil || got != 1 {
		t.Fatalf("delivered %d, %v", got, err)
	}
	if r.count() != 1 || r.badSig != 0 {
		t.Fatalf("received %d, bad signatures %d", r.count(), r.badSig)
	}
	msg := r.received[0]
	data := msg["data"].(map[string]any)["payment"].(map[string]any)
	if msg["id"] != events[0] || msg["type"] != "payment.pending_provider" || msg["api_version"] != "v1" || data["id"] != payID {
		t.Fatalf("payload %v", msg)
	}
	if strings.Contains(strings.Join(r.headers[0].Values("Axene-Event-Id"), ""), events[0]) == false ||
		r.headers[0].Get("Axene-Delivery-Id") == "" || r.headers[0].Get("Axene-Event-Type") != "payment.pending_provider" {
		t.Fatalf("headers %v", r.headers[0])
	}
	if phone := data["payment_method"].(map[string]any)["phone_number"]; phone != "25470****000" {
		t.Fatalf("phone not masked: %v", phone)
	}

	ds, err := e.wh.ListDeliveries(ctx, e.pr, all.Endpoint.PublicID, 10)
	if err != nil || len(ds) != 1 || ds[0].Status != "delivered" {
		t.Fatalf("deliveries %+v %v", ds, err)
	}
	detail, err := e.wh.GetDelivery(ctx, e.pr, ds[0].PublicID)
	if err != nil || len(detail.Attempts) != 1 || *detail.Attempts[0].StatusCode != 200 {
		t.Fatalf("detail %+v %v", detail, err)
	}
	if ds, _ := e.wh.ListDeliveries(ctx, e.pr, onlySucceeded.Endpoint.PublicID, 10); len(ds) != 0 {
		t.Fatalf("unsubscribed endpoint got %d deliveries", len(ds))
	}
	// Merchant isolation.
	if _, err := e.wh.GetDelivery(ctx, otherMerchant, ds0(t, e, all)); !errors.Is(err, webhooks.ErrNotFound) {
		t.Fatalf("other merchant saw delivery: %v", err)
	}
	if _, err := e.wh.ListDeliveries(ctx, otherMerchant, all.Endpoint.PublicID, 10); !errors.Is(err, webhooks.ErrNotFound) {
		t.Fatalf("other merchant listed deliveries: %v", err)
	}
}

func ds0(t *testing.T, e *env, ep webhooks.CreatedEndpoint) string {
	t.Helper()
	ds, err := e.wh.ListDeliveries(ctx, e.pr, ep.Endpoint.PublicID, 1)
	if err != nil || len(ds) == 0 {
		t.Fatal(err)
	}
	return ds[0].PublicID
}

func TestRetryBackoffDeadAndReplay(t *testing.T) {
	e := setup(t)
	r := newReceiver(t)
	ep := e.endpoint(r, nil)
	r.status.Store(http.StatusInternalServerError)
	_, events := e.payment(e.pr)
	if _, err := e.wh.FanOut(ctx, events[0]); err != nil {
		t.Fatal(err)
	}

	// Attempt 1 fails; nothing is due until the backoff elapses.
	if n, _ := e.wh.DeliverDue(ctx, 10); n != 1 {
		t.Fatalf("first attempt %d", n)
	}
	if n, _ := e.wh.DeliverDue(ctx, 10); n != 0 {
		t.Fatalf("retried before backoff: %d", n)
	}
	e.clock.Advance(31 * time.Second)
	if n, _ := e.wh.DeliverDue(ctx, 10); n != 1 {
		t.Fatalf("second attempt %d", n)
	}
	e.clock.Advance(61 * time.Second)
	if n, _ := e.wh.DeliverDue(ctx, 10); n != 1 {
		t.Fatalf("third attempt %d", n)
	}
	id := ds0(t, e, ep)
	d, _ := e.wh.GetDelivery(ctx, e.pr, id)
	if d.Delivery.Status != "dead" || d.Delivery.Attempts != 3 || len(d.Attempts) != 3 {
		t.Fatalf("after max attempts: %+v (%d attempts recorded)", d.Delivery, len(d.Attempts))
	}
	e.clock.Advance(24 * time.Hour)
	if n, _ := e.wh.DeliverDue(ctx, 10); n != 0 {
		t.Fatalf("dead delivery retried: %d", n)
	}

	// The merchant fixes their endpoint and replays.
	r.status.Store(http.StatusNoContent)
	if _, err := e.wh.Replay(ctx, e.pr, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.wh.Replay(ctx, e.pr, id); !errors.Is(err, webhooks.ErrNotReplayable) {
		t.Fatalf("replay of pending delivery: %v", err)
	}
	if n, _ := e.wh.DeliverDue(ctx, 10); n != 1 {
		t.Fatalf("replayed attempt %d", n)
	}
	d, _ = e.wh.GetDelivery(ctx, e.pr, id)
	if d.Delivery.Status != "delivered" || len(d.Attempts) != 4 || r.count() != 4 || r.badSig != 0 {
		t.Fatalf("after replay: %+v attempts=%d received=%d badSig=%d", d.Delivery.Status, len(d.Attempts), r.count(), r.badSig)
	}
	// Attempt history is append-only.
	if _, err := e.pool.Exec(ctx, `DELETE FROM webhook_delivery_attempts`); err == nil {
		t.Fatal("attempt history deletable")
	}
}

func TestDisabledEndpointStopsDelivery(t *testing.T) {
	e := setup(t)
	r := newReceiver(t)
	ep := e.endpoint(r, nil)
	_, events := e.payment(e.pr)
	if _, err := e.wh.FanOut(ctx, events[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := e.wh.DisableEndpoint(ctx, e.pr, ep.Endpoint.PublicID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.wh.DeliverDue(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if r.count() != 0 {
		t.Fatal("delivered to a disabled endpoint")
	}
	d, _ := e.wh.GetDelivery(ctx, e.pr, ds0(t, e, ep))
	if d.Delivery.Status != "dead" || d.Delivery.LastError == nil || *d.Delivery.LastError != "endpoint disabled" {
		t.Fatalf("delivery %+v", d.Delivery)
	}
}

func TestSweepFansOutEventsTheBrokerMissed(t *testing.T) {
	e := setup(t)
	r := newReceiver(t)
	e.endpoint(r, nil)
	_, events := e.payment(e.pr)
	if _, err := e.pool.Exec(ctx, `UPDATE outbox_events SET published_at = $1`, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.wh.SweepUnfanned(ctx, 10); n != 0 {
		t.Fatalf("swept before delay: %d", n)
	}
	e.clock.Advance(webhooks.DefaultPolicy().SweepDelay + time.Second)
	if n, err := e.wh.SweepUnfanned(ctx, 10); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	if n, _ := e.wh.SweepUnfanned(ctx, 10); n != 0 {
		t.Fatalf("swept twice: %d", n)
	}
	if _, err := e.wh.FanOut(ctx, events[0]); err != nil {
		t.Fatal(err)
	}
	var deliveries int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries`).Scan(&deliveries)
	if deliveries != 1 {
		t.Fatalf("deliveries = %d, want 1", deliveries)
	}
}

func TestConcurrentFanOutIsIdempotent(t *testing.T) {
	e := setup(t)
	r := newReceiver(t)
	e.endpoint(r, nil)
	_, events := e.payment(e.pr)
	var wg sync.WaitGroup
	var created atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := e.wh.FanOut(ctx, events[0])
			if err != nil {
				t.Error(err)
			}
			created.Add(int32(n))
		}()
	}
	wg.Wait()
	if created.Load() != 1 {
		t.Fatalf("created %d deliveries", created.Load())
	}
}

func TestEndpointManagement(t *testing.T) {
	e := setup(t)
	ep, err := e.wh.CreateEndpoint(ctx, e.pr, "https://merchant.example/hooks", []string{"payment.succeeded", "payment.failed"}, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ep.Secret, "whsec_") || bytes.Contains(ep.Endpoint.SecretCiphertext, []byte(ep.Secret)) {
		t.Fatal("secret not generated or stored in plaintext")
	}
	list, err := e.wh.ListEndpoints(ctx, e.pr)
	if err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	if _, err := e.wh.CreateEndpoint(ctx, e.pr, "https://merchant.example/hooks", []string{"payment.exploded"}, ""); !errors.Is(err, webhooks.ErrInvalid) {
		t.Fatalf("unknown event type: %v", err)
	}
	other := e.merchant()
	if _, err := e.wh.GetEndpoint(ctx, other, ep.Endpoint.PublicID); !errors.Is(err, webhooks.ErrNotFound) {
		t.Fatalf("other merchant: %v", err)
	}
	if _, err := e.wh.DisableEndpoint(ctx, other, ep.Endpoint.PublicID); !errors.Is(err, webhooks.ErrNotFound) {
		t.Fatalf("other merchant disabled endpoint: %v", err)
	}
	got, err := e.wh.DisableEndpoint(ctx, e.pr, ep.Endpoint.PublicID)
	if err != nil || got.Status != "disabled" {
		t.Fatalf("disable: %+v %v", got, err)
	}
}
