package fees

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stan-ley-tech/axene-pay/internal/audit"
	"github.com/stan-ley-tech/axene-pay/internal/ledger"
	"github.com/stan-ley-tech/axene-pay/internal/platform/ids"
	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
	"github.com/stan-ley-tech/axene-pay/internal/store"
)

var (
	// ErrInvalid wraps schedule and request validation failures.
	ErrInvalid = errors.New("fees: invalid request")
	// ErrNotFound means the schedule or fee does not exist.
	ErrNotFound = errors.New("fees: not found")
	// ErrOverlap means an active schedule already covers the scope and period.
	ErrOverlap = errors.New("fees: an active schedule already covers this scope and period")
	// ErrNotApproved means the terms need business approval before activation.
	ErrNotApproved = errors.New("fees: terms require business approval that is not recorded")
	// ErrMechanismUnavailable means the collection mechanism is not permitted here.
	ErrMechanismUnavailable = errors.New("fees: collection mechanism not available")
)

// Mechanism is how Axene collects a merchant's fees (ADR-0011).
type Mechanism string

const (
	// MechanismNone: no confirmed mechanism; fees accrue as receivables.
	MechanismNone Mechanism = "none_configured"
	// MechanismSimulated: development/test collector. Refused in production.
	MechanismSimulated Mechanism = "simulated"
)

// ScheduleInput describes a new (draft) fee schedule.
type ScheduleInput struct {
	MerchantID        pgtype.UUID // zero: platform default
	Environment       string
	Method            string
	Provider          string // empty: any provider
	Currency          string
	Terms             Terms
	FeePayer          string // merchant | customer
	ProviderFeeBearer string // merchant | axene | customer
	ReversalPolicy    string // refund_fee | retain_fee | pending_approval
	EffectiveFrom     time.Time
	EffectiveTo       *time.Time
	Description       string
}

// Collector performs automatic fee collection for one mechanism. Collect
// must be idempotent per fee ID: calling it again for a fee it already
// collected returns the same reference.
type Collector interface {
	Mechanism() Mechanism
	Collect(ctx context.Context, f CollectRequest) (CollectOutcome, error)
}

// CollectRequest identifies a fee to collect.
type CollectRequest struct {
	FeeID      string
	PaymentID  string
	MerchantID string
	Amount     money.Amount
}

// CollectClass normalizes a collector's result.
type CollectClass string

const (
	CollectSucceeded CollectClass = "collected"
	CollectFailed    CollectClass = "failed"
	CollectUnknown   CollectClass = "unknown" // retried; never treated as failure
)

// CollectOutcome is a collector's result.
type CollectOutcome struct {
	Class     CollectClass
	Reference string // provider or mechanism reference: the collection evidence
	Message   string
}

// Policy holds collection timing.
type Policy struct {
	RetryBase   time.Duration
	RetryMax    time.Duration
	Lease       time.Duration
	CallTimeout time.Duration
}

// DefaultPolicy returns conservative operational defaults.
func DefaultPolicy() Policy {
	return Policy{RetryBase: time.Minute, RetryMax: time.Hour, Lease: 2 * time.Minute, CallTimeout: 30 * time.Second}
}

// Service manages schedules and fees.
type Service struct {
	pool       *pgxpool.Pool
	q          *store.Queries
	collectors map[Mechanism]Collector
	production bool
	policy     Policy
	now        func() time.Time
	log        *slog.Logger
}

// Deps configure a Service.
type Deps struct {
	Pool       *pgxpool.Pool
	Collectors []Collector
	// Production refuses the simulated mechanism.
	Production bool
	Policy     Policy
	Clock      func() time.Time
	Logger     *slog.Logger
}

// NewService returns a Service.
func NewService(d Deps) (*Service, error) {
	if d.Pool == nil || d.Logger == nil {
		return nil, errors.New("fees: missing dependency")
	}
	if d.Clock == nil {
		d.Clock = time.Now
	}
	s := &Service{pool: d.Pool, q: store.New(d.Pool), collectors: map[Mechanism]Collector{}, production: d.Production,
		policy: d.Policy, now: func() time.Time { return d.Clock().UTC() }, log: d.Logger}
	for _, c := range d.Collectors {
		if d.Production && c.Mechanism() == MechanismSimulated {
			return nil, fmt.Errorf("%w: simulated collector refused in production", ErrMechanismUnavailable)
		}
		s.collectors[c.Mechanism()] = c
	}
	return s, nil
}

