//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/stan-ley-tech/axene-pay/internal/httpapi"
	"github.com/stan-ley-tech/axene-pay/internal/merchant"
	"github.com/stan-ley-tech/axene-pay/internal/payments"
	"github.com/stan-ley-tech/axene-pay/internal/platform/testdb"
	"github.com/stan-ley-tech/axene-pay/internal/providers"
	"github.com/stan-ley-tech/axene-pay/internal/providers/testprovider"
	"github.com/stan-ley-tech/axene-pay/internal/webhooks"
)

const (
	callbackToken  = "cbtoken_0123456789abcdefghijklmnopqrstuv"
	callbackSecret = "tp-callback-secret"
	base           = "http://localhost:8080"
)

// syncBuffer collects every log line the server writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type harness struct {
	t       *testing.T
	handler http.Handler
	router  routers.Router
	logs    *syncBuffer
	ms      *merchant.Service
	key     string // sandbox API key secret
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	pool := testdb.New(t)
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	reg := providers.NewRegistry(false)
	if err := reg.Register(testprovider.New(callbackSecret)); err != nil {
		t.Fatal(err)
	}
	ms, err := merchant.NewService(pool, []byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	ps, err := payments.NewService(payments.Deps{
		Pool: pool, Providers: reg, Policy: payments.DefaultPolicy(), Logger: logger,
		CallbackURL: func(p string) string { return base + "/callbacks/" + p + "/" + callbackToken },
	})
	if err != nil {
		t.Fatal(err)
	}

	sealer, err := webhooks.NewSealer(bytes.Repeat([]byte{9}, 32), 1)
	if err != nil {
		t.Fatal(err)
	}
	wh, err := webhooks.NewService(webhooks.Deps{Pool: pool, Sealer: sealer, Payments: ps, Policy: webhooks.DefaultPolicy(), Logger: logger})
	if err != nil {
		t.Fatal(err)
	}

	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../api-spec/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(ctx); err != nil {
		t.Fatalf("spec invalid: %v", err)
	}
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		t: t, router: router, logs: logs, ms: ms,
		handler: httpapi.NewRouter(httpapi.Deps{Logger: logger, DB: pool, Auth: ms, Payments: ps, CallbackToken: callbackToken, Webhooks: wh}),
	}
	h.key = h.merchantKey("testprovider")
	return h
}

func (h *harness) merchantKey(provider string) string {
	h.t.Helper()
	ctx := context.Background()
	m, err := h.ms.CreateMerchant(ctx, "M")
	if err != nil {
		h.t.Fatal(err)
	}
	d, err := h.ms.CreateDestination(ctx, m.PublicID, merchant.Sandbox, "mpesa_till", "555000", "")
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.ms.CreateProviderConfig(ctx, m.PublicID, merchant.Sandbox, "mpesa", provider, d.PublicID, 1); err != nil {
		h.t.Fatal(err)
	}
	k, err := h.ms.IssueAPIKey(ctx, m.PublicID, merchant.Sandbox)
	if err != nil {
		h.t.Fatal(err)
	}
	return k.Secret
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("response is not JSON: %s", r.body)
	}
	return m
}

