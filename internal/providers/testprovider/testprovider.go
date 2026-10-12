// Package testprovider is a SIMULATED payment provider for development and
// automated tests. It moves no money, talks to no network, and is refused
// by the provider registry in production.
//
// Behaviour is scripted per payer phone number. The defaults below let a
// developer exercise every lifecycle path through the real API:
//
//	254700000000  accepted; status query reports success
//	254700000001  accepted; status query reports declined
//	254700000002  submission times out (outcome unknown); status query reports success
//	254700000003  rejected synchronously
//	254700000004  accepted; status query is always inconclusive
//	254700000005  accepted; status query reports success with a different amount
//	254700000006  accepted; status query reports still pending forever
//	any other     accepted; status query reports success
//
// Callbacks are JSON posted by hand (or by tests); see CallbackBody.
package testprovider

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
	"github.com/stan-ley-tech/axene-pay/internal/providers"
)

// Name is the provider name used in configuration and records.
const Name = "testprovider"

// SecretHeader carries the shared callback secret.
const SecretHeader = "X-Testprovider-Secret"

// SubmitBehavior scripts the response to Submit.
type SubmitBehavior int

const (
	SubmitAccept SubmitBehavior = iota
	SubmitReject
	SubmitTimeout
)

// StatusBehavior scripts one response to QueryStatus.
type StatusBehavior int

const (
	StatusSucceeded StatusBehavior = iota
	StatusDeclined
	StatusPending
	StatusInconclusive
	StatusAmountMismatch
)

// Script is the scripted behaviour for one payer. Status responses are
// consumed in order; the last one repeats.
type Script struct {
	Submit SubmitBehavior
	Status []StatusBehavior
}

var defaultScripts = map[string]Script{
	"254700000000": {SubmitAccept, []StatusBehavior{StatusSucceeded}},
	"254700000001": {SubmitAccept, []StatusBehavior{StatusDeclined}},
	"254700000002": {SubmitTimeout, []StatusBehavior{StatusSucceeded}},
	"254700000003": {SubmitReject, nil},
	"254700000004": {SubmitAccept, []StatusBehavior{StatusInconclusive}},
	"254700000005": {SubmitAccept, []StatusBehavior{StatusAmountMismatch}},
	"254700000006": {SubmitAccept, []StatusBehavior{StatusPending}},
}

var fallbackScript = Script{SubmitAccept, []StatusBehavior{StatusSucceeded}}

type record struct {
	ref     string
	amount  money.Amount
	script  Script
	queries int
}

// Provider is the simulated provider. Safe for concurrent use.
type Provider struct {
	secret string

	mu        sync.Mutex
	scripts   map[string]Script
	byAttempt map[string]*record
	submits   int
}

// New returns a simulated provider that accepts callbacks carrying secret.
func New(secret string) *Provider {
	scripts := make(map[string]Script, len(defaultScripts))
	for k, v := range defaultScripts {
		scripts[k] = v
	}
	return &Provider{secret: secret, scripts: scripts, byAttempt: map[string]*record{}}
}

// SetScript overrides the behaviour for a payer phone number.
func (p *Provider) SetScript(phone string, s Script) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scripts[phone] = s
}

// SubmitCount returns how many times Submit reached the simulated provider,
// so tests can assert that uncertain attempts are never resubmitted.
func (p *Provider) SubmitCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.submits
}

func (p *Provider) Name() string    { return Name }
func (p *Provider) Simulated() bool { return true }

func (p *Provider) Capabilities() providers.CapabilitySet {
	return providers.CapabilitySet{providers.CapCollect, providers.CapStatusQuery}
}

func (p *Provider) Limits() providers.Limits {
	return providers.Limits{
		Methods:        []providers.Method{providers.MethodMpesa},
		Currencies:     []string{"KES"},
		WholeUnitsOnly: true,
	}
}

