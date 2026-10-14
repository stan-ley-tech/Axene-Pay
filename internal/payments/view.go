package payments

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// View is the merchant-facing representation of a payment.
type View struct {
	ID                string
	Environment       string
	Status            Status
	StatusReason      *string
	Amount            int64
	Currency          string
	Method            string
	PhoneNumberMasked string
	Description       string
	MerchantReference *string
	Destination       DestinationView
	Attempts          []AttemptView
	// Fee is Axene's fee once accrued (successful payments only).
	Fee       *FeeView
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DestinationView is where the provider settles the funds (ADR-0008).
type DestinationView struct {
	ID         string
	Type       string
	Identifier string
}

// FeeView is Axene's fee for a payment and its collection status.
type FeeView struct {
	ID       string
	Amount   int64
	Currency string
	FeePayer string
	Status   string
}

// AttemptView summarizes one provider attempt.
type AttemptView struct {
	ID          string
	Provider    string
	Status      AttemptStatus
	CreatedAt   time.Time
	SubmittedAt *time.Time
	ResolvedAt  *time.Time
}

func (s *Service) view(ctx context.Context, paymentID pgtype.UUID) (View, error) {
	p, err := s.q.GetPaymentByID(ctx, paymentID)
	if err != nil {
		return View{}, err
	}
	d, err := s.q.GetSettlementDestinationByID(ctx, p.SettlementDestinationID)
	if err != nil {
		return View{}, err
	}
	attempts, err := s.q.ListAttemptsForPayment(ctx, p.ID)
	if err != nil {
		return View{}, err
	}
	v := View{
		ID: p.PublicID, Environment: p.Environment, Status: Status(p.Status), StatusReason: p.StatusReason,
		Amount: p.AmountMinor, Currency: p.Currency, Method: p.Method, PhoneNumberMasked: MaskPhone(p.PhoneNumber),
		Description: p.Description, MerchantReference: p.MerchantReference,
		Destination: DestinationView{ID: d.PublicID, Type: d.Type, Identifier: d.Identifier},
		Attempts:    make([]AttemptView, 0, len(attempts)),
		CreatedAt:   p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
	if f, err := s.q.GetFeeByPaymentID(ctx, p.ID); err == nil {
		v.Fee = &FeeView{ID: f.PublicID, Amount: f.FeeMinor, Currency: f.Currency, FeePayer: f.FeePayer, Status: f.Status}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return View{}, err
	}
	for _, a := range attempts {
		v.Attempts = append(v.Attempts, AttemptView{
			ID: a.PublicID, Provider: a.Provider, Status: AttemptStatus(a.Status),
			CreatedAt: a.CreatedAt, SubmittedAt: a.SubmittedAt, ResolvedAt: a.ResolvedAt,
		})
	}
	return v, nil
}

// MaskPhone hides the middle of an MSISDN for display and logs:
// 254712345678 -> 25471****678.
func MaskPhone(p string) string {
	if len(p) < 9 {
		return "****"
	}
	return p[:5] + "****" + p[len(p)-3:]
}

// Owner identifies who a payment belongs to.
type Owner struct {
	MerchantID  pgtype.UUID
	Environment string
}

// ViewForEvent returns a payment and its owner by public ID without merchant
// scoping. It exists for internal consumers of payment events (webhook
// fan-out); merchant-facing code must use Get.
func (s *Service) ViewForEvent(ctx context.Context, paymentID string) (Owner, View, error) {
	p, err := s.q.GetPaymentByPublicID(ctx, paymentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Owner{}, View{}, ErrNotFound
	}
	if err != nil {
		return Owner{}, View{}, err
	}
	v, err := s.view(ctx, p.ID)
	return Owner{MerchantID: p.MerchantID, Environment: p.Environment}, v, err
}
