// Package apiview renders domain objects in their public JSON form, shared
// by the HTTP API and webhook payloads so both always agree with the
// OpenAPI contract.
package apiview

import (
	"time"

	"github.com/stan-ley-tech/axene-pay/internal/payments"
)

// PaymentJSON is the public JSON representation of a payment.
type PaymentJSON struct {
	ID            string  `json:"id"`
	Object        string  `json:"object"`
	Environment   string  `json:"environment"`
	Status        string  `json:"status"`
	StatusReason  *string `json:"status_reason"`
	Amount        int64   `json:"amount"`
	Currency      string  `json:"currency"`
	PaymentMethod struct {
		Type        string `json:"type"`
		PhoneNumber string `json:"phone_number"`
	} `json:"payment_method"`
	Description           string  `json:"description"`
	MerchantReference     *string `json:"merchant_reference"`
	SettlementDestination struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Identifier string `json:"identifier"`
	} `json:"settlement_destination"`
	Attempts  []AttemptJSON `json:"attempts"`
	AxeneFee  *FeeJSON      `json:"axene_fee"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
}

// FeeJSON is Axene's fee for a payment. Status "accrued" means calculated
// and owed but not yet collected; "collected" requires collection evidence.
type FeeJSON struct {
	ID       string `json:"id"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	FeePayer string `json:"fee_payer"`
	Status   string `json:"status"`
}

// AttemptJSON is the public JSON representation of a payment attempt.
type AttemptJSON struct {
	ID          string     `json:"id"`
	Provider    string     `json:"provider"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	SubmittedAt *time.Time `json:"submitted_at"`
	ResolvedAt  *time.Time `json:"resolved_at"`
}

// Payment renders v. It is the single definition of the payment object
// shared by API responses and webhook payloads.
func Payment(v payments.View) PaymentJSON {
	out := PaymentJSON{
		ID: v.ID, Object: "payment", Environment: v.Environment, Status: string(v.Status), StatusReason: v.StatusReason,
		Amount: v.Amount, Currency: v.Currency, Description: v.Description, MerchantReference: v.MerchantReference,
		Attempts: make([]AttemptJSON, 0, len(v.Attempts)), CreatedAt: v.CreatedAt.UTC(), UpdatedAt: v.UpdatedAt.UTC(),
	}
	out.PaymentMethod.Type, out.PaymentMethod.PhoneNumber = v.Method, v.PhoneNumberMasked
	out.SettlementDestination.ID, out.SettlementDestination.Type, out.SettlementDestination.Identifier = v.Destination.ID, v.Destination.Type, v.Destination.Identifier
	for _, a := range v.Attempts {
		out.Attempts = append(out.Attempts, AttemptJSON{ID: a.ID, Provider: a.Provider, Status: string(a.Status),
			CreatedAt: a.CreatedAt.UTC(), SubmittedAt: utc(a.SubmittedAt), ResolvedAt: utc(a.ResolvedAt)})
	}
	if v.Fee != nil {
		out.AxeneFee = &FeeJSON{ID: v.Fee.ID, Amount: v.Fee.Amount, Currency: v.Fee.Currency, FeePayer: v.Fee.FeePayer, Status: v.Fee.Status}
	}
	return out
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
