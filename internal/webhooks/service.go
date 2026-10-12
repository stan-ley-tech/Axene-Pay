package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stan-ley-tech/axene-pay/internal/apiview"
	"github.com/stan-ley-tech/axene-pay/internal/merchant"
	"github.com/stan-ley-tech/axene-pay/internal/payments"
	"github.com/stan-ley-tech/axene-pay/internal/platform/ids"
	"github.com/stan-ley-tech/axene-pay/internal/store"
)

var (
	// ErrInvalid wraps endpoint validation failures.
	ErrInvalid = errors.New("webhooks: invalid request")
	// ErrNotFound means the endpoint or delivery is not visible to the merchant.
	ErrNotFound = errors.New("webhooks: not found")
	// ErrNotReplayable means the delivery is still pending.
	ErrNotReplayable = errors.New("webhooks: delivery is still pending")
)

// EventTypes are the event types an endpoint may subscribe to.
var EventTypes = []string{
	"payment.pending_provider", "payment.pending_verification", "payment.unresolved",
	"payment.succeeded", "payment.failed", "payment.cancelled", "payment.reversed",
}

// Policy holds delivery timing. Operational values, documented in
// docs/development.md.
type Policy struct {
	Timeout     time.Duration // per HTTP attempt
	RetryBase   time.Duration
	RetryMax    time.Duration
	MaxAttempts int
	Lease       time.Duration // must exceed Timeout
	SweepDelay  time.Duration // published events older than this without a fan-out are swept
}

// DefaultPolicy: 16 attempts with backoff from 30s doubling to 1h, about
// 9 hours from the first attempt until a delivery is dead.
func DefaultPolicy() Policy {
	return Policy{Timeout: 10 * time.Second, RetryBase: 30 * time.Second, RetryMax: time.Hour,
		MaxAttempts: 16, Lease: time.Minute, SweepDelay: 2 * time.Minute}
}

func (p Policy) backoff(attempt int) time.Duration {
	d := p.RetryBase
	for i := 0; i < attempt && d < p.RetryMax; i++ {
		d *= 2
	}
	return min(d, p.RetryMax)
}

// Service manages endpoints, fans events out, and delivers them.
type Service struct {
	pool         *pgxpool.Pool
	q            *store.Queries
	sealer       *Sealer
	payments     *payments.Service
	client       *http.Client
	policy       Policy
	requireHTTPS bool
	allowPrivate bool
	now          func() time.Time
	log          *slog.Logger
}

// Deps configure a Service.
type Deps struct {
	Pool     *pgxpool.Pool
	Sealer   *Sealer
	Payments *payments.Service
	Policy   Policy
	// RequireHTTPS rejects http:// endpoints (deployed environments).
	RequireHTTPS bool
	// AllowPrivateTargets permits loopback/private destinations
	// (development and tests only).
	AllowPrivateTargets bool
	Clock               func() time.Time
	Logger              *slog.Logger
}

// NewService returns a Service.
func NewService(d Deps) (*Service, error) {
	if d.Pool == nil || d.Sealer == nil || d.Payments == nil || d.Logger == nil {
		return nil, errors.New("webhooks: missing dependency")
	}
	if d.Policy.Lease <= d.Policy.Timeout || d.Policy.MaxAttempts < 1 || d.Policy.RetryBase <= 0 {
		return nil, errors.New("webhooks: invalid policy")
	}
	if d.Clock == nil {
		d.Clock = time.Now
	}
	return &Service{
		pool: d.Pool, q: store.New(d.Pool), sealer: d.Sealer, payments: d.Payments,
		client: newClient(d.Policy.Timeout, d.AllowPrivateTargets), policy: d.Policy,
		requireHTTPS: d.RequireHTTPS, allowPrivate: d.AllowPrivateTargets,
		now: func() time.Time { return d.Clock().UTC() }, log: d.Logger,
	}, nil
}

// CreatedEndpoint is a new endpoint with its signing secret, shown once.
type CreatedEndpoint struct {
	Endpoint store.WebhookEndpoint
	Secret   string
}

