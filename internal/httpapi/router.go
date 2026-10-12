// Package httpapi assembles the public HTTP surface of the api process.
// Domain handlers are mounted here as modules are implemented.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stan-ley-tech/axene-pay/internal/payments"
	"github.com/stan-ley-tech/axene-pay/internal/platform/httpx"
	"github.com/stan-ley-tech/axene-pay/internal/webhooks"
)

// Pinger is satisfied by *pgxpool.Pool.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps are the dependencies the router needs. Payments, Auth and
// CallbackToken are optional so that probes can be tested alone; when
// Payments is nil the payment and callback routes are not mounted.
type Deps struct {
	Logger        *slog.Logger
	DB            Pinger
	Auth          Authenticator
	Payments      *payments.Service
	CallbackToken string
	// Webhooks, when set, mounts webhook endpoint management.
	Webhooks *webhooks.Service
	// HTTPMetrics, when set, records request metrics by route pattern.
	HTTPMetrics func(http.Handler) http.Handler
}

// readinessTimeout bounds how long /readyz waits on dependencies so that a
// hung database makes the instance unready instead of hanging the probe.
const readinessTimeout = 2 * time.Second

// maxBodyBytes caps request bodies. Payment requests are small; large bodies
// are rejected before any handler runs.
const maxBodyBytes = 1 << 20

// NewRouter returns the api process's root handler.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(
		httpx.RequestIDMiddleware,
		httpx.AccessLog(d.Logger),
		httpx.Recoverer(d.Logger),
		limitBody,
	)
	if d.HTTPMetrics != nil {
		r.Use(d.HTTPMetrics)
	}
	r.NotFound(httpx.NotFound)
	r.MethodNotAllowed(httpx.MethodNotAllowed)

	r.Get("/healthz", healthz)
	r.Get("/readyz", readyz(d.DB, d.Logger))

	if d.Payments != nil {
		ph := paymentHandlers{svc: d.Payments, log: d.Logger}
		r.Route("/v1", func(r chi.Router) {
			r.Use(requireAPIKey(d.Auth, d.Logger))
			r.Post("/payments", ph.create)
			r.Get("/payments/{paymentID}", ph.get)
			r.Post("/payments/{paymentID}/cancel", ph.cancel)
			if d.Webhooks != nil {
				wh := webhookHandlers{svc: d.Webhooks, log: d.Logger}
				r.Post("/webhook_endpoints", wh.createEndpoint)
				r.Get("/webhook_endpoints", wh.listEndpoints)
				r.Get("/webhook_endpoints/{endpointID}", wh.getEndpoint)
				r.Post("/webhook_endpoints/{endpointID}/disable", wh.disableEndpoint)
				r.Get("/webhook_endpoints/{endpointID}/deliveries", wh.listDeliveries)
				r.Get("/webhook_deliveries/{deliveryID}", wh.getDelivery)
				r.Post("/webhook_deliveries/{deliveryID}/replay", wh.replayDelivery)
			}
		})
		r.Method(http.MethodPost, "/callbacks/{provider}/{token}", callbackHandler{svc: d.Payments, token: d.CallbackToken, log: d.Logger})
	}
	return r
}

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

type healthResponse struct {
	Status string `json:"status"`
}

// healthz reports liveness only. It must never depend on downstream
// services, or a database outage would cause the platform to restart every
// instance.
func healthz(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, r, http.StatusOK, healthResponse{Status: "ok"})
}

func readyz(db Pinger, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			logger.WarnContext(r.Context(), "readiness check failed",
				"request_id", httpx.RequestID(r.Context()), "dependency", "postgres", "err", err)
			httpx.WriteError(w, r, http.StatusServiceUnavailable, httpx.APIError{
				Type: httpx.ErrTypeAPI, Code: "not_ready", Message: "Service is not ready.",
			})
			return
		}
		httpx.WriteJSON(w, r, http.StatusOK, healthResponse{Status: "ready"})
	}
}
