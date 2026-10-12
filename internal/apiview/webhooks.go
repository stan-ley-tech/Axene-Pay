package apiview

import (
	"time"

	"github.com/stan-ley-tech/axene-pay/internal/store"
)

// WebhookEndpointJSON is the public representation of a webhook endpoint.
// Secret is only set in the response that creates the endpoint.
type WebhookEndpointJSON struct {
	ID          string    `json:"id"`
	Object      string    `json:"object"`
	URL         string    `json:"url"`
	EventTypes  []string  `json:"event_types"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	Secret      string    `json:"secret,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// WebhookEndpoint renders an endpoint without its secret.
func WebhookEndpoint(e store.WebhookEndpoint) WebhookEndpointJSON {
	types := e.EventTypes
	if types == nil {
		types = []string{}
	}
	return WebhookEndpointJSON{
		ID: e.PublicID, Object: "webhook_endpoint", URL: e.Url, EventTypes: types,
		Description: e.Description, Status: e.Status, CreatedAt: e.CreatedAt.UTC(),
	}
}

// WebhookDeliveryJSON is the public representation of a delivery.
type WebhookDeliveryJSON struct {
	ID             string                       `json:"id"`
	Object         string                       `json:"object"`
	EndpointID     string                       `json:"endpoint_id"`
	EventID        string                       `json:"event_id"`
	EventType      string                       `json:"event_type"`
	Status         string                       `json:"status"`
	Attempts       int32                        `json:"attempts"`
	NextAttemptAt  *time.Time                   `json:"next_attempt_at"`
	LastStatusCode *int32                       `json:"last_status_code"`
	LastError      *string                      `json:"last_error"`
	DeliveredAt    *time.Time                   `json:"delivered_at"`
	CreatedAt      time.Time                    `json:"created_at"`
	AttemptHistory []WebhookDeliveryAttemptJSON `json:"attempt_history,omitempty"`
}

// WebhookDeliveryAttemptJSON is one HTTP attempt.
type WebhookDeliveryAttemptJSON struct {
	AttemptedAt time.Time `json:"attempted_at"`
	StatusCode  *int32    `json:"status_code"`
	Error       *string   `json:"error"`
	DurationMs  int32     `json:"duration_ms"`
}

// WebhookDelivery renders a delivery; attempts are included when non-nil.
func WebhookDelivery(d store.WebhookDelivery, endpointID string, attempts []store.WebhookDeliveryAttempt) WebhookDeliveryJSON {
	out := WebhookDeliveryJSON{
		ID: d.PublicID, Object: "webhook_delivery", EndpointID: endpointID, EventID: d.EventPublicID, EventType: d.EventType,
		Status: d.Status, Attempts: d.Attempts, NextAttemptAt: utc(d.NextAttemptAt), LastStatusCode: d.LastStatusCode,
		LastError: d.LastError, DeliveredAt: utc(d.DeliveredAt), CreatedAt: d.CreatedAt.UTC(),
	}
	if attempts != nil {
		out.AttemptHistory = make([]WebhookDeliveryAttemptJSON, 0, len(attempts))
		for _, a := range attempts {
			out.AttemptHistory = append(out.AttemptHistory, WebhookDeliveryAttemptJSON{
				AttemptedAt: a.AttemptedAt.UTC(), StatusCode: a.StatusCode, Error: a.Error, DurationMs: a.DurationMs,
			})
		}
	}
	return out
}
