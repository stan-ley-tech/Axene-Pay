package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stan-ley-tech/axene-pay/internal/platform/httpx"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func newTestRouter(db Pinger) http.Handler {
	return NewRouter(Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: db})
}

func TestHealthzIgnoresDatabase(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	newTestRouter(fakeDB{err: errors.New("down")}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get(httpx.RequestIDHeader) == "" {
		t.Fatal("missing request id header")
	}
}

func TestReadyz(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		dbErr    error
		wantCode int
	}{
		{"db up", nil, http.StatusOK},
		{"db down", errors.New("connection refused"), http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			newTestRouter(fakeDB{err: tt.dbErr}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if rec.Code != tt.wantCode {
				t.Fatalf("status %d, want %d", rec.Code, tt.wantCode)
			}
			if strings.Contains(rec.Body.String(), "connection refused") {
				t.Fatal("internal error detail leaked to client")
			}
		})
	}
}

func TestUnknownRouteUsesErrorEnvelope(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	newTestRouter(fakeDB{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
	var body struct{ Error httpx.APIError }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "route_not_found" || body.Error.RequestID == "" ||
		body.Error.RequestID != rec.Header().Get(httpx.RequestIDHeader) {
		t.Fatalf("body %+v", body.Error)
	}
}

func TestWrongMethodUsesErrorEnvelope(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	newTestRouter(fakeDB{}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Body.String(), "method_not_allowed") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}
