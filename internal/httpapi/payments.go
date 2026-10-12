package httpapi

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stan-ley-tech/axene-pay/internal/idempotency"
	"github.com/stan-ley-tech/axene-pay/internal/payments"
	"github.com/stan-ley-tech/axene-pay/internal/platform/httpx"
	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
	"github.com/stan-ley-tech/axene-pay/internal/providers"
)

type paymentHandlers struct {
	svc *payments.Service
	log *slog.Logger
}

type createPaymentRequest struct {
	Amount        *int64 `json:"amount"`
	Currency      string `json:"currency"`
	PaymentMethod *struct {
		Type        string `json:"type"`
		PhoneNumber string `json:"phone_number"`
	} `json:"payment_method"`
	Description       string `json:"description"`
	MerchantReference string `json:"merchant_reference"`
}

func (h paymentHandlers) create(w http.ResponseWriter, r *http.Request) {
	pr, _ := principal(r.Context())
	keyValue := r.Header.Get("Idempotency-Key")
	if keyValue == "" {
		invalid(w, r, "idempotency_key_required", "Idempotency-Key header is required for this request.", "Idempotency-Key")
		return
	}
	if err := idempotency.ValidateValue(keyValue); err != nil {
		invalid(w, r, "idempotency_key_invalid", err.Error(), "Idempotency-Key")
		return
	}
	var req createPaymentRequest
	if !decode(w, r, &req) {
		return
	}
	params, param, err := req.params()
	if err != nil {
		invalid(w, r, "parameter_invalid", err.Error(), param)
		return
	}

	res, err := h.svc.Create(r.Context(), pr, idempotency.Key{Value: keyValue, Method: r.Method, Path: r.URL.Path}, params, httpx.RequestID(r.Context()))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if res.Replay != nil {
		w.Header().Set("Idempotent-Replayed", "true")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(int(*res.Replay.ResponseStatus))
		_, _ = w.Write(res.Replay.ResponseBody)
		return
	}
	body, err := json.Marshal(render(*res.Payment))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	body = append(body, '\n')
	if err := h.svc.CompleteIdempotency(r.Context(), res.Key, http.StatusCreated, body); err != nil {
		// The payment exists; a retry will be answered from its current state
		// once the recovery window passes.
		h.log.ErrorContext(r.Context(), "complete idempotency key", "request_id", httpx.RequestID(r.Context()), "err", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(body)
}

func (req createPaymentRequest) params() (payments.CreateParams, string, error) {
	if req.Amount == nil {
		return payments.CreateParams{}, "amount", errors.New("amount is required (integer, minor units)")
	}
	cur, err := money.ParseCurrency(req.Currency)
	if err != nil {
		return payments.CreateParams{}, "currency", errors.New("currency must be a supported upper-case ISO 4217 code")
	}
	amount, err := money.New(*req.Amount, cur)
	if err != nil {
		return payments.CreateParams{}, "amount", err
	}
	if req.PaymentMethod == nil {
		return payments.CreateParams{}, "payment_method", errors.New("payment_method is required")
	}
	p := payments.CreateParams{
		Amount: amount, Method: providers.Method(req.PaymentMethod.Type), PhoneNumber: req.PaymentMethod.PhoneNumber,
		Description: req.Description, MerchantReference: req.MerchantReference,
	}
	if err := p.Validate(); err != nil {
		return p, "", err
	}
	return p, "", nil
}

func (h paymentHandlers) get(w http.ResponseWriter, r *http.Request) {
	pr, _ := principal(r.Context())
	v, err := h.svc.Get(r.Context(), pr, chi.URLParam(r, "paymentID"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, render(v))
}

func (h paymentHandlers) cancel(w http.ResponseWriter, r *http.Request) {
	pr, _ := principal(r.Context())
	v, err := h.svc.Cancel(r.Context(), pr, chi.URLParam(r, "paymentID"), httpx.RequestID(r.Context()))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, render(v))
}

func (h paymentHandlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, payments.ErrInvalid):
		invalid(w, r, "parameter_invalid", err.Error(), "")
	case errors.Is(err, payments.ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, httpx.APIError{Type: httpx.ErrTypeNotFound, Code: "payment_not_found", Message: "No such payment."})
	case errors.Is(err, payments.ErrNotCancellable):
		httpx.WriteError(w, r, http.StatusConflict, httpx.APIError{Type: httpx.ErrTypeConflict, Code: "payment_not_cancellable",
			Message: "The payment has already been sent to the provider and cannot be cancelled."})
	case errors.Is(err, payments.ErrIdempotencyInProgress):
		httpx.WriteError(w, r, http.StatusConflict, httpx.APIError{Type: httpx.ErrTypeConflict, Code: "idempotency_in_progress",
			Message: "A request with this Idempotency-Key is still being processed. Retry later."})
	case errors.Is(err, payments.ErrIdempotencyMismatch):
		httpx.WriteError(w, r, http.StatusUnprocessableEntity, httpx.APIError{Type: httpx.ErrTypeInvalidRequest, Code: "idempotency_key_reused",
			Message: "This Idempotency-Key was used with a different request.", Param: "Idempotency-Key"})
	case errors.Is(err, payments.ErrNoEligibleProvider):
		h.log.WarnContext(r.Context(), "no eligible provider", "request_id", httpx.RequestID(r.Context()), "detail", err)
		httpx.WriteError(w, r, http.StatusUnprocessableEntity, httpx.APIError{Type: httpx.ErrTypeInvalidRequest, Code: "no_eligible_provider",
			Message: "No provider configured for this merchant can process this payment method, currency, and amount."})
	default:
		h.log.ErrorContext(r.Context(), "payment request failed", "request_id", httpx.RequestID(r.Context()), "err", err)
		internalError(w, r)
	}
}

