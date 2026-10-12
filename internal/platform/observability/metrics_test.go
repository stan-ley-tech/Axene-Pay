package observability

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/stan-ley-tech/axene-pay/internal/payments"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

func TestMetricsRecordDomainEvents(t *testing.T) {
	t.Parallel()
	m := New()
	m.Transition(payments.StatusPendingVerification, payments.StatusSucceeded, payments.EvidenceStatusQuery)
	m.ProviderCall("testprovider", "submit", "accepted", 120*time.Millisecond)
	m.ProviderCall("testprovider", "submit", "", time.Millisecond)
	m.Verification("testprovider", "retry")
	m.Callback("testprovider", "duplicate")
	m.ReconException("evidence_conflict")
	m.SetOutboxBacklog(7, 3*time.Second)

	out := scrape(t, m)
	for _, want := range []string{
		`axene_payment_transitions_total{evidence="status_query",from="PENDING_VERIFICATION",to="SUCCEEDED"} 1`,
		`axene_provider_calls_total{class="accepted",operation="submit",provider="testprovider"} 1`,
		`axene_provider_calls_total{class="none",operation="submit",provider="testprovider"} 1`,
		`axene_verifications_total{provider="testprovider",result="retry"} 1`,
		`axene_provider_callbacks_total{provider="testprovider",result="duplicate"} 1`,
		`axene_reconciliation_exceptions_total{type="evidence_conflict"} 1`,
		`axene_outbox_unpublished_events 7`,
		`axene_outbox_oldest_unpublished_age_seconds 3`,
		`go_goroutines`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestHTTPMiddlewareUsesRoutePatterns(t *testing.T) {
	t.Parallel()
	m := New()
	r := chi.NewRouter()
	r.Use(m.HTTPMiddleware)
	r.Get("/v1/payments/{id}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	for _, id := range []string{"pay_A", "pay_B", "pay_C"} {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/payments/"+id, nil))
	}
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nope", nil))

	out := scrape(t, m)
	if !strings.Contains(out, `axene_http_requests_total{method="GET",route="/v1/payments/{id}",status="404"} 3`) {
		t.Errorf("route pattern not aggregated:\n%s", out)
	}
	if !strings.Contains(out, `route="unmatched"`) {
		t.Error("unmatched route not recorded")
	}
	if strings.Contains(out, "pay_A") {
		t.Error("resource ID used as a label")
	}
}
