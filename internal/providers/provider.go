// Package providers defines the boundary between Axene's payment domain and
// external payment providers (docs/provider-integration.md).
//
// Provider wire formats, authentication, and error codes stay inside each
// adapter sub-package. The domain sees only the types in this file.
package providers

import (
	"context"
	"errors"
	"net/http"

	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
)

// Capability is an operation a provider adapter actually supports.
type Capability string

const (
	CapCollect       Capability = "collect"
	CapStatusQuery   Capability = "status_query"
	CapRefundFull    Capability = "refund_full"
	CapRefundPartial Capability = "refund_partial"
	CapReversal      Capability = "reversal"
	CapPayout        Capability = "payout"
	CapCancel        Capability = "cancel"
)

// CapabilitySet is the set of capabilities a provider declares.
type CapabilitySet []Capability

// Has reports whether c is declared.
func (s CapabilitySet) Has(c Capability) bool {
	for _, v := range s {
		if v == c {
			return true
		}
	}
	return false
}

// Method is a payment method a provider can collect with.
type Method string

// MethodMpesa is an M-Pesa customer payment (M-Pesa Express prompt).
const MethodMpesa Method = "mpesa"

// Limits are static constraints declared by a provider adapter.
type Limits struct {
	Methods    []Method
	Currencies []string // ISO 4217 codes
	// WholeUnitsOnly means amounts with a fractional major unit are rejected.
	WholeUnitsOnly bool
}

// Supports reports whether the limits allow method m in currency c.
func (l Limits) Supports(m Method, c money.Currency) bool {
	return contains(l.Methods, m) && contains(l.Currencies, c.Code())
}

func contains[T comparable](xs []T, x T) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Class normalizes every provider response (docs/provider-integration.md §4).
type Class string

const (
	// ClassAccepted: the provider took the request; outcome still pending.
	ClassAccepted Class = "accepted"
	// ClassSucceeded: the provider reports completion.
	ClassSucceeded Class = "succeeded"
	// ClassDeclined: customer/issuer declined, cancelled, or timed out. Not
	// eligible for failover.
	ClassDeclined Class = "declined"
	// ClassRejected: refused before execution (validation, limits, config).
	ClassRejected Class = "rejected"
	// ClassUnknown: the outcome cannot be determined (timeout, transport
	// error, unparseable response). Never treated as failure.
	ClassUnknown Class = "unknown"
)

// Destination is where the provider settles the funds (ADR-0008).
type Destination struct {
	Type       string // e.g. "mpesa_paybill", "mpesa_till"
	Identifier string // e.g. shortcode
}

// SubmitRequest asks a provider to collect a payment.
type SubmitRequest struct {
	// AttemptID is Axene's attempt identifier; adapters pass it to the
	// provider as the idempotency/correlation reference where supported.
	AttemptID   string
	Amount      money.Amount
	Method      Method
	PhoneNumber string // MSISDN for mobile money methods
	Destination Destination
	Description string
	CallbackURL string
}

// SubmitOutcome is the result of Submit.
type SubmitOutcome struct {
	Class             Class
	ProviderReference string // provider's identifier for the request, if any
	RawCode           string
	RawMessage        string
}

// AttemptRef identifies a submitted attempt to the provider.
type AttemptRef struct {
	AttemptID         string
	ProviderReference string
	Destination       Destination
}

// StatusOutcome is the result of an authenticated status query.
type StatusOutcome struct {
	// Class is ClassAccepted while the provider still considers the request
	// in progress, ClassUnknown if the query itself was inconclusive.
	Class Class
	// Amount is the amount the provider reports, when it reports one.
	Amount          *money.Amount
	ProviderReceipt string
	RawCode         string
	RawMessage      string
}

// CallbackRequest is an inbound provider callback as received over HTTP.
type CallbackRequest struct {
	Header http.Header
	Body   []byte
}

// CallbackEvent is a parsed callback. It is an unverified claim.
type CallbackEvent struct {
	ProviderReference string
	// DedupKey uniquely identifies this notification for duplicate detection.
	DedupKey        string
	Claimed         Class
	Amount          *money.Amount
	ProviderReceipt string
	RawCode         string
	RawMessage      string
}

// ErrCallbackRejected means the callback could not be parsed or
// authenticated. It is still persisted by the caller for investigation.
var ErrCallbackRejected = errors.New("providers: callback rejected")

// Provider is implemented by every adapter.
type Provider interface {
	// Name is the stable identifier used in configuration and records.
	Name() string
	// Simulated reports whether this adapter is a test double. Simulated
	// providers are refused in production.
	Simulated() bool
	Capabilities() CapabilitySet
	Limits() Limits

	// Submit sends a collection request. When the adapter cannot tell whether
	// the provider received the request it returns ClassUnknown and a nil
	// error. A non-nil error means nothing was sent (programming or
	// configuration fault).
	Submit(ctx context.Context, req SubmitRequest) (SubmitOutcome, error)

	// QueryStatus is only called when CapStatusQuery is declared. Transport
	// failures are reported as ClassUnknown, not as errors.
	QueryStatus(ctx context.Context, ref AttemptRef) (StatusOutcome, error)

	// ParseCallback parses an inbound callback, returning ErrCallbackRejected
	// (wrapped) when it is malformed or fails authentication.
	ParseCallback(ctx context.Context, req CallbackRequest) (CallbackEvent, error)
}
