// Package observability exposes Prometheus metrics for the api and worker.
//
// Label values are limited to low-cardinality, non-sensitive values: route
// patterns, status codes, provider names, operation names, outcome classes,
// payment states, and evidence kinds. Payment IDs, merchant IDs, phone
// numbers, and amounts are never labels.
package observability

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/stan-ley-tech/axene-pay/internal/payments"
)

// Metrics holds every collector. It implements payments.Metrics.
type Metrics struct {
	reg *prometheus.Registry

	httpRequests     *prometheus.CounterVec
	httpDuration     *prometheus.HistogramVec
	transitions      *prometheus.CounterVec
	providerCalls    *prometheus.CounterVec
	providerDuration *prometheus.HistogramVec
	verifications    *prometheus.CounterVec
	callbacks        *prometheus.CounterVec
	reconExceptions  *prometheus.CounterVec
	outboxPending    prometheus.Gauge
	outboxOldest     prometheus.Gauge
}

var _ payments.Metrics = (*Metrics)(nil)

// New creates and registers all collectors, including Go runtime and
// process metrics.
func New() *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "axene_http_requests_total", Help: "HTTP requests by route pattern, method and status code.",
		}, []string{"route", "method", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "axene_http_request_duration_seconds", Help: "HTTP request latency by route pattern.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"route", "method"}),
		transitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "axene_payment_transitions_total", Help: "Payment state transitions.",
		}, []string{"from", "to", "evidence"}),
		providerCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "axene_provider_calls_total", Help: "Provider calls by operation and normalized outcome class.",
		}, []string{"provider", "operation", "class"}),
		providerDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "axene_provider_call_duration_seconds", Help: "Provider call latency.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60},
		}, []string{"provider", "operation"}),
		verifications: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "axene_verifications_total", Help: "Verification attempts by result (retry or resulting payment status).",
		}, []string{"provider", "result"}),
		callbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "axene_provider_callbacks_total", Help: "Provider callbacks by ingestion result.",
		}, []string{"provider", "result"}),
		reconExceptions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "axene_reconciliation_exceptions_total", Help: "Reconciliation exceptions opened, by type.",
		}, []string{"type"}),
		outboxPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "axene_outbox_unpublished_events", Help: "Outbox events not yet published to the broker.",
		}),
		outboxOldest: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "axene_outbox_oldest_unpublished_age_seconds", Help: "Age of the oldest unpublished outbox event.",
		}),
	}
	m.reg.MustRegister(
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.httpRequests, m.httpDuration, m.transitions, m.providerCalls, m.providerDuration,
		m.verifications, m.callbacks, m.reconExceptions, m.outboxPending, m.outboxOldest,
	)
	return m
}

// Handler serves the metrics in Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Transition implements payments.Metrics.
func (m *Metrics) Transition(from, to payments.Status, ev payments.Evidence) {
	m.transitions.WithLabelValues(string(from), string(to), string(ev)).Inc()
}

// ProviderCall implements payments.Metrics.
func (m *Metrics) ProviderCall(provider, operation, class string, d time.Duration) {
	if class == "" {
		class = "none"
	}
	m.providerCalls.WithLabelValues(provider, operation, class).Inc()
	m.providerDuration.WithLabelValues(provider, operation).Observe(d.Seconds())
}

// Verification implements payments.Metrics.
func (m *Metrics) Verification(provider, result string) {
	m.verifications.WithLabelValues(provider, result).Inc()
}

// Callback implements payments.Metrics.
func (m *Metrics) Callback(provider, result string) {
	m.callbacks.WithLabelValues(provider, result).Inc()
}

// ReconException implements payments.Metrics.
func (m *Metrics) ReconException(kind string) {
	m.reconExceptions.WithLabelValues(kind).Inc()
}

// SetOutboxBacklog records the outbox backlog.
func (m *Metrics) SetOutboxBacklog(n int64, oldest time.Duration) {
	m.outboxPending.Set(float64(n))
	m.outboxOldest.Set(oldest.Seconds())
}

// HTTPMiddleware records request counts and latency by route pattern. It
// must run inside the chi router so the pattern is known.
func (m *Metrics) HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		route := "unmatched"
		if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
			route = rc.RoutePattern()
		}
		m.httpRequests.WithLabelValues(route, r.Method, strconv.Itoa(rec.status)).Inc()
		m.httpDuration.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wrote {
		s.status, s.wrote = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wrote {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(b)
}

// Serve runs the metrics endpoint on its own listener (not the public API
// port) until ctx is cancelled.
func Serve(ctx context.Context, addr string, m *Metrics, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		logger.Info("metrics listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server", "err", err)
		}
	}()
}
