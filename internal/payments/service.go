package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stan-ley-tech/axene-pay/internal/idempotency"
	"github.com/stan-ley-tech/axene-pay/internal/merchant"
	"github.com/stan-ley-tech/axene-pay/internal/orchestration"
	"github.com/stan-ley-tech/axene-pay/internal/platform/ids"
	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
	"github.com/stan-ley-tech/axene-pay/internal/providers"
	"github.com/stan-ley-tech/axene-pay/internal/store"
)

var (
	// ErrInvalid wraps request validation failures.
	ErrInvalid = errors.New("payments: invalid request")
	// ErrNotFound means the payment does not exist for this merchant and environment.
	ErrNotFound = errors.New("payments: not found")
	// ErrNotCancellable means a provider request may already be in flight.
	ErrNotCancellable = errors.New("payments: payment can no longer be cancelled")
	// ErrNoEligibleProvider means routing found no provider for the request.
	ErrNoEligibleProvider = orchestration.ErrNoEligibleProvider
	// ErrIdempotencyMismatch means the key was used for a different request.
	ErrIdempotencyMismatch = errors.New("payments: idempotency key reused for a different request")
	// ErrIdempotencyInProgress means an identical request is still running.
	ErrIdempotencyInProgress = errors.New("payments: request with this idempotency key is in progress")
)

// FinancialEffects records the financial consequences of payment outcomes.
// It runs inside the transition's transaction, so an outcome and its
// effects commit together, exactly once (internal/accounting).
type FinancialEffects interface {
	PaymentSucceeded(ctx context.Context, q *store.Queries, p store.Payment, requestID string) error
	PaymentReversed(ctx context.Context, q *store.Queries, p store.Payment, requestID string) error
}

// Clock returns the current time; injected so tests control time.
type Clock func() time.Time

// Policy holds operational timing parameters. They are configuration, not
// business rules, and are documented in docs/development.md.
type Policy struct {
	// ProviderTimeout bounds each provider call.
	ProviderTimeout time.Duration
	// InitialVerificationDelay: first status query after submission when no
	// callback arrives (the customer needs time to respond to the prompt).
	InitialVerificationDelay time.Duration
	// RetryBase and RetryMax shape exponential backoff for inconclusive
	// status queries.
	RetryBase time.Duration
	RetryMax  time.Duration
	// MaxVerificationAttempts before a payment becomes UNRESOLVED.
	MaxVerificationAttempts int
	// VerificationLease: how long a worker owns a claimed verification.
	VerificationLease time.Duration
	// SubmissionLease: a submission claim older than this without a recorded
	// outcome is treated as SUBMISSION_UNKNOWN.
	SubmissionLease time.Duration
	// UnclaimedSubmissionGrace: an attempt never claimed for this long is
	// submitted by the worker.
	UnclaimedSubmissionGrace time.Duration
	// IdempotencyTTL: how long idempotency keys are retained (D-10).
	IdempotencyTTL time.Duration
	// IdempotencyRecoverAfter: an in-progress key older than this whose
	// resource exists is answered from the resource's current state.
	IdempotencyRecoverAfter time.Duration
}

// DefaultPolicy returns conservative defaults.
func DefaultPolicy() Policy {
	return Policy{
		ProviderTimeout:          30 * time.Second,
		InitialVerificationDelay: 60 * time.Second,
		RetryBase:                15 * time.Second,
		RetryMax:                 5 * time.Minute,
		MaxVerificationAttempts:  12,
		VerificationLease:        2 * time.Minute,
		SubmissionLease:          2 * time.Minute,
		UnclaimedSubmissionGrace: 30 * time.Second,
		IdempotencyTTL:           24 * time.Hour,
		IdempotencyRecoverAfter:  2 * time.Minute,
	}
}

// Validate rejects inconsistent timing (e.g. a lease shorter than a call).
func (p Policy) Validate() error {
	switch {
	case p.ProviderTimeout <= 0, p.InitialVerificationDelay < 0, p.RetryBase <= 0,
		p.RetryMax < p.RetryBase, p.MaxVerificationAttempts < 1:
		return errors.New("payments: invalid verification policy")
	case p.SubmissionLease <= p.ProviderTimeout, p.VerificationLease <= p.ProviderTimeout:
		return errors.New("payments: leases must exceed the provider timeout")
	case p.IdempotencyTTL < 24*time.Hour:
		return errors.New("payments: idempotency keys must be retained at least 24h")
	case p.IdempotencyRecoverAfter <= p.ProviderTimeout:
		return errors.New("payments: idempotency recovery must wait longer than a provider call")
	}
	return nil
}