// CreateSchedule stores a draft schedule. Drafts never price payments.
func (s *Service) CreateSchedule(ctx context.Context, in ScheduleInput) (store.FeeSchedule, error) {
	if err := in.Terms.Validate(); err != nil {
		return store.FeeSchedule{}, err
	}
	if _, err := money.ParseCurrency(in.Currency); err != nil {
		return store.FeeSchedule{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	switch {
	case in.Environment != "sandbox" && in.Environment != "live":
		return store.FeeSchedule{}, fmt.Errorf("%w: environment must be sandbox or live", ErrInvalid)
	case in.Method != "mpesa":
		return store.FeeSchedule{}, fmt.Errorf("%w: method must be mpesa", ErrInvalid)
	case in.FeePayer != "merchant" && in.FeePayer != "customer":
		return store.FeeSchedule{}, fmt.Errorf("%w: fee payer must be merchant or customer", ErrInvalid)
	case in.ProviderFeeBearer != "merchant" && in.ProviderFeeBearer != "axene" && in.ProviderFeeBearer != "customer":
		return store.FeeSchedule{}, fmt.Errorf("%w: provider fee bearer must be merchant, axene or customer", ErrInvalid)
	case in.ReversalPolicy != "refund_fee" && in.ReversalPolicy != "retain_fee" && in.ReversalPolicy != "pending_approval":
		return store.FeeSchedule{}, fmt.Errorf("%w: reversal policy must be refund_fee, retain_fee or pending_approval", ErrInvalid)
	case in.EffectiveFrom.IsZero():
		return store.FeeSchedule{}, fmt.Errorf("%w: effective_from is required", ErrInvalid)
	case in.EffectiveTo != nil && !in.EffectiveTo.After(in.EffectiveFrom):
		return store.FeeSchedule{}, fmt.Errorf("%w: effective_to must be after effective_from", ErrInvalid)
	}
	var provider *string
	if in.Provider != "" {
		provider = &in.Provider
	}
	sc, err := s.q.CreateFeeSchedule(ctx, store.CreateFeeScheduleParams{
		PublicID: ids.New(ids.FeeSchedule), MerchantID: in.MerchantID, Environment: in.Environment, Method: in.Method,
		Provider: provider, Currency: in.Currency, PercentagePpm: int32(in.Terms.PercentagePPM), FixedMinor: in.Terms.Fixed,
		MinimumMinor: in.Terms.Minimum, MaximumMinor: in.Terms.Maximum, Rounding: string(in.Terms.Rounding),
		FeePayer: in.FeePayer, ProviderFeeBearer: in.ProviderFeeBearer, ReversalPolicy: in.ReversalPolicy,
		EffectiveFrom: in.EffectiveFrom.UTC(), EffectiveTo: in.EffectiveTo, Description: in.Description,
	})
	if pgCode(err) == "23514" {
		return store.FeeSchedule{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return sc, err
}

// Activate makes a draft schedule price payments. It records who approved
// the terms and where; schedules whose customer-borne fees have no approved
// flow cannot be activated (ADR-0011 §6).
func (s *Service) Activate(ctx context.Context, scheduleID, approvedBy, approvalReference string) (store.FeeSchedule, error) {
	if strings.TrimSpace(approvedBy) == "" || strings.TrimSpace(approvalReference) == "" {
		return store.FeeSchedule{}, fmt.Errorf("%w: approved_by and approval_reference are required", ErrInvalid)
	}
	sc, err := s.q.GetFeeSchedule(ctx, scheduleID)
	if errors.Is(err, pgx.ErrNoRows) {
		return sc, ErrNotFound
	}
	if err != nil {
		return sc, err
	}
	if sc.FeePayer == "customer" || sc.ProviderFeeBearer == "customer" {
		return sc, fmt.Errorf("%w: customer-borne fees need an approved surcharge flow (D-12)", ErrNotApproved)
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		n, err := q.ActivateFeeSchedule(ctx, store.ActivateFeeScheduleParams{
			PublicID: scheduleID, ApprovedBy: &approvedBy, ApprovalReference: &approvalReference, Now: s.now(),
		})
		if pgCode(err) == "23P01" {
			return ErrOverlap
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: only draft schedules can be activated", ErrInvalid)
		}
		return audit.Record(ctx, q, approvedBy, "fee_schedule.activate", "fee_schedule", scheduleID,
			map[string]any{"approval_reference": approvalReference}, "activate-"+scheduleID)
	})
	if err != nil {
		return sc, err
	}
	return s.q.GetFeeSchedule(ctx, scheduleID)
}

// End stops an active schedule from pricing payments created at or after at.
func (s *Service) End(ctx context.Context, scheduleID string, at time.Time, operator string) (store.FeeSchedule, error) {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		n, err := q.EndFeeSchedule(ctx, store.EndFeeScheduleParams{PublicID: scheduleID, EndsAt: at.UTC()})
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: schedule is not active or already ends earlier", ErrInvalid)
		}
		return audit.Record(ctx, q, operator, "fee_schedule.end", "fee_schedule", scheduleID,
			map[string]any{"ends_at": at.UTC()}, "end-"+scheduleID)
	})
	if err != nil {
		return store.FeeSchedule{}, err
	}
	return s.q.GetFeeSchedule(ctx, scheduleID)
}