// CreateEndpoint registers a webhook endpoint for the principal.
func (s *Service) CreateEndpoint(ctx context.Context, pr merchant.Principal, rawURL string, eventTypes []string, description string) (CreatedEndpoint, error) {
	if err := s.validateURL(rawURL); err != nil {
		return CreatedEndpoint{}, err
	}
	for _, t := range eventTypes {
		if !slices.Contains(EventTypes, t) {
			return CreatedEndpoint{}, fmt.Errorf("%w: unknown event type %q", ErrInvalid, t)
		}
	}
	if len(description) > 200 {
		return CreatedEndpoint{}, fmt.Errorf("%w: description must be at most 200 characters", ErrInvalid)
	}
	if eventTypes == nil {
		eventTypes = []string{}
	}
	publicID := ids.New(ids.WebhookEndpoint)
	secret := newSecret()
	ep, err := s.q.CreateWebhookEndpoint(ctx, store.CreateWebhookEndpointParams{
		PublicID: publicID, MerchantID: pr.MerchantID, Environment: string(pr.Environment), Url: rawURL,
		EventTypes: eventTypes, Description: description,
		SecretCiphertext: s.sealer.Seal([]byte(secret), []byte(publicID)), SecretKeyVersion: s.sealer.version,
	})
	if err != nil {
		return CreatedEndpoint{}, err
	}
	return CreatedEndpoint{Endpoint: ep, Secret: secret}, nil
}

func (s *Service) validateURL(raw string) error {
	if len(raw) > 2048 {
		return fmt.Errorf("%w: url must be at most 2048 characters", ErrInvalid)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("%w: url must be an absolute URL", ErrInvalid)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && !s.requireHTTPS:
	default:
		return fmt.Errorf("%w: url must use https", ErrInvalid)
	}
	if u.User != nil {
		return fmt.Errorf("%w: url must not contain credentials", ErrInvalid)
	}
	if u.Fragment != "" {
		return fmt.Errorf("%w: url must not contain a fragment", ErrInvalid)
	}
	if !s.allowPrivate && (literalBlocked(u.Hostname()) || strings.EqualFold(u.Hostname(), "localhost")) {
		return fmt.Errorf("%w: url must not point to a private or local address", ErrInvalid)
	}
	return nil
}

// ListEndpoints returns the principal's endpoints.
func (s *Service) ListEndpoints(ctx context.Context, pr merchant.Principal) ([]store.WebhookEndpoint, error) {
	return s.q.ListWebhookEndpoints(ctx, store.ListWebhookEndpointsParams{MerchantID: pr.MerchantID, Environment: string(pr.Environment)})
}

// GetEndpoint returns one of the principal's endpoints.
func (s *Service) GetEndpoint(ctx context.Context, pr merchant.Principal, id string) (store.WebhookEndpoint, error) {
	if _, err := ids.Parse(ids.WebhookEndpoint, id); err != nil {
		return store.WebhookEndpoint{}, ErrNotFound
	}
	ep, err := s.q.GetWebhookEndpoint(ctx, store.GetWebhookEndpointParams{PublicID: id, MerchantID: pr.MerchantID, Environment: string(pr.Environment)})
	if errors.Is(err, pgx.ErrNoRows) {
		return ep, ErrNotFound
	}
	return ep, err
}

// DisableEndpoint stops deliveries to an endpoint. Pending deliveries are
// marked dead when next attempted.
func (s *Service) DisableEndpoint(ctx context.Context, pr merchant.Principal, id string) (store.WebhookEndpoint, error) {
	if _, err := s.GetEndpoint(ctx, pr, id); err != nil {
		return store.WebhookEndpoint{}, err
	}
	if _, err := s.q.DisableWebhookEndpoint(ctx, store.DisableWebhookEndpointParams{PublicID: id, MerchantID: pr.MerchantID, Environment: string(pr.Environment)}); err != nil {
		return store.WebhookEndpoint{}, err
	}
	return s.GetEndpoint(ctx, pr, id)
}

// ListDeliveries returns the latest deliveries for one of the principal's endpoints.
func (s *Service) ListDeliveries(ctx context.Context, pr merchant.Principal, endpointID string, max int32) ([]store.WebhookDelivery, error) {
	ep, err := s.GetEndpoint(ctx, pr, endpointID)
	if err != nil {
		return nil, err
	}
	return s.q.ListDeliveriesForEndpoint(ctx, store.ListDeliveriesForEndpointParams{EndpointID: ep.ID, MaxRows: max})
}