// backoff returns the delay before verification attempt n (0-based).
func (p Policy) backoff(n int) time.Duration {
	d := p.RetryBase
	for i := 0; i < n && d < p.RetryMax; i++ {
		d *= 2
	}
	return min(d, p.RetryMax)
}

// Service implements the payment lifecycle.
type Service struct {
	pool        *pgxpool.Pool
	q           *store.Queries
	providers   *providers.Registry
	callbackURL func(provider string) string
	policy      Policy
	now         Clock
	log         *slog.Logger
	metrics     Metrics
	effects     FinancialEffects
}

// Deps are the Service's collaborators.
type Deps struct {
	Pool      *pgxpool.Pool
	Providers *providers.Registry
	// CallbackURL returns the URL a provider must call back for its results.
	CallbackURL func(provider string) string
	Policy      Policy
	Clock       Clock
	Logger      *slog.Logger
	Metrics     Metrics
	// Effects records financial consequences of outcomes. Optional only for
	// tests of the lifecycle alone; processes always provide it.
	Effects FinancialEffects
}

// NewService validates deps and returns a Service.
func NewService(d Deps) (*Service, error) {
	if d.Pool == nil || d.Providers == nil || d.CallbackURL == nil || d.Logger == nil {
		return nil, errors.New("payments: missing dependency")
	}
	if err := d.Policy.Validate(); err != nil {
		return nil, err
	}
	if d.Clock == nil {
		d.Clock = time.Now
	}
	if d.Metrics == nil {
		d.Metrics = noMetrics{}
	}
	return &Service{
		pool: d.Pool, q: store.New(d.Pool), providers: d.Providers, callbackURL: d.CallbackURL,
		policy: d.Policy, now: func() time.Time { return d.Clock().UTC() }, log: d.Logger, metrics: d.Metrics,
		effects: d.Effects,
	}, nil
}

// CreateParams is a validated payment request.
type CreateParams struct {
	Amount            money.Amount
	Method            providers.Method
	PhoneNumber       string
	Description       string
	MerchantReference string
}

var msisdn = regexp.MustCompile(`^254[17][0-9]{8}$`)

// Validate checks request fields that do not depend on configuration.
func (p CreateParams) Validate() error {
	switch {
	case p.Amount.Currency().IsZero():
		return fmt.Errorf("%w: currency is required", ErrInvalid)
	case !p.Amount.IsPositive():
		return fmt.Errorf("%w: amount must be a positive integer in minor units", ErrInvalid)
	case p.Method != providers.MethodMpesa:
		return fmt.Errorf("%w: payment_method.type must be mpesa", ErrInvalid)
	case !msisdn.MatchString(p.PhoneNumber):
		return fmt.Errorf("%w: phone_number must be a Kenyan MSISDN like 254712345678", ErrInvalid)
	case len(p.Description) > 200:
		return fmt.Errorf("%w: description must be at most 200 characters", ErrInvalid)
	case len(p.MerchantReference) > 200:
		return fmt.Errorf("%w: merchant_reference must be at most 200 characters", ErrInvalid)
	}
	return nil
}

// Canonical is the deterministic representation used for idempotency
// fingerprints.
func (p CreateParams) Canonical() []byte {
	b, _ := json.Marshal(struct {
		Amount      int64  `json:"amount"`
		Currency    string `json:"currency"`
		Method      string `json:"method"`
		Phone       string `json:"phone"`
		Description string `json:"description"`
		Reference   string `json:"reference"`
	}{p.Amount.Minor(), p.Amount.Currency().Code(), string(p.Method), p.PhoneNumber, p.Description, p.MerchantReference})
	return b
}

// CreateResult is the outcome of Create. Exactly one of Payment (a payment
// was created or recovered) or Replay (a stored response) is set.
type CreateResult struct {
	Payment *View
	Replay  *store.IdempotencyKey
	// Key must be completed by the caller with the rendered response.
	Key idempotency.Key
}