func invalid(w http.ResponseWriter, r *http.Request, code, msg, param string) {
	httpx.WriteError(w, r, http.StatusBadRequest, httpx.APIError{Type: httpx.ErrTypeInvalidRequest, Code: code, Message: msg, Param: param})
}

// decode strictly parses a JSON body: unknown fields, trailing data, and
// non-integer amounts are rejected.
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, httpx.APIError{Type: httpx.ErrTypeInvalidRequest, Code: "body_too_large", Message: "Request body is too large."})
			return false
		}
		invalid(w, r, "invalid_json", "Request body must be a JSON object matching the API schema: "+err.Error(), "")
		return false
	}
	if dec.More() {
		invalid(w, r, "invalid_json", "Request body must contain a single JSON object.", "")
		return false
	}
	return true
}

type paymentJSON struct {
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
	Attempts  []attemptJSON `json:"attempts"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
}

type attemptJSON struct {
	ID          string     `json:"id"`
	Provider    string     `json:"provider"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	SubmittedAt *time.Time `json:"submitted_at"`
	ResolvedAt  *time.Time `json:"resolved_at"`
}

func render(v payments.View) paymentJSON {
	out := paymentJSON{
		ID: v.ID, Object: "payment", Environment: v.Environment, Status: string(v.Status), StatusReason: v.StatusReason,
		Amount: v.Amount, Currency: v.Currency, Description: v.Description, MerchantReference: v.MerchantReference,
		Attempts: make([]attemptJSON, 0, len(v.Attempts)), CreatedAt: v.CreatedAt.UTC(), UpdatedAt: v.UpdatedAt.UTC(),
	}
	out.PaymentMethod.Type, out.PaymentMethod.PhoneNumber = v.Method, v.PhoneNumberMasked
	out.SettlementDestination.ID, out.SettlementDestination.Type, out.SettlementDestination.Identifier = v.Destination.ID, v.Destination.Type, v.Destination.Identifier
	for _, a := range v.Attempts {
		out.Attempts = append(out.Attempts, attemptJSON{ID: a.ID, Provider: a.Provider, Status: string(a.Status),
			CreatedAt: a.CreatedAt.UTC(), SubmittedAt: utc(a.SubmittedAt), ResolvedAt: utc(a.ResolvedAt)})
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

// callbackHandler receives provider callbacks at /callbacks/{provider}/{token}.
type callbackHandler struct {
	svc   *payments.Service
	token string
	log   *slog.Logger
}

// maxCallbackBytes bounds callback bodies; provider notifications are small.
const maxCallbackBytes = 64 << 10

func (h callbackHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A wrong token is indistinguishable from an unknown route.
	if subtle.ConstantTimeCompare([]byte(chi.URLParam(r, "token")), []byte(h.token)) != 1 {
		httpx.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCallbackBytes+1))
	if err != nil || len(body) > maxCallbackBytes {
		invalid(w, r, "invalid_callback", "Callback body is unreadable or too large.", "")
		return
	}
	provider := chi.URLParam(r, "provider")
	err = h.svc.IngestCallback(r.Context(), provider, providers.CallbackRequest{Header: r.Header.Clone(), Body: bytes.Clone(body)}, httpx.RequestID(r.Context()))
	switch {
	case err == nil:
		httpx.WriteJSON(w, r, http.StatusOK, map[string]bool{"received": true})
	case errors.Is(err, payments.ErrUnknownProvider):
		httpx.NotFound(w, r)
	case errors.Is(err, providers.ErrCallbackRejected):
		invalid(w, r, "invalid_callback", "Callback could not be parsed or authenticated.", "")
	default:
		// Non-2xx lets the provider retry; the status query covers a lost callback.
		h.log.ErrorContext(r.Context(), "ingest callback", "request_id", httpx.RequestID(r.Context()), "provider", provider, "err", err)
		internalError(w, r)
	}
}