// SetMechanism configures how a merchant's fees are collected. Mechanisms
// without a registered collector are refused; existing accrued fees become
// due for collection.
func (s *Service) SetMechanism(ctx context.Context, merchantID pgtype.UUID, environment string, m Mechanism, operator string) error {
	if m != MechanismNone {
		if _, ok := s.collectors[m]; !ok {
			return fmt.Errorf("%w: %s", ErrMechanismUnavailable, m)
		}
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := q.SetFeeCollectionMechanism(ctx, store.SetFeeCollectionMechanismParams{
			MerchantID: merchantID, Environment: environment, Mechanism: string(m),
		}); err != nil {
			if pgCode(err) == "23514" {
				return fmt.Errorf("%w: %s is not permitted by the schema", ErrMechanismUnavailable, m)
			}
			return err
		}
		if err := audit.Record(ctx, q, operator, "fee_terms.set_mechanism", "merchant", merchantUUIDString(merchantID),
			map[string]any{"environment": environment, "mechanism": m}, "terms-"+environment); err != nil {
			return err
		}
		if m == MechanismNone {
			return nil
		}
		_, err := q.ScheduleAccruedFees(ctx, store.ScheduleAccruedFeesParams{MerchantID: merchantID, Environment: environment, Now: s.now()})
		return err
	})
}

func (s *Service) mechanism(ctx context.Context, q *store.Queries, merchantID pgtype.UUID, environment string) (Mechanism, error) {
	m, err := q.GetFeeCollectionMechanism(ctx, store.GetFeeCollectionMechanismParams{MerchantID: merchantID, Environment: environment})
	if errors.Is(err, pgx.ErrNoRows) {
		return MechanismNone, nil
	}
	return Mechanism(m), err
}

// PaymentFacts describe a succeeded payment for accrual.
type PaymentFacts struct {
	PaymentID        pgtype.UUID
	PaymentPublicID  string
	MerchantID       pgtype.UUID
	MerchantPublicID string
	Environment      string
	Method           string
	Provider         string
	Amount           money.Amount
	CreatedAt        time.Time
}

// ErrNoSchedule means no active schedule priced the payment.
var ErrNoSchedule = errors.New("fees: no active fee schedule")