// Create creates a payment and submits its first attempt.
func (s *Service) Create(ctx context.Context, pr merchant.Principal, key idempotency.Key, p CreateParams, requestID string) (CreateResult, error) {
	if err := p.Validate(); err != nil {
		return CreateResult{}, err
	}
	key.MerchantID, key.Environment = pr.MerchantID, string(pr.Environment)
	key.Fingerprint = idempotency.Fingerprint(p.Canonical())
	res := CreateResult{Key: key}

	// Replays must not depend on current routing configuration.
	existing, err := idempotency.Lookup(ctx, s.q, key, s.now())
	if err != nil {
		return res, err
	}
	if existing.State != idempotency.StateNew {
		return s.existingKey(ctx, pr, res, existing)
	}

	candidates, err := s.candidates(ctx, pr, p.Method)
	if err != nil {
		return res, err
	}
	decision, err := orchestration.Route(orchestration.Request{Method: p.Method, Amount: p.Amount}, candidates, s.providers)
	if err != nil {
		return res, err
	}

	var payment store.Payment
	var attempt store.PaymentAttempt
	var claimed idempotency.Result
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		claimed, err = idempotency.Claim(ctx, q, key, s.now(), s.policy.IdempotencyTTL)
		if err != nil || claimed.State != idempotency.StateNew {
			return err
		}
		destID, err := s.destinationUUID(ctx, q, pr, decision.Candidate.DestinationID)
		if err != nil {
			return err
		}
		var ref *string
		if p.MerchantReference != "" {
			ref = &p.MerchantReference
		}
		payment, err = q.InsertPayment(ctx, store.InsertPaymentParams{
			PublicID: ids.New(ids.Payment), MerchantID: pr.MerchantID, Environment: string(pr.Environment),
			AmountMinor: p.Amount.Minor(), Currency: p.Amount.Currency().Code(), Method: string(p.Method),
			PhoneNumber: p.PhoneNumber, Description: p.Description, MerchantReference: ref,
			SettlementDestinationID: destID, CreatedAt: s.now(),
		})
		if err != nil {
			return fmt.Errorf("payments: insert payment: %w", err)
		}
		attempt, err = q.InsertAttempt(ctx, store.InsertAttemptParams{
			PublicID: ids.New(ids.Attempt), PaymentID: payment.ID, Provider: decision.Provider.Name(),
			RoutingReason: decision.Reason, CreatedAt: s.now(),
		})
		if err != nil {
			return fmt.Errorf("payments: insert attempt: %w", err)
		}
		return idempotency.SetResource(ctx, q, key, payment.PublicID)
	})
	if err != nil {
		return res, err
	}
	if claimed.State != idempotency.StateNew { // lost a race with an identical request
		return s.existingKey(ctx, pr, res, claimed)
	}
	s.log.InfoContext(ctx, "payment created", "request_id", requestID, "payment_id", payment.PublicID,
		"attempt_id", attempt.PublicID, "provider", attempt.Provider)

	if err := s.SubmitAttempt(ctx, attempt.ID, requestID); err != nil {
		// The payment exists and the worker will finish the submission; report
		// the current state rather than failing the request.
		s.log.ErrorContext(ctx, "submit attempt", "request_id", requestID, "attempt_id", attempt.PublicID, "err", err)
	}
	v, err := s.view(ctx, payment.ID)
	if err != nil {
		return res, err
	}
	res.Payment = &v
	return res, nil
}

func (s *Service) existingKey(ctx context.Context, pr merchant.Principal, res CreateResult, r idempotency.Result) (CreateResult, error) {
	switch r.State {
	case idempotency.StateCompleted:
		res.Replay = &r.Record
		return res, nil
	case idempotency.StateMismatch:
		return res, ErrIdempotencyMismatch
	}
	// In progress. If its owner died after creating the payment, answer from
	// the payment's current state so the client is not blocked until expiry.
	if r.Record.ResourceID != nil && s.now().Sub(r.Record.CreatedAt) > s.policy.IdempotencyRecoverAfter {
		v, err := s.Get(ctx, pr, *r.Record.ResourceID)
		if err != nil {
			return res, err
		}
		res.Payment = &v
		return res, nil
	}
	return res, ErrIdempotencyInProgress
}

// CompleteIdempotency stores the rendered response for future replays.
func (s *Service) CompleteIdempotency(ctx context.Context, key idempotency.Key, status int, body []byte) error {
	return idempotency.Complete(ctx, s.q, key, status, body)
}