func (r response) errorCode(t *testing.T) string {
	t.Helper()
	e, _ := r.json(t)["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// do sends a request and validates the response against the OpenAPI
// contract. validRequest=false skips request validation for negative tests.
func (h *harness) do(method, path, key string, headers map[string]string, body string, validRequest bool) response {
	h.t.Helper()
	req := httptest.NewRequest(method, base+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	res := response{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}

	if res.header.Get("X-Request-Id") == "" {
		h.t.Errorf("%s %s: missing X-Request-Id", method, path)
	}
	route, params, err := h.router.FindRoute(req)
	if err != nil {
		if res.status != http.StatusNotFound && res.status != http.StatusMethodNotAllowed {
			h.t.Fatalf("%s %s: no route in spec: %v", method, path, err)
		}
		return res
	}
	in := &openapi3filter.RequestValidationInput{
		Request: httptest.NewRequest(method, base+path, strings.NewReader(body)), PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	}
	in.Request.Header = req.Header.Clone()
	if validRequest {
		if err := openapi3filter.ValidateRequest(context.Background(), in); err != nil {
			h.t.Fatalf("%s %s: request does not match contract: %v", method, path, err)
		}
	}
	if err := openapi3filter.ValidateResponse(context.Background(), &openapi3filter.ResponseValidationInput{
		RequestValidationInput: in, Status: res.status, Header: res.header, Body: io.NopCloser(bytes.NewReader(res.body)),
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}); err != nil {
		h.t.Fatalf("%s %s -> %d: response does not match contract: %v\n%s", method, path, res.status, err, res.body)
	}
	return res
}

const validBody = `{"amount":10000,"currency":"KES","payment_method":{"type":"mpesa","phone_number":"254700000000"},"description":"Order 7","merchant_reference":"order-7"}`

func idem(k string) map[string]string { return map[string]string{"Idempotency-Key": k} }

func TestContractAndBehaviour(t *testing.T) {
	h := newHarness(t)

	t.Run("probes", func(t *testing.T) {
		if r := h.do("GET", "/healthz", "", nil, "", true); r.status != 200 {
			t.Fatalf("healthz %d", r.status)
		}
		if r := h.do("GET", "/readyz", "", nil, "", true); r.status != 200 {
			t.Fatalf("readyz %d", r.status)
		}
	})

	t.Run("authentication", func(t *testing.T) {
		if r := h.do("GET", "/v1/payments/pay_00000000000000000000000000", "", nil, "", false); r.status != 401 || r.errorCode(t) != "missing_api_key" {
			t.Fatalf("no key: %d %s", r.status, r.body)
		}
		if r := h.do("GET", "/v1/payments/pay_00000000000000000000000000", "sk_test_wrong", nil, "", false); r.status != 401 || r.errorCode(t) != "invalid_api_key" {
			t.Fatalf("bad key: %d %s", r.status, r.body)
		}
	})

	var paymentID string
	t.Run("create, replay, mismatch", func(t *testing.T) {
		r := h.do("POST", "/v1/payments", h.key, idem("order-7"), validBody, true)
		if r.status != 201 {
			t.Fatalf("create: %d %s", r.status, r.body)
		}
		p := r.json(t)
		paymentID = p["id"].(string)
		if p["status"] != "PENDING_PROVIDER" || p["amount"] != float64(10000) {
			t.Fatalf("created payment: %s", r.body)
		}
		replay := h.do("POST", "/v1/payments", h.key, idem("order-7"), validBody, true)
		if replay.status != 201 || replay.header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(replay.body, r.body) {
			t.Fatalf("replay: %d %v\n%s\nvs\n%s", replay.status, replay.header, replay.body, r.body)
		}
		other := strings.Replace(validBody, "Order 7", "Order 8", 1)
		if m := h.do("POST", "/v1/payments", h.key, idem("order-7"), other, true); m.status != 422 || m.errorCode(t) != "idempotency_key_reused" {
			t.Fatalf("mismatch: %d %s", m.status, m.body)
		}
	})

	t.Run("validation", func(t *testing.T) {
		cases := []struct {
			name, key, body, code string
		}{
			{"no idempotency key", "", validBody, "idempotency_key_required"},
			{"bad idempotency key", "has space", validBody, "idempotency_key_invalid"},
			{"malformed json", "v1", `{"amount":`, "invalid_json"},
			{"unknown field", "v2", strings.Replace(validBody, `"currency"`, `"surprise":1,"currency"`, 1), "invalid_json"},
			{"float amount", "v3", strings.Replace(validBody, "10000", "100.5", 1), "invalid_json"},
			{"trailing data", "v4", validBody + `{}`, "invalid_json"},
			{"missing amount", "v5", strings.Replace(validBody, `"amount":10000,`, "", 1), "parameter_invalid"},
			{"bad currency", "v6", strings.Replace(validBody, `"KES"`, `"kes"`, 1), "parameter_invalid"},
			{"bad phone", "v7", strings.Replace(validBody, "254700000000", "0700000000", 1), "parameter_invalid"},
			{"zero amount", "v8", strings.Replace(validBody, "10000", "0", 1), "parameter_invalid"},
		}
		for _, c := range cases {
			hdr := map[string]string{}
			if c.key != "" {
				hdr["Idempotency-Key"] = c.key
			}
			r := h.do("POST", "/v1/payments", h.key, hdr, c.body, false)
			if r.status != 400 || r.errorCode(t) != c.code {
				t.Errorf("%s: %d %s", c.name, r.status, r.body)
			}
		}
		fractional := strings.Replace(validBody, "10000", "10050", 1)
		if r := h.do("POST", "/v1/payments", h.key, idem("v9"), fractional, true); r.status != 422 || r.errorCode(t) != "no_eligible_provider" {
			t.Errorf("fractional shillings: %d %s", r.status, r.body)
		}
	})

	t.Run("get and isolation", func(t *testing.T) {
		if r := h.do("GET", "/v1/payments/"+paymentID, h.key, nil, "", true); r.status != 200 {
			t.Fatalf("get: %d %s", r.status, r.body)
		}
		otherKey := h.merchantKey("testprovider")
		if r := h.do("GET", "/v1/payments/"+paymentID, otherKey, nil, "", true); r.status != 404 || r.errorCode(t) != "payment_not_found" {
			t.Fatalf("other merchant: %d %s", r.status, r.body)
		}
		if r := h.do("GET", "/v1/payments/not-an-id", h.key, nil, "", false); r.status != 404 {
			t.Fatalf("malformed id: %d", r.status)
		}
	})

	t.Run("cancel after submission", func(t *testing.T) {
		r := h.do("POST", "/v1/payments/"+paymentID+"/cancel", h.key, nil, "", true)
		if r.status != 409 || r.errorCode(t) != "payment_not_cancellable" {
			t.Fatalf("cancel: %d %s", r.status, r.body)
		}
	})

	t.Run("callbacks", func(t *testing.T) {
		if r := h.do("POST", "/callbacks/testprovider/wrong-token", "", nil, `{}`, false); r.status != 404 {
			t.Fatalf("wrong token: %d", r.status)
		}
		cb := `{"notification_id":"n-1","reference":"tp_unknown","result":"succeeded"}`
		path := "/callbacks/testprovider/" + callbackToken
		if r := h.do("POST", path, "", map[string]string{testprovider.SecretHeader: "forged"}, cb, true); r.status != 400 || r.errorCode(t) != "invalid_callback" {
			t.Fatalf("forged: %d %s", r.status, r.body)
		}
		if r := h.do("POST", path, "", map[string]string{testprovider.SecretHeader: callbackSecret}, cb, true); r.status != 200 {
			t.Fatalf("unmatched but stored: %d %s", r.status, r.body)
		}
		if r := h.do("POST", "/callbacks/nope/"+callbackToken, "", nil, cb, true); r.status != 404 {
			t.Fatalf("unknown provider: %d", r.status)
		}
	})

	var webhookSecret string
	t.Run("webhook endpoints", func(t *testing.T) {
		r := h.do("POST", "/v1/webhook_endpoints", h.key, nil, `{"url":"https://merchant.example/hooks","event_types":["payment.succeeded"],"description":"prod"}`, true)
		if r.status != 201 {
			t.Fatalf("create: %d %s", r.status, r.body)
		}
		ep := r.json(t)
		webhookSecret, _ = ep["secret"].(string)
		id := ep["id"].(string)
		if !strings.HasPrefix(webhookSecret, "whsec_") {
			t.Fatalf("no secret in create response: %s", r.body)
		}
		list := h.do("GET", "/v1/webhook_endpoints", h.key, nil, "", true)
		if list.status != 200 || strings.Contains(string(list.body), "whsec_") {
			t.Fatalf("list: %d %s", list.status, list.body)
		}
		if g := h.do("GET", "/v1/webhook_endpoints/"+id, h.key, nil, "", true); g.status != 200 || strings.Contains(string(g.body), "whsec_") {
			t.Fatalf("get: %d %s", g.status, g.body)
		}
		if d := h.do("GET", "/v1/webhook_endpoints/"+id+"/deliveries?limit=5", h.key, nil, "", true); d.status != 200 {
			t.Fatalf("deliveries: %d %s", d.status, d.body)
		}
		if d := h.do("GET", "/v1/webhook_endpoints/"+id+"/deliveries?limit=500", h.key, nil, "", false); d.status != 400 {
			t.Fatalf("bad limit: %d", d.status)
		}
		otherKey := h.merchantKey("testprovider")
		if g := h.do("GET", "/v1/webhook_endpoints/"+id, otherKey, nil, "", true); g.status != 404 || g.errorCode(t) != "resource_not_found" {
			t.Fatalf("other merchant: %d %s", g.status, g.body)
		}
		if dis := h.do("POST", "/v1/webhook_endpoints/"+id+"/disable", h.key, nil, "", true); dis.status != 200 || dis.json(t)["status"] != "disabled" {
			t.Fatalf("disable: %d %s", dis.status, dis.body)
		}
		for _, bad := range []string{
			`{"url":"https://127.0.0.1/hooks"}`,
			`{"url":"not a url"}`,
			`{"url":"https://merchant.example/h","event_types":["payment.exploded"]}`,
		} {
			if b := h.do("POST", "/v1/webhook_endpoints", h.key, nil, bad, false); b.status != 400 {
				t.Errorf("%s: %d %s", bad, b.status, b.body)
			}
		}
		if g := h.do("GET", "/v1/webhook_deliveries/whd_00000000000000000000000000", h.key, nil, "", true); g.status != 404 {
			t.Fatalf("unknown delivery: %d", g.status)
		}
		if g := h.do("POST", "/v1/webhook_deliveries/whd_00000000000000000000000000/replay", h.key, nil, "", true); g.status != 404 {
			t.Fatalf("replay unknown: %d", g.status)
		}
	})

	t.Run("unknown route and method", func(t *testing.T) {
		if r := h.do("GET", "/v1/nothing", h.key, nil, "", false); r.status != 404 || r.errorCode(t) != "route_not_found" {
			t.Fatalf("unknown route: %d %s", r.status, r.body)
		}
		if r := h.do("DELETE", "/v1/payments/"+paymentID, h.key, nil, "", false); r.status != 405 {
			t.Fatalf("wrong method: %d", r.status)
		}
	})

	t.Run("no secrets or personal data in logs", func(t *testing.T) {
		time.Sleep(10 * time.Millisecond)
		logs := h.logs.String()
		for name, canary := range map[string]string{
			"API key":         h.key,
			"callback token":  callbackToken,
			"phone number":    "254700000000",
			"callback secret": callbackSecret,
			"webhook secret":  webhookSecret,
		} {
			if strings.Contains(logs, canary) {
				t.Errorf("%s appears in logs", name)
			}
		}
		if !strings.Contains(logs, `"route":"/v1/payments"`) {
			t.Error("access log lines missing")
		}
	})
}