// Accrue records Axene's fee for a succeeded payment and posts the accrual,
// inside the caller's transaction (q). It is idempotent per payment. The
// schedule in effect when the payment was created applies. With no schedule
// it returns ErrNoSchedule and records nothing: the caller opens an
// exception rather than accruing a silent zero fee.
func (s *Service) Accrue(ctx context.Context, q *store.Queries, p PaymentFacts, requestID string) (store.AxeneFee, error) {
	if existing, err := q.GetFeeByPaymentID(ctx, p.PaymentID); err == nil {
		return existing, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return store.AxeneFee{}, err
	}
	sc, err := q.SelectFeeSchedule(ctx, store.SelectFeeScheduleParams{
		Environment: p.Environment, Method: p.Method, Currency: p.Amount.Currency().Code(),
		MerchantID: p.MerchantID, Provider: &p.Provider, At: p.CreatedAt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.AxeneFee{}, ErrNoSchedule
	}
	if err != nil {
		return store.AxeneFee{}, err
	}
	b, err := Calculate(termsOf(sc), p.Amount)
	if err != nil {
		return store.AxeneFee{}, err
	}
	now := s.now()
	mech, err := s.mechanism(ctx, q, p.MerchantID, p.Environment)
	if err != nil {
		return store.AxeneFee{}, err
	}
	var next *time.Time
	if mech != MechanismNone && b.Total.IsPositive() {
		next = &now
	}
	fee, err := q.InsertAxeneFee(ctx, store.InsertAxeneFeeParams{
		PublicID: ids.New(ids.Fee), PaymentID: p.PaymentID, MerchantID: p.MerchantID, Environment: p.Environment,
		ScheduleID: sc.ID, Currency: p.Amount.Currency().Code(), BaseAmountMinor: p.Amount.Minor(),
		PercentageMinor: b.Percentage.Minor(), FixedMinor: b.Fixed.Minor(), FeeMinor: b.Total.Minor(),
		FeePayer: sc.FeePayer, ProviderFeeBearer: sc.ProviderFeeBearer, ReversalPolicy: sc.ReversalPolicy,
		NextCollectionAt: next, CreatedAt: now,
	})
	if errors.Is(err, pgx.ErrNoRows) { // concurrent accrual won
		return q.GetFeeByPaymentID(ctx, p.PaymentID)
	}
	if err != nil {
		return store.AxeneFee{}, err
	}
	reason := "schedule " + sc.PublicID
	if err := q.InsertFeeTransition(ctx, store.InsertFeeTransitionParams{
		FeeID: fee.ID, ToStatus: string(StatusAccrued), Actor: "system:accrual", Reason: &reason, RequestID: requestID,
	}); err != nil {
		return store.AxeneFee{}, err
	}
	if b.Total.IsPositive() {
		if _, err := ledger.Post(ctx, q, ledger.Entry{
			Book: ledger.BookAxene, Reason: ledger.ReasonFeeAccrued, SourceType: "fee", SourceID: fee.PublicID,
			Description: fmt.Sprintf("Axene fee for %s (%s)", p.PaymentPublicID, sc.PublicID), EffectiveAt: now, RequestID: requestID,
			Lines: []ledger.Line{
				{Account: receivableAccount(p), Direction: ledger.Debit, Amount: b.Total},
				{Account: ledger.Account{Book: ledger.BookAxene, Code: "fee_revenue", Type: ledger.Revenue}, Direction: ledger.Credit, Amount: b.Total},
			},
		}); err != nil {
			return store.AxeneFee{}, err
		}
	}
	return fee, nil
}

func receivableAccount(p PaymentFacts) ledger.Account {
	return ledger.Account{Book: ledger.BookAxene, Code: "fees_receivable:" + p.MerchantPublicID, Type: ledger.Asset, MerchantID: p.MerchantID}
}

func termsOf(sc store.FeeSchedule) Terms {
	return Terms{PercentagePPM: int64(sc.PercentagePpm), Fixed: sc.FixedMinor, Minimum: sc.MinimumMinor,
		Maximum: sc.MaximumMinor, Rounding: money.RoundingMode(sc.Rounding)}
}

// ReversalOutcome tells the caller what a reversal did to the fee.
type ReversalOutcome string

const (
	ReversalNoFee          ReversalOutcome = "no_fee"
	ReversalFeeRefunded    ReversalOutcome = "fee_refunded"
	ReversalFeeRetained    ReversalOutcome = "fee_retained"
	ReversalReviewRequired ReversalOutcome = "review_required"
)

// ApplyReversal applies the fee's reversal policy when its payment is
// reversed, inside the caller's transaction. A collected fee always needs
// review (a billing adjustment), whatever the policy.
func (s *Service) ApplyReversal(ctx context.Context, q *store.Queries, paymentID pgtype.UUID, merchantPublicID, requestID string) (ReversalOutcome, error) {
	fee, err := q.GetFeeByPaymentID(ctx, paymentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReversalNoFee, nil
	}
	if err != nil {
		return "", err
	}
	fee, err = q.LockFee(ctx, fee.ID)
	if err != nil {
		return "", err
	}
	st := Status(fee.Status)
	switch {
	case st.IsTerminal():
		return ReversalFeeRefunded, nil
	case st == StatusCollected || st == StatusCollectionPending || fee.ReversalPolicy == "pending_approval":
		reason := "payment reversed; reversal policy " + fee.ReversalPolicy
		return ReversalReviewRequired, s.transition(ctx, q, fee, StatusReviewRequired, change{actor: "system:reversal", reason: reason, requestID: requestID})
	case fee.ReversalPolicy == "retain_fee":
		return ReversalFeeRetained, nil
	default: // refund_fee, not yet collected
		if fee.FeeMinor > 0 {
			if _, err := ledger.Reverse(ctx, q, "fee", fee.PublicID, ledger.ReasonFeeAccrued, ledger.ReasonFeeReversed,
				"fee refunded after payment reversal", requestID, s.now()); err != nil {
				return "", err
			}
		}
		return ReversalFeeRefunded, s.transition(ctx, q, fee, StatusRefunded, change{actor: "system:reversal", reason: "payment reversed; refund_fee policy", requestID: requestID})
	}
}

// Waive writes off a fee that has not been collected, with a recorded
// operator and reason, and reverses its accrual.
func (s *Service) Waive(ctx context.Context, feeID, operator, reason, requestID string) (store.AxeneFee, error) {
	if strings.TrimSpace(operator) == "" || strings.TrimSpace(reason) == "" {
		return store.AxeneFee{}, fmt.Errorf("%w: operator and reason are required", ErrInvalid)
	}
	var out store.AxeneFee
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		fee, err := s.lockByPublicID(ctx, q, feeID)
		if err != nil {
			return err
		}
		if Status(fee.Status) == StatusWaived {
			return fmt.Errorf("%w: fee is already waived", ErrInvalidTransition)
		}
		if _, err := CheckTransition(Status(fee.Status), StatusWaived); err != nil {
			return err
		}
		if fee.FeeMinor > 0 {
			if _, err := ledger.Reverse(ctx, q, "fee", fee.PublicID, ledger.ReasonFeeAccrued, ledger.ReasonFeeReversed,
				"fee waived: "+reason, requestID, s.now()); err != nil {
				return err
			}
		}
		if err := s.transition(ctx, q, fee, StatusWaived, change{actor: "operator:" + operator, reason: reason, requestID: requestID}); err != nil {
			return err
		}
		if err := audit.Record(ctx, q, operator, "fee.waive", "fee", fee.PublicID, map[string]any{"reason": reason, "amount": fee.FeeMinor}, requestID); err != nil {
			return err
		}
		out, err = q.GetFeeByPublicID(ctx, feeID)
		return err
	})
	return out, err
}