func (s *Service) candidates(ctx context.Context, pr merchant.Principal, m providers.Method) ([]orchestration.Candidate, error) {
	rows, err := s.q.ListRoutingCandidates(ctx, store.ListRoutingCandidatesParams{
		MerchantID: pr.MerchantID, Environment: string(pr.Environment), Method: string(m),
	})
	if err != nil {
		return nil, fmt.Errorf("payments: load routing configuration: %w", err)
	}
	out := make([]orchestration.Candidate, 0, len(rows))
	for _, r := range rows {
		out = append(out, orchestration.Candidate{
			ConfigID: r.ConfigPublicID, Provider: r.Provider, Priority: r.Priority, Enabled: r.Enabled,
			DestinationID: r.DestinationPublicID, DestinationActive: r.DestinationStatus == "active",
			Destination: providers.Destination{Type: r.DestinationType, Identifier: r.DestinationIdentifier},
		})
	}
	return out, nil
}

func (s *Service) destinationUUID(ctx context.Context, q *store.Queries, pr merchant.Principal, publicID string) (pgtype.UUID, error) {
	d, err := q.GetSettlementDestination(ctx, store.GetSettlementDestinationParams{PublicID: publicID, MerchantID: pr.MerchantID})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("payments: load destination: %w", err)
	}
	return d.ID, nil
}

// SubmitAttempt sends a PENDING_SUBMISSION attempt to its provider. It is
// safe to call concurrently and repeatedly: only the caller that claims the
// attempt calls the provider, and only once.
func (s *Service) SubmitAttempt(ctx context.Context, attemptID pgtype.UUID, requestID string) error {
	n, err := s.q.ClaimAttemptSubmission(ctx, store.ClaimAttemptSubmissionParams{ID: attemptID, Now: s.now()})
	if err != nil {
		return fmt.Errorf("payments: claim submission: %w", err)
	}
	if n == 0 {
		return nil // already claimed elsewhere, or no longer pending
	}
	attempt, err := s.q.GetAttempt(ctx, attemptID)
	if err != nil {
		return err
	}
	payment, err := s.q.GetPaymentByID(ctx, attempt.PaymentID)
	if err != nil {
		return err
	}
	dest, err := s.q.GetSettlementDestinationByID(ctx, payment.SettlementDestinationID)
	if err != nil {
		return err
	}
	amount, err := paymentAmount(payment)
	if err != nil {
		return err
	}

	provider, perr := s.providers.Get(attempt.Provider)
	var out providers.SubmitOutcome
	if perr == nil {
		callCtx, cancel := context.WithTimeout(ctx, s.policy.ProviderTimeout)
		started := s.now()
		out, perr = provider.Submit(callCtx, providers.SubmitRequest{
			AttemptID: attempt.PublicID, Amount: amount, Method: providers.Method(payment.Method),
			PhoneNumber: payment.PhoneNumber, Description: payment.Description,
			Destination: providers.Destination{Type: dest.Type, Identifier: dest.Identifier},
			CallbackURL: s.callbackURL(attempt.Provider),
		})
		cancel()
		s.metrics.ProviderCall(attempt.Provider, "submit", string(out.Class), s.now().Sub(started))
	}

	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		payment, err := q.LockPayment(ctx, payment.ID)
		if err != nil {
			return err
		}
		attempt, err := q.GetAttempt(ctx, attemptID)
		if err != nil {
			return err
		}
		if AttemptStatus(attempt.Status) != AttemptPendingSubmission {
			return nil
		}
		now := s.now()
		actor := "system:submission"
		if perr != nil {
			// Nothing was sent: provider missing from this deployment or a
			// configuration fault. Failing is safe and visible.
			reason := "provider unavailable: " + perr.Error()
			if err := s.setAttempt(ctx, q, attempt, AttemptAbandoned, attemptUpdate{resolvedAt: &now, rawMessage: &reason}); err != nil {
				return err
			}
			_, err := s.transition(ctx, q, payment, change{to: StatusFailed, evidence: EvidenceRouting, actor: actor,
				reason: reason, attemptID: attempt.ID, requestID: requestID})
			return err
		}
		upd := attemptUpdate{submittedAt: &now, rawCode: nonEmpty(out.RawCode), rawMessage: nonEmpty(out.RawMessage), providerRef: nonEmpty(out.ProviderReference)}
		if p, _ := s.providers.Get(attempt.Provider); p != nil && p.Capabilities().Has(providers.CapStatusQuery) {
			at := now.Add(s.policy.InitialVerificationDelay)
			upd.nextVerification = &at
		}
		switch out.Class {
		case providers.ClassAccepted:
			if err := s.setAttempt(ctx, q, attempt, AttemptSubmitted, upd); err != nil {
				return err
			}
			_, err = s.transition(ctx, q, payment, change{to: StatusPendingProvider, evidence: EvidenceSubmission, actor: actor, attemptID: attempt.ID, requestID: requestID})
		case providers.ClassRejected:
			upd.nextVerification, upd.resolvedAt = nil, &now
			if err := s.setAttempt(ctx, q, attempt, AttemptRejected, upd); err != nil {
				return err
			}
			_, err = s.transition(ctx, q, payment, change{to: StatusFailed, evidence: EvidenceSubmissionRejected, actor: actor,
				reason: "rejected by provider: " + out.RawMessage, attemptID: attempt.ID, requestID: requestID})
		default:
			// Unknown, or a contract violation (a final outcome on submit): the
			// provider may have the request. Never resubmit; verify instead.
			if out.Class != providers.ClassUnknown {
				s.log.WarnContext(ctx, "provider returned unexpected class on submit", "provider", attempt.Provider, "class", out.Class)
			}
			if err := s.setAttempt(ctx, q, attempt, AttemptSubmissionUnknown, upd); err != nil {
				return err
			}
			_, err = s.transition(ctx, q, payment, change{to: StatusPendingProvider, evidence: EvidenceSubmission, actor: actor,
				reason: "submission outcome unknown", attemptID: attempt.ID, requestID: requestID})
		}
		return err
	})
}

