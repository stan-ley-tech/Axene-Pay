package payments

import (
	"context"
	"time"

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
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// DestinationView is where the provider settles the funds (ADR-0008).
type DestinationView struct {
	ID         string
	Type       string
	Identifier string
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