// RecordCollectionEvidence marks a fee collected from external evidence (a
// reconciled settlement line), inside the caller's transaction.
func (s *Service) RecordCollectionEvidence(ctx context.Context, q *store.Queries, fee store.AxeneFee, merchantPublicID, mechanism, reference, evidence, requestID string) error {
	fee, err := q.LockFee(ctx, fee.ID)
	if err != nil {
		return err
	}
	return s.markCollected(ctx, q, fee, merchantPublicID, mechanism, reference, evidence, "system:reconciliation", requestID)
}

func (s *Service) markCollected(ctx context.Context, q *store.Queries, fee store.AxeneFee, merchantPublicID, mechanism, reference, evidence, actor, requestID string) error {
	if noop, err := CheckTransition(Status(fee.Status), StatusCollected); err != nil || noop {
		return err
	}
	cur, err := money.ParseCurrency(fee.Currency)
	if err != nil {
		return err
	}
	amt, _ := money.New(fee.FeeMinor, cur)
	now := s.now()
	if amt.IsPositive() {
		if _, err := ledger.Post(ctx, q, ledger.Entry{
			Book: ledger.BookAxene, Reason: ledger.ReasonFeeCollected, SourceType: "fee", SourceID: fee.PublicID,
			Description: "fee collected via " + mechanism + " (" + reference + ")", EffectiveAt: now, RequestID: requestID,
			Lines: []ledger.Line{
				{Account: ledger.Account{Book: ledger.BookAxene, Code: "fee_collections:" + mechanism, Type: ledger.Asset}, Direction: ledger.Debit, Amount: amt},
				{Account: ledger.Account{Book: ledger.BookAxene, Code: "fees_receivable:" + merchantPublicID, Type: ledger.Asset, MerchantID: fee.MerchantID}, Direction: ledger.Credit, Amount: amt},
			},
		}); err != nil {
			return err
		}
	}
	return s.transition(ctx, q, fee, StatusCollected, change{actor: actor, reason: "collected via " + mechanism, evidence: evidence,
		mechanism: &mechanism, reference: &reference, evidenceRef: &evidence, collectedAt: &now, requestID: requestID})
}