func (p *Provider) Submit(_ context.Context, req providers.SubmitRequest) (providers.SubmitOutcome, error) {
	if _, err := req.Amount.WholeUnits(); err != nil {
		return providers.SubmitOutcome{Class: providers.ClassRejected, RawCode: "AMOUNT", RawMessage: "whole units only"}, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.submits++

	if rec, ok := p.byAttempt[req.AttemptID]; ok { // provider-side idempotency
		return providers.SubmitOutcome{Class: providers.ClassAccepted, ProviderReference: rec.ref, RawCode: "0", RawMessage: "duplicate accepted"}, nil
	}
	script, ok := p.scripts[req.PhoneNumber]
	if !ok {
		script = fallbackScript
	}
	switch script.Submit {
	case SubmitReject:
		return providers.SubmitOutcome{Class: providers.ClassRejected, RawCode: "REJECT", RawMessage: "simulated rejection"}, nil
	case SubmitTimeout:
		// The simulated provider did receive it; the caller just cannot know.
		p.byAttempt[req.AttemptID] = &record{ref: newRef(), amount: req.Amount, script: script}
		return providers.SubmitOutcome{Class: providers.ClassUnknown, RawMessage: "simulated timeout"}, nil
	default:
		rec := &record{ref: newRef(), amount: req.Amount, script: script}
		p.byAttempt[req.AttemptID] = rec
		return providers.SubmitOutcome{Class: providers.ClassAccepted, ProviderReference: rec.ref, RawCode: "0", RawMessage: "accepted"}, nil
	}
}

func (p *Provider) QueryStatus(_ context.Context, ref providers.AttemptRef) (providers.StatusOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, ok := p.byAttempt[ref.AttemptID]
	if !ok {
		return providers.StatusOutcome{Class: providers.ClassUnknown, RawCode: "NOT_FOUND"}, nil
	}
	b := StatusSucceeded
	if n := len(rec.script.Status); n > 0 {
		b = rec.script.Status[min(rec.queries, n-1)]
	}
	rec.queries++
	switch b {
	case StatusDeclined:
		return providers.StatusOutcome{Class: providers.ClassDeclined, RawCode: "1032", RawMessage: "simulated: cancelled by user"}, nil
	case StatusPending:
		return providers.StatusOutcome{Class: providers.ClassAccepted, RawCode: "PENDING"}, nil
	case StatusInconclusive:
		return providers.StatusOutcome{Class: providers.ClassUnknown, RawCode: "TIMEOUT"}, nil
	case StatusAmountMismatch:
		other, _ := money.New(rec.amount.Minor()+100, rec.amount.Currency())
		return providers.StatusOutcome{Class: providers.ClassSucceeded, Amount: &other, ProviderReceipt: "SIM" + rec.ref[3:13], RawCode: "0"}, nil
	default:
		amt := rec.amount
		return providers.StatusOutcome{Class: providers.ClassSucceeded, Amount: &amt, ProviderReceipt: "SIM" + rec.ref[3:13], RawCode: "0"}, nil
	}
}

// CallbackBody is the JSON accepted by ParseCallback.
type CallbackBody struct {
	NotificationID string `json:"notification_id"`
	Reference      string `json:"reference"`
	Result         string `json:"result"` // "succeeded" or "declined"
	AmountMinor    *int64 `json:"amount_minor,omitempty"`
	Currency       string `json:"currency,omitempty"`
}

func (p *Provider) ParseCallback(_ context.Context, req providers.CallbackRequest) (providers.CallbackEvent, error) {
	got := req.Header.Get(SecretHeader)
	if p.secret == "" || subtle.ConstantTimeCompare([]byte(got), []byte(p.secret)) != 1 {
		return providers.CallbackEvent{}, fmt.Errorf("%w: bad secret", providers.ErrCallbackRejected)
	}
	var body CallbackBody
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return providers.CallbackEvent{}, fmt.Errorf("%w: %v", providers.ErrCallbackRejected, err)
	}
	if body.NotificationID == "" || body.Reference == "" {
		return providers.CallbackEvent{}, fmt.Errorf("%w: missing notification_id or reference", providers.ErrCallbackRejected)
	}
	ev := providers.CallbackEvent{
		ProviderReference: body.Reference,
		DedupKey:          body.NotificationID,
		RawCode:           body.Result,
	}
	switch body.Result {
	case "succeeded":
		ev.Claimed = providers.ClassSucceeded
	case "declined":
		ev.Claimed = providers.ClassDeclined
	default:
		ev.Claimed = providers.ClassUnknown
	}
	if body.AmountMinor != nil {
		c, err := money.ParseCurrency(body.Currency)
		if err != nil {
			return providers.CallbackEvent{}, fmt.Errorf("%w: %v", providers.ErrCallbackRejected, err)
		}
		a, _ := money.New(*body.AmountMinor, c)
		ev.Amount = &a
	}
	return ev, nil
}

func newRef() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "tp_" + hex.EncodeToString(b[:])
}
