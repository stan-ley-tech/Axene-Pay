package testprovider

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
	"github.com/stan-ley-tech/axene-pay/internal/providers"
)

var ctx = context.Background()

func kes(t *testing.T, minor int64) money.Amount {
	t.Helper()
	a, err := money.New(minor, money.MustCurrency("KES"))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func submit(t *testing.T, p *Provider, attempt, phone string, minor int64) providers.SubmitOutcome {
	t.Helper()
	out, err := p.Submit(ctx, providers.SubmitRequest{AttemptID: attempt, Amount: kes(t, minor), Method: providers.MethodMpesa, PhoneNumber: phone})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDefaultScripts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		phone      string
		wantSubmit providers.Class
		wantStatus providers.Class
	}{
		{"254700000000", providers.ClassAccepted, providers.ClassSucceeded},
		{"254700000001", providers.ClassAccepted, providers.ClassDeclined},
		{"254700000002", providers.ClassUnknown, providers.ClassSucceeded},
		{"254700000003", providers.ClassRejected, providers.ClassUnknown},
		{"254700000004", providers.ClassAccepted, providers.ClassUnknown},
		{"254700000005", providers.ClassAccepted, providers.ClassSucceeded},
		{"254700000006", providers.ClassAccepted, providers.ClassAccepted},
		{"254711111111", providers.ClassAccepted, providers.ClassSucceeded},
	}
	for _, tt := range tests {
		p := New("s")
		out := submit(t, p, "att_1", tt.phone, 10000)
		if out.Class != tt.wantSubmit {
			t.Errorf("%s submit = %s, want %s", tt.phone, out.Class, tt.wantSubmit)
		}
		st, err := p.QueryStatus(ctx, providers.AttemptRef{AttemptID: "att_1", ProviderReference: out.ProviderReference})
		if err != nil {
			t.Fatal(err)
		}
		if st.Class != tt.wantStatus {
			t.Errorf("%s status = %s, want %s", tt.phone, st.Class, tt.wantStatus)
		}
		if tt.phone == "254700000005" && (st.Amount == nil || st.Amount.Minor() == 10000) {
			t.Errorf("mismatch script returned matching amount %v", st.Amount)
		}
		if tt.phone == "254700000000" && (st.Amount == nil || st.Amount.Minor() != 10000 || st.ProviderReceipt == "") {
			t.Errorf("success status missing amount/receipt: %+v", st)
		}
	}
}

func TestScriptSequenceAndRepeat(t *testing.T) {
	t.Parallel()
	p := New("s")
	p.SetScript("254799999999", Script{SubmitAccept, []StatusBehavior{StatusInconclusive, StatusPending, StatusSucceeded}})
	submit(t, p, "att_seq", "254799999999", 500)
	want := []providers.Class{providers.ClassUnknown, providers.ClassAccepted, providers.ClassSucceeded, providers.ClassSucceeded}
	for i, w := range want {
		st, _ := p.QueryStatus(ctx, providers.AttemptRef{AttemptID: "att_seq"})
		if st.Class != w {
			t.Errorf("query %d = %s, want %s", i, st.Class, w)
		}
	}
}

func TestSubmitIsIdempotentPerAttempt(t *testing.T) {
	t.Parallel()
	p := New("s")
	a := submit(t, p, "att_dup", "254711111111", 100)
	b := submit(t, p, "att_dup", "254711111111", 100)
	if a.ProviderReference == "" || a.ProviderReference != b.ProviderReference {
		t.Fatalf("refs differ: %q vs %q", a.ProviderReference, b.ProviderReference)
	}
	if p.SubmitCount() != 2 {
		t.Fatalf("SubmitCount = %d", p.SubmitCount())
	}
}

func TestSubmitRejectsFractionalShillings(t *testing.T) {
	t.Parallel()
	if out := submit(t, New("s"), "att_frac", "254711111111", 1050); out.Class != providers.ClassRejected {
		t.Fatalf("class = %s", out.Class)
	}
}

func TestQueryUnknownAttempt(t *testing.T) {
	t.Parallel()
	st, err := New("s").QueryStatus(ctx, providers.AttemptRef{AttemptID: "nope"})
	if err != nil || st.Class != providers.ClassUnknown {
		t.Fatalf("got %+v, %v", st, err)
	}
}

func TestParseCallback(t *testing.T) {
	t.Parallel()
	p := New("topsecret")
	good := http.Header{SecretHeader: {"topsecret"}}
	tests := []struct {
		name    string
		header  http.Header
		body    string
		wantErr bool
		want    providers.Class
	}{
		{"success", good, `{"notification_id":"n1","reference":"tp_1","result":"succeeded","amount_minor":1000,"currency":"KES"}`, false, providers.ClassSucceeded},
		{"declined", good, `{"notification_id":"n2","reference":"tp_1","result":"declined"}`, false, providers.ClassDeclined},
		{"odd result", good, `{"notification_id":"n3","reference":"tp_1","result":"weird"}`, false, providers.ClassUnknown},
		{"bad secret", http.Header{SecretHeader: {"nope"}}, `{"notification_id":"n","reference":"r","result":"succeeded"}`, true, ""},
		{"no secret", http.Header{}, `{"notification_id":"n","reference":"r","result":"succeeded"}`, true, ""},
		{"bad json", good, `{`, true, ""},
		{"missing ids", good, `{"result":"succeeded"}`, true, ""},
		{"bad currency", good, `{"notification_id":"n","reference":"r","result":"succeeded","amount_minor":1,"currency":"XXX"}`, true, ""},
	}
	for _, tt := range tests {
		ev, err := p.ParseCallback(ctx, providers.CallbackRequest{Header: tt.header, Body: []byte(tt.body)})
		if tt.wantErr {
			if !errors.Is(err, providers.ErrCallbackRejected) {
				t.Errorf("%s: err = %v, want ErrCallbackRejected", tt.name, err)
			}
			continue
		}
		if err != nil || ev.Claimed != tt.want {
			t.Errorf("%s: got %+v, %v", tt.name, ev, err)
		}
	}
	ev, _ := p.ParseCallback(ctx, providers.CallbackRequest{Header: good, Body: []byte(`{"notification_id":"n1","reference":"tp_1","result":"succeeded","amount_minor":1000,"currency":"KES"}`)})
	if ev.DedupKey != "n1" || ev.ProviderReference != "tp_1" || ev.Amount == nil || ev.Amount.Minor() != 1000 {
		t.Errorf("fields: %+v", ev)
	}
}

func TestEmptySecretRejectsEverything(t *testing.T) {
	t.Parallel()
	_, err := New("").ParseCallback(ctx, providers.CallbackRequest{Header: http.Header{SecretHeader: {""}}, Body: []byte(`{"notification_id":"n","reference":"r"}`)})
	if !errors.Is(err, providers.ErrCallbackRejected) {
		t.Fatalf("err = %v", err)
	}
}