type change struct {
	actor, reason, evidence, requestID string
	mechanism, reference, evidenceRef  *string
	collectedAt, next                  *time.Time
	attemptIncrement                   int32
}

func (s *Service) transition(ctx context.Context, q *store.Queries, fee store.AxeneFee, to Status, c change) error {
	from := Status(fee.Status)
	noop, err := CheckTransition(from, to)
	if err != nil {
		return err
	}
	if noop && c.attemptIncrement == 0 && c.next == nil {
		return nil
	}
	n, err := q.UpdateFeeStatus(ctx, store.UpdateFeeStatusParams{
		ID: fee.ID, FromStatus: string(from), ToStatus: string(to), CollectionMechanism: c.mechanism,
		CollectionReference: c.reference, CollectionEvidence: c.evidenceRef, CollectedAt: c.collectedAt,
		NextCollectionAt: c.next, AttemptIncrement: c.attemptIncrement,
	})
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("fees: fee %s changed concurrently", fee.PublicID)
	}
	if noop {
		return nil
	}
	fromS := string(from)
	return q.InsertFeeTransition(ctx, store.InsertFeeTransitionParams{
		FeeID: fee.ID, FromStatus: &fromS, ToStatus: string(to), Actor: c.actor, Reason: nonEmpty(c.reason),
		Evidence: nonEmpty(c.evidence), RequestID: c.requestID,
	})
}

func (s *Service) lockByPublicID(ctx context.Context, q *store.Queries, feeID string) (store.AxeneFee, error) {
	fee, err := q.GetFeeByPublicID(ctx, feeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fee, ErrNotFound
	}
	if err != nil {
		return fee, err
	}
	return q.LockFee(ctx, fee.ID)
}

// CollectDue runs automatic collection for due fees. Each fee is claimed
// with a lease; the collector is called outside any transaction; results
// are recorded with the payment-style rules: unknown outcomes are retried,
// never failed or collected.
func (s *Service) CollectDue(ctx context.Context, max int32) (int, error) {
	now := s.now()
	due, err := s.q.ClaimDueFees(ctx, store.ClaimDueFeesParams{LeaseUntil: now.Add(s.policy.Lease), Now: now, MaxRows: max})
	if err != nil {
		return 0, fmt.Errorf("fees: claim due fees: %w", err)
	}
	for _, f := range due {
		if err := s.collect(ctx, f); err != nil {
			s.log.ErrorContext(ctx, "collect fee", "fee_id", f.PublicID, "err", err)
		}
	}
	return len(due), nil
}

