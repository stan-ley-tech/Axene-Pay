package httpx

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRequestIDGeneratedWhenAbsentOrInvalid(t *testing.T) {
	t.Parallel()
	for _, inbound := range []string{"", "has space", "bad\nnewline", strings.Repeat("a", 129)} {
		var seen string
		h := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = RequestID(r.Context())
		}))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if inbound != "" {
			req.Header.Set(RequestIDHeader, inbound)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if !strings.HasPrefix(seen, "req_") || len(seen) != 36 {
			t.Errorf("inbound %q: generated id %q", inbound, seen)
		}
		if rec.Header().Get(RequestIDHeader) != seen {
			t.Errorf("inbound %q: response header %q != context %q", inbound, rec.Header().Get(RequestIDHeader), seen)
		}
	}
}

func TestRequestIDAcceptsValidInbound(t *testing.T) {
	t.Parallel()
	var seen string
	h := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, "client-abc_123.x")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen != "client-abc_123.x" || rec.Header().Get(RequestIDHeader) != seen {
		t.Fatalf("seen %q header %q", seen, rec.Header().Get(RequestIDHeader))
	}
}

func TestRecovererWritesEnvelope(t *testing.T) {
	t.Parallel()
	h := RequestIDMiddleware(Recoverer(discard())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	var body struct{ Error APIError }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != ErrTypeAPI || body.Error.Code != "internal_error" ||
		body.Error.RequestID != rec.Header().Get(RequestIDHeader) {
		t.Fatalf("body %+v", body.Error)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Fatal("panic value leaked to client")
	}
}

func TestAccessLogLogsRoutePatternNotRawPath(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	r := chi.NewRouter()
	r.Use(AccessLog(logger))
	r.Get("/callbacks/{token}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/callbacks/s3cr3t-token?api_key=sk_live_secret", nil))

	out := buf.String()
	if strings.Contains(out, "sk_live_secret") || strings.Contains(out, "s3cr3t-token") {
		t.Fatalf("secret logged: %s", out)
	}
	if !strings.Contains(out, `"status":418`) || !strings.Contains(out, `"route":"/callbacks/{token}"`) {
		t.Fatalf("unexpected log: %s", out)
	}
}