// Cancel cancels a payment that has not been sent to a provider.
func (s *Service) Cancel(ctx context.Context, pr merchant.Principal, paymentID, requestID string) (View, error) {
	p, err := s.lookup(ctx, s.q, pr, paymentID)
	if err != nil {
		return View{}, err
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		p, err := q.LockPayment(ctx, p.ID)
		if err != nil {
			return err
		}
		switch Status(p.Status) {
		case StatusCancelled:
			return nil
		case StatusCreated:
		default:
			return ErrNotCancellable
		}
		attempts, err := q.ListAttemptsForPayment(ctx, p.ID)
		if err != nil {
			return err
		}
		now := s.now()
		for _, a := range attempts {
			if !AttemptStatus(a.Status).IsOpen() {
				continue
			}
			// Claiming blocks a concurrent submitter; if it already claimed,
			// the request may be in flight and cannot be withdrawn.
			n, err := q.ClaimAttemptSubmission(ctx, store.ClaimAttemptSubmissionParams{ID: a.ID, Now: now})
			if err != nil {
				return err
			}
			if n == 0 {
				return ErrNotCancellable
			}
			if err := s.setAttempt(ctx, q, a, AttemptAbandoned, attemptUpdate{resolvedAt: &now}); err != nil {
				return err
			}
		}
		_, err = s.transition(ctx, q, p, change{to: StatusCancelled, evidence: EvidenceMerchant, actor: "api:" + pr.KeyID, requestID: requestID})
		return err
	})
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, p.ID)
}

// Get returns a payment visible to the principal.
func (s *Service) Get(ctx context.Context, pr merchant.Principal, paymentID string) (View, error) {
	p, err := s.lookup(ctx, s.q, pr, paymentID)
	if err != nil {
		return View{}, err
	}
	return s.view(ctx, p.ID)
}

