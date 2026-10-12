package idempotency

import (
	"strings"
	"testing"

	"github.com/stan-ley-tech/axene-pay/internal/store"
)

func TestValidateValue(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"a", "order-123_retry.1", strings.Repeat("x", 255), "~!@#$%^&*()"} {
		if err := ValidateValue(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", strings.Repeat("x", 256), "has space", "tab\t", "ünïcode", "new\nline"} {
		if err := ValidateValue(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()
	k := Key{Method: "POST", Path: "/v1/payments", Fingerprint: Fingerprint([]byte("body"))}
	base := store.IdempotencyKey{RequestMethod: "POST", RequestPath: "/v1/payments", RequestFingerprint: k.Fingerprint[:], Status: "in_progress"}

	if r := classify(k, base); r.State != StateInProgress {
		t.Errorf("in progress: %v", r.State)
	}
	done := base
	done.Status = "completed"
	if r := classify(k, done); r.State != StateCompleted {
		t.Errorf("completed: %v", r.State)
	}
	for name, mutate := range map[string]func(*store.IdempotencyKey){
		"method": func(r *store.IdempotencyKey) { r.RequestMethod = "PUT" },
		"path":   func(r *store.IdempotencyKey) { r.RequestPath = "/v1/other" },
		"body":   func(r *store.IdempotencyKey) { f := Fingerprint([]byte("other")); r.RequestFingerprint = f[:] },
	} {
		rec := done
		mutate(&rec)
		if r := classify(k, rec); r.State != StateMismatch {
			t.Errorf("%s change: %v, want mismatch", name, r.State)
		}
	}
}