// DeliveryDetail is a delivery with its endpoint and attempt history.
type DeliveryDetail struct {
	Delivery         store.WebhookDelivery
	EndpointPublicID string
	Attempts         []store.WebhookDeliveryAttempt
}

// GetDelivery returns one of the principal's deliveries with every attempt.
func (s *Service) GetDelivery(ctx context.Context, pr merchant.Principal, id string) (DeliveryDetail, error) {
	if _, err := ids.Parse(ids.WebhookDelivery, id); err != nil {
		return DeliveryDetail{}, ErrNotFound
	}
	row, err := s.q.GetDeliveryForMerchant(ctx, store.GetDeliveryForMerchantParams{PublicID: id, MerchantID: pr.MerchantID, Environment: string(pr.Environment)})
	if errors.Is(err, pgx.ErrNoRows) {
		return DeliveryDetail{}, ErrNotFound
	}
	if err != nil {
		return DeliveryDetail{}, err
	}
	attempts, err := s.q.ListAttemptsForDelivery(ctx, row.WebhookDelivery.ID)
	if err != nil {
		return DeliveryDetail{}, err
	}
	return DeliveryDetail{Delivery: row.WebhookDelivery, EndpointPublicID: row.EndpointPublicID, Attempts: attempts}, nil
}

// Replay schedules a finished (delivered or dead) delivery again with a
// fresh attempt budget. The body is unchanged; the signature is recomputed.
func (s *Service) Replay(ctx context.Context, pr merchant.Principal, id string) (DeliveryDetail, error) {
	d, err := s.GetDelivery(ctx, pr, id)
	if err != nil {
		return d, err
	}
	n, err := s.q.ReplayDelivery(ctx, store.ReplayDeliveryParams{ID: d.Delivery.ID, Now: s.now()})
	if err != nil {
		return d, err
	}
	if n == 0 {
		return d, ErrNotReplayable
	}
	return s.GetDelivery(ctx, pr, id)
}

// envelope is the delivered JSON body.
type envelope struct {
	ID         string    `json:"id"`
	Object     string    `json:"object"`
	Type       string    `json:"type"`
	APIVersion string    `json:"api_version"`
	CreatedAt  time.Time `json:"created_at"`
	Data       struct {
		Payment apiview.PaymentJSON `json:"payment"`
	} `json:"data"`
}

// FanOut creates one delivery per subscribed, enabled endpoint for an
// outbox event. Safe to call repeatedly and concurrently for the same event.
func (s *Service) FanOut(ctx context.Context, eventPublicID string) (int, error) {
	ev, err := s.q.GetOutboxEventByPublicID(ctx, eventPublicID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("webhooks: event %s not found", eventPublicID)
	}
	if err != nil {
		return 0, err
	}
	var endpoints []store.WebhookEndpoint
	var body []byte
	if ev.AggregateType == "payment" {
		owner, view, err := s.payments.ViewForEvent(ctx, ev.AggregateID)
		if err != nil {
			return 0, fmt.Errorf("webhooks: load payment for %s: %w", eventPublicID, err)
		}
		all, err := s.q.ListEnabledEndpoints(ctx, store.ListEnabledEndpointsParams{MerchantID: owner.MerchantID, Environment: owner.Environment})
		if err != nil {
			return 0, err
		}
		for _, ep := range all {
			if len(ep.EventTypes) == 0 || slices.Contains(ep.EventTypes, ev.EventType) {
				endpoints = append(endpoints, ep)
			}
		}
		env := envelope{ID: ev.PublicID, Object: "event", Type: ev.EventType, APIVersion: "v1", CreatedAt: ev.CreatedAt.UTC()}
		env.Data.Payment = apiview.Payment(view)
		if body, err = json.Marshal(env); err != nil {
			return 0, err
		}
	}

	created := 0
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		now := s.now()
		for _, ep := range endpoints {
			n, err := q.InsertWebhookDelivery(ctx, store.InsertWebhookDeliveryParams{
				PublicID: ids.New(ids.WebhookDelivery), EndpointID: ep.ID, EventID: ev.ID, EventPublicID: ev.PublicID,
				EventType: ev.EventType, Body: body, Now: now,
			})
			if err != nil {
				return err
			}
			created += int(n)
		}
		_, err := q.InsertWebhookFanout(ctx, store.InsertWebhookFanoutParams{EventID: ev.ID, Deliveries: int32(len(endpoints)), Now: now})
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("webhooks: fan out %s: %w", eventPublicID, err)
	}
	return created, nil
}