func (s *Service) lookup(ctx context.Context, q *store.Queries, pr merchant.Principal, paymentID string) (store.Payment, error) {
	if _, err := ids.Parse(ids.Payment, paymentID); err != nil {
		return store.Payment{}, ErrNotFound
	}
	p, err := q.GetPaymentForMerchant(ctx, store.GetPaymentForMerchantParams{
		PublicID: paymentID, MerchantID: pr.MerchantID, Environment: string(pr.Environment),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Payment{}, ErrNotFound
	}
	return p, err
}

// change describes one payment transition.
type change struct {
	to         Status
	evidence   Evidence
	actor      string
	reason     string
	attemptID  pgtype.UUID
	callbackID pgtype.UUID
	requestID  string
}

// transition applies c within q's transaction. The caller must hold the
// payment's row lock. It writes the status, one transition row, and one
// outbox event; re-applying the current status writes nothing.
func (s *Service) transition(ctx context.Context, q *store.Queries, p store.Payment, c change) (bool, error) {
	from := Status(p.Status)
	noop, err := CheckTransition(from, c.to, c.evidence)
	if err != nil || noop {
		return false, err
	}
	reason := nonEmpty(c.reason)
	n, err := q.UpdatePaymentStatus(ctx, store.UpdatePaymentStatusParams{ID: p.ID, ToStatus: string(c.to), FromStatus: string(from), StatusReason: reason})
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, fmt.Errorf("payments: %s changed concurrently", p.PublicID)
	}
	tr, err := q.InsertTransition(ctx, store.InsertTransitionParams{
		PaymentID: p.ID, AttemptID: c.attemptID, CallbackID: c.callbackID, FromStatus: string(from),
		ToStatus: string(c.to), Evidence: string(c.evidence), Actor: c.actor, Reason: reason, RequestID: c.requestID,
	})
	if err != nil {
		return false, err
	}
	payload, err := json.Marshal(eventPayload{
		PaymentID: p.PublicID, Environment: p.Environment, Status: string(c.to), PreviousStatus: string(from),
		Evidence: string(c.evidence), Amount: p.AmountMinor, Currency: p.Currency, OccurredAt: tr.CreatedAt,
	})
	if err != nil {
		return false, err
	}
	if _, err := q.InsertOutboxEvent(ctx, store.InsertOutboxEventParams{
		PublicID: ids.New(ids.Event), EventType: "payment." + strings.ToLower(string(c.to)),
		AggregateType: "payment", AggregateID: p.PublicID, TransitionID: &tr.ID, Payload: payload, RequestID: c.requestID,
	}); err != nil {
		return false, err
	}
	if s.effects != nil {
		updated := p
		updated.Status = string(c.to)
		updated.UpdatedAt = tr.CreatedAt
		switch c.to {
		case StatusSucceeded:
			err = s.effects.PaymentSucceeded(ctx, q, updated, c.requestID)
		case StatusReversed:
			err = s.effects.PaymentReversed(ctx, q, updated, c.requestID)
		}
		if err != nil {
			return false, fmt.Errorf("payments: financial effects of %s: %w", c.to, err)
		}
	}
	s.metrics.Transition(from, c.to, c.evidence)
	s.log.InfoContext(ctx, "payment transition", "request_id", c.requestID, "payment_id", p.PublicID,
		"from", from, "to", c.to, "evidence", c.evidence, "actor", c.actor)
	return true, nil
}

// eventPayload is the outbox event body. It carries identifiers and minimal
// state only; no personal data (ADR-0004).
type eventPayload struct {
	PaymentID      string    `json:"payment_id"`
	Environment    string    `json:"environment"`
	Status         string    `json:"status"`
	PreviousStatus string    `json:"previous_status"`
	Evidence       string    `json:"evidence"`
	Amount         int64     `json:"amount"`
	Currency       string    `json:"currency"`
	OccurredAt     time.Time `json:"occurred_at"`
}

type attemptUpdate struct {
	providerRef, receipt, rawCode, rawMessage *string
	submittedAt, resolvedAt, nextVerification *time.Time
	verificationIncrement                     int32
}

func (s *Service) setAttempt(ctx context.Context, q *store.Queries, a store.PaymentAttempt, to AttemptStatus, u attemptUpdate) error {
	from := AttemptStatus(a.Status)
	if _, err := CheckAttemptTransition(from, to); err != nil {
		return err
	}
	n, err := q.UpdateAttemptStatus(ctx, store.UpdateAttemptStatusParams{
		ID: a.ID, FromStatus: string(from), ToStatus: string(to),
		ProviderReference: u.providerRef, ProviderReceipt: u.receipt, LastRawCode: u.rawCode, LastRawMessage: u.rawMessage,
		SubmittedAt: u.submittedAt, ResolvedAt: u.resolvedAt, NextVerificationAt: u.nextVerification,
		VerificationIncrement: u.verificationIncrement,
	})
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("payments: attempt %s changed concurrently", a.PublicID)
	}
	return nil
}

func paymentAmount(p store.Payment) (money.Amount, error) {
	c, err := money.ParseCurrency(p.Currency)
	if err != nil {
		return money.Amount{}, err
	}
	return money.New(p.AmountMinor, c)
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