func (s *Service) collect(ctx context.Context, f store.AxeneFee) error {
	requestID := fmt.Sprintf("collect-%s-%d", f.PublicID, f.CollectionAttempts+1)
	mech, err := s.mechanism(ctx, s.q, f.MerchantID, f.Environment)
	if err != nil {
		return err
	}
	collector, ok := s.collectors[mech]
	if mech == MechanismNone || !ok {
		// Mechanism removed or unavailable: stop scheduling; the fee stays an
		// uncollected receivable.
		return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			q := s.q.WithTx(tx)
			cur, err := q.LockFee(ctx, f.ID)
			if err != nil {
				return err
			}
			return s.transition(ctx, q, cur, Status(cur.Status), change{requestID: requestID, next: nil, attemptIncrement: 0})
		})
	}
	merchant, err := s.q.GetMerchantByID(ctx, f.MerchantID)
	if err != nil {
		return err
	}
	payment, err := s.q.GetPaymentByID(ctx, f.PaymentID)
	if err != nil {
		return err
	}
	// Move to collection_pending before calling out, so an interrupted call
	// is visible and retried rather than forgotten.
	if Status(f.Status) != StatusCollectionPending {
		if err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			q := s.q.WithTx(tx)
			cur, err := q.LockFee(ctx, f.ID)
			if err != nil {
				return err
			}
			m := string(mech)
			lease := s.now().Add(s.policy.Lease)
			return s.transition(ctx, q, cur, StatusCollectionPending, change{actor: "system:collection", reason: "collection started",
				mechanism: &m, next: &lease, requestID: requestID})
		}); err != nil {
			return err
		}
	}
	cur, _ := money.ParseCurrency(f.Currency)
	amt, _ := money.New(f.FeeMinor, cur)
	callCtx, cancel := context.WithTimeout(ctx, s.policy.CallTimeout)
	out, cerr := collector.Collect(callCtx, CollectRequest{FeeID: f.PublicID, PaymentID: payment.PublicID, MerchantID: merchant.PublicID, Amount: amt})
	cancel()
	if cerr != nil {
		out = CollectOutcome{Class: CollectUnknown, Message: cerr.Error()}
	}

	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		fee, err := q.LockFee(ctx, f.ID)
		if err != nil {
			return err
		}
		if Status(fee.Status) != StatusCollectionPending {
			return nil // resolved meanwhile (e.g. by reconciliation)
		}
		attempt := fee.CollectionAttempts + 1
		switch out.Class {
		case CollectSucceeded:
			if out.Reference == "" {
				return fmt.Errorf("fees: collector reported success without a reference")
			}
			if err := s.transition(ctx, q, fee, StatusCollectionPending, change{requestID: requestID, attemptIncrement: 1}); err != nil {
				return err
			}
			fee.CollectionAttempts = attempt
			return s.markCollected(ctx, q, fee, merchant.PublicID, string(mech), out.Reference,
				"collector:"+string(mech)+":"+out.Reference, "system:collection", requestID)
		case CollectFailed:
			next := s.now().Add(s.backoff(int(attempt)))
			return s.transition(ctx, q, fee, StatusCollectionFailed, change{actor: "system:collection", reason: out.Message,
				next: &next, attemptIncrement: 1, requestID: requestID})
		default:
			next := s.now().Add(s.backoff(int(attempt)))
			return s.transition(ctx, q, fee, StatusCollectionPending, change{requestID: requestID, next: &next, attemptIncrement: 1})
		}
	})
}

func (s *Service) backoff(attempt int) time.Duration {
	d := s.policy.RetryBase
	for i := 1; i < attempt && d < s.policy.RetryMax; i++ {
		d *= 2
	}
	return min(d, s.policy.RetryMax)
}

// ForPayment returns the fee for a payment, if one has accrued.
func (s *Service) ForPayment(ctx context.Context, paymentID pgtype.UUID) (store.AxeneFee, bool, error) {
	f, err := s.q.GetFeeByPaymentID(ctx, paymentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, false, nil
	}
	return f, err == nil, err
}

// Transitions returns a fee's history.
func (s *Service) Transitions(ctx context.Context, feeID pgtype.UUID) ([]store.FeeTransition, error) {
	return s.q.ListFeeTransitions(ctx, feeID)
}

// SimulatedCollector is a DEVELOPMENT collector: it moves no money and
// reports every fee collected with a deterministic reference. It exists so
// the collection lifecycle can be exercised end to end; NewService refuses
// it in production.
type SimulatedCollector struct {
	// Script, when set, decides the outcome per fee (tests).
	Script func(CollectRequest) CollectOutcome
}

func (SimulatedCollector) Mechanism() Mechanism { return MechanismSimulated }

func (c SimulatedCollector) Collect(_ context.Context, r CollectRequest) (CollectOutcome, error) {
	if c.Script != nil {
		return c.Script(r), nil
	}
	return CollectOutcome{Class: CollectSucceeded, Reference: "SIM-" + r.FeeID}, nil
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func pgCode(err error) string {
	var e *pgconn.PgError
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func merchantUUIDString(id pgtype.UUID) string {
	v, err := id.Value()
	if err != nil || v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