// SweepUnfanned fans out published events the broker consumer has not
// handled within the sweep delay, so a lost message only delays delivery.
func (s *Service) SweepUnfanned(ctx context.Context, max int32) (int, error) {
	evs, err := s.q.ListUnfannedEvents(ctx, store.ListUnfannedEventsParams{PublishedBefore: s.now().Add(-s.policy.SweepDelay), MaxRows: max})
	if err != nil {
		return 0, err
	}
	for _, ev := range evs {
		if _, err := s.FanOut(ctx, ev.PublicID); err != nil {
			s.log.ErrorContext(ctx, "sweep fan-out", "event_id", ev.PublicID, "err", err)
		}
	}
	return len(evs), nil
}

// DeliverDue claims due deliveries and attempts each once. It returns how
// many were attempted.
func (s *Service) DeliverDue(ctx context.Context, max int32) (int, error) {
	now := s.now()
	due, err := s.q.ClaimDueDeliveries(ctx, store.ClaimDueDeliveriesParams{LeaseUntil: now.Add(s.policy.Lease), Now: now, MaxRows: max})
	if err != nil {
		return 0, fmt.Errorf("webhooks: claim deliveries: %w", err)
	}
	for _, d := range due {
		if err := s.deliver(ctx, d); err != nil {
			s.log.ErrorContext(ctx, "deliver webhook", "delivery_id", d.PublicID, "err", err)
		}
	}
	return len(due), nil
}

// maxResponseBytes bounds how much of a merchant's response is read.
const maxResponseBytes = 4 << 10

func (s *Service) deliver(ctx context.Context, d store.WebhookDelivery) error {
	ep, err := s.q.GetWebhookEndpointByID(ctx, d.EndpointID)
	if err != nil {
		return err
	}
	if ep.Status != "enabled" {
		return s.q.DeferDelivery(ctx, d.ID)
	}
	secret, err := s.sealer.Open(ep.SecretCiphertext, []byte(ep.PublicID))
	if err != nil {
		return err
	}

	started := s.now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.Url, bytes.NewReader(d.Body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Axene-Webhooks/1")
	req.Header.Set("Axene-Event-Id", d.EventPublicID)
	req.Header.Set("Axene-Event-Type", d.EventType)
	req.Header.Set("Axene-Delivery-Id", d.PublicID)
	req.Header.Set(SignatureHeader, Sign(string(secret), started, d.Body))

	var status *int32
	var errMsg *string
	resp, herr := s.client.Do(req)
	if herr != nil {
		m := truncate(herr.Error(), 300)
		errMsg = &m
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		resp.Body.Close()
		code := int32(resp.StatusCode)
		status = &code
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			m := fmt.Sprintf("endpoint responded %d", resp.StatusCode)
			errMsg = &m
		}
	}
	elapsed := s.now().Sub(started)

	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := q.RecordDeliveryAttempt(ctx, store.RecordDeliveryAttemptParams{
			DeliveryID: d.ID, AttemptedAt: started, StatusCode: status, Error: errMsg, DurationMs: int32(elapsed.Milliseconds()),
		}); err != nil {
			return err
		}
		switch attempt := int(d.Attempts) + 1; {
		case errMsg == nil:
			return q.MarkDeliveryDelivered(ctx, store.MarkDeliveryDeliveredParams{ID: d.ID, StatusCode: status, Now: s.now()})
		case attempt >= s.policy.MaxAttempts:
			s.log.WarnContext(ctx, "webhook delivery dead", "delivery_id", d.PublicID, "endpoint_id", ep.PublicID, "attempts", attempt)
			return q.MarkDeliveryDead(ctx, store.MarkDeliveryDeadParams{ID: d.ID, StatusCode: status, Error: errMsg})
		default:
			return q.MarkDeliveryRetry(ctx, store.MarkDeliveryRetryParams{
				ID: d.ID, NextAttemptAt: s.now().Add(s.policy.backoff(attempt - 1)), StatusCode: status, Error: errMsg,
			})
		}
	})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
