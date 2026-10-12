package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/stan-ley-tech/axene-pay/internal/apiview"
	"github.com/stan-ley-tech/axene-pay/internal/platform/httpx"
	"github.com/stan-ley-tech/axene-pay/internal/webhooks"
)

type webhookHandlers struct {
	svc *webhooks.Service
	log *slog.Logger
}

type listJSON[T any] struct {
	Object string `json:"object"`
	Data   []T    `json:"data"`
}

func (h webhookHandlers) createEndpoint(w http.ResponseWriter, r *http.Request) {
	pr, _ := principal(r.Context())
	var req struct {
		URL         string   `json:"url"`
		EventTypes  []string `json:"event_types"`
		Description string   `json:"description"`
	}
	if !decode(w, r, &req) {
		return
	}
	created, err := h.svc.CreateEndpoint(r.Context(), pr, req.URL, req.EventTypes, req.Description)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := apiview.WebhookEndpoint(created.Endpoint)
	out.Secret = created.Secret
	httpx.WriteJSON(w, r, http.StatusCreated, out)
}

func (h webhookHandlers) listEndpoints(w http.ResponseWriter, r *http.Request) {
	pr, _ := principal(r.Context())
	eps, err := h.svc.ListEndpoints(r.Context(), pr)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := listJSON[apiview.WebhookEndpointJSON]{Object: "list", Data: make([]apiview.WebhookEndpointJSON, 0, len(eps))}
	for _, e := range eps {
		out.Data = append(out.Data, apiview.WebhookEndpoint(e))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

func (h webhookHandlers) getEndpoint(w http.ResponseWriter, r *http.Request) {
	pr, _ := principal(r.Context())
	ep, err := h.svc.GetEndpoint(r.Context(), pr, chi.URLParam(r, "endpointID"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, apiview.WebhookEndpoint(ep))
}

func (h webhookHandlers) disableEndpoint(w http.ResponseWriter, r *http.Request) {
	pr, _ := principal(r.Context())
	ep, err := h.svc.DisableEndpoint(r.Context(), pr, chi.URLParam(r, "endpointID"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, apiview.WebhookEndpoint(ep))
}

func (h webhookHandlers) listDeliveries(w http.ResponseWriter, r *http.Request) {
	pr, _ := principal(r.Context())
	limit := int32(20)
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			invalid(w, r, "parameter_invalid", "limit must be an integer from 1 to 100.", "limit")
			return
		}
		limit = int32(n)
	}
	endpointID := chi.URLParam(r, "endpointID")
	ds, err := h.svc.ListDeliveries(r.Context(), pr, endpointID, limit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := listJSON[apiview.WebhookDeliveryJSON]{Object: "list", Data: make([]apiview.WebhookDeliveryJSON, 0, len(ds))}
	for _, d := range ds {
		out.Data = append(out.Data, apiview.WebhookDelivery(d, endpointID, nil))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

func (h webhookHandlers) getDelivery(w http.ResponseWriter, r *http.Request) {
	pr, _ := principal(r.Context())
	d, err := h.svc.GetDelivery(r.Context(), pr, chi.URLParam(r, "deliveryID"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, apiview.WebhookDelivery(d.Delivery, d.EndpointPublicID, d.Attempts))
}

func (h webhookHandlers) replayDelivery(w http.ResponseWriter, r *http.Request) {
	pr, _ := principal(r.Context())
	d, err := h.svc.Replay(r.Context(), pr, chi.URLParam(r, "deliveryID"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, apiview.WebhookDelivery(d.Delivery, d.EndpointPublicID, d.Attempts))
}

func (h webhookHandlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, webhooks.ErrInvalid):
		invalid(w, r, "parameter_invalid", err.Error(), "")
	case errors.Is(err, webhooks.ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, httpx.APIError{Type: httpx.ErrTypeNotFound, Code: "resource_not_found", Message: "No such webhook endpoint or delivery."})
	case errors.Is(err, webhooks.ErrNotReplayable):
		httpx.WriteError(w, r, http.StatusConflict, httpx.APIError{Type: httpx.ErrTypeConflict, Code: "delivery_pending",
			Message: "The delivery is still pending and will be attempted automatically."})
	default:
		h.log.ErrorContext(r.Context(), "webhook request failed", "request_id", httpx.RequestID(r.Context()), "err", err)
		internalError(w, r)
	}
}
