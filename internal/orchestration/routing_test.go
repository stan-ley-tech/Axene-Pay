package orchestration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
	"github.com/stan-ley-tech/axene-pay/internal/providers"
	"github.com/stan-ley-tech/axene-pay/internal/providers/testprovider"
)

// stub is a provider whose capabilities and limits are set per test.
type stub struct {
	name string
	caps providers.CapabilitySet
	lim  providers.Limits
}

func (s stub) Name() string                          { return s.name }
func (s stub) Simulated() bool                       { return true }
func (s stub) Capabilities() providers.CapabilitySet { return s.caps }
func (s stub) Limits() providers.Limits              { return s.lim }
func (s stub) Submit(context.Context, providers.SubmitRequest) (providers.SubmitOutcome, error) {
	return providers.SubmitOutcome{}, nil
}
func (s stub) QueryStatus(context.Context, providers.AttemptRef) (providers.StatusOutcome, error) {
	return providers.StatusOutcome{}, nil
}
func (s stub) ParseCallback(context.Context, providers.CallbackRequest) (providers.CallbackEvent, error) {
	return providers.CallbackEvent{}, nil
}

func registry(t *testing.T, ps ...providers.Provider) *providers.Registry {
	t.Helper()
	r := providers.NewRegistry(false)
	for _, p := range ps {
		if err := r.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func kes(t *testing.T, minor int64) money.Amount {
	t.Helper()
	a, err := money.New(minor, money.MustCurrency("KES"))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func cand(id, provider string, priority int32) Candidate {
	return Candidate{ConfigID: id, Provider: provider, Priority: priority, Enabled: true, DestinationActive: true,
		DestinationID: "dst_x", Destination: providers.Destination{Type: "mpesa_till", Identifier: "1"}}
}

func TestRouteSelectsFirstEligibleAndExplains(t *testing.T) {
	t.Parallel()
	noCollect := stub{name: "nocollect", caps: providers.CapabilitySet{providers.CapStatusQuery},
		lim: providers.Limits{Methods: []providers.Method{providers.MethodMpesa}, Currencies: []string{"KES"}}}
	usdOnly := stub{name: "usdonly", caps: providers.CapabilitySet{providers.CapCollect},
		lim: providers.Limits{Methods: []providers.Method{providers.MethodMpesa}, Currencies: []string{"USD"}}}
	reg := registry(t, testprovider.New("s"), noCollect, usdOnly)

	disabled := cand("pcf_1", "testprovider", 1)
	disabled.Enabled = false
	inactive := cand("pcf_2", "testprovider", 2)
	inactive.DestinationActive = false
	candidates := []Candidate{
		disabled, inactive,
		cand("pcf_3", "missing", 3),
		cand("pcf_4", "nocollect", 4),
		cand("pcf_5", "usdonly", 5),
		cand("pcf_6", "testprovider", 6),
		cand("pcf_7", "testprovider", 7),
	}
	d, err := Route(Request{Method: providers.MethodMpesa, Amount: kes(t, 10000)}, candidates, reg)
	if err != nil {
		t.Fatal(err)
	}
	if d.Candidate.ConfigID != "pcf_6" || d.Provider.Name() != "testprovider" {
		t.Fatalf("selected %s", d.Candidate.ConfigID)
	}
	for _, want := range []string{
		"selected testprovider via pcf_6 (priority 6)",
		"pcf_1 (testprovider): configuration disabled",
		"pcf_2 (testprovider): settlement destination disabled",
		"pcf_3 (missing): provider not available",
		"pcf_4 (nocollect): provider cannot collect",
		"pcf_5 (usdonly): provider does not support mpesa in KES",
	} {
		if !strings.Contains(d.Reason, want) {
			t.Errorf("reason missing %q:\n%s", want, d.Reason)
		}
	}
	if strings.Contains(d.Reason, "pcf_7") {
		t.Errorf("reason mentions candidates after the selected one: %s", d.Reason)
	}
}

func TestRouteIsDeterministic(t *testing.T) {
	t.Parallel()
	reg := registry(t, testprovider.New("s"))
	cs := []Candidate{cand("pcf_a", "testprovider", 1), cand("pcf_b", "testprovider", 1)}
	for i := 0; i < 50; i++ {
		d, err := Route(Request{Method: providers.MethodMpesa, Amount: kes(t, 100)}, cs, reg)
		if err != nil || d.Candidate.ConfigID != "pcf_a" {
			t.Fatalf("iteration %d: %v %v", i, d.Candidate.ConfigID, err)
		}
	}
}

func TestRouteNoEligibleProvider(t *testing.T) {
	t.Parallel()
	reg := registry(t, testprovider.New("s"))
	_, err := Route(Request{Method: providers.MethodMpesa, Amount: kes(t, 100)}, nil, reg)
	if !errors.Is(err, ErrNoEligibleProvider) || !strings.Contains(err.Error(), "no provider configured") {
		t.Fatalf("empty: %v", err)
	}
	// testprovider only accepts whole shillings.
	_, err = Route(Request{Method: providers.MethodMpesa, Amount: kes(t, 1050)}, []Candidate{cand("pcf_1", "testprovider", 1)}, reg)
	if !errors.Is(err, ErrNoEligibleProvider) || !strings.Contains(err.Error(), "whole currency units") {
		t.Fatalf("fractional: %v", err)
	}
	usd, _ := money.New(100, money.MustCurrency("USD"))
	_, err = Route(Request{Method: providers.MethodMpesa, Amount: usd}, []Candidate{cand("pcf_1", "testprovider", 1)}, reg)
	if !errors.Is(err, ErrNoEligibleProvider) {
		t.Fatalf("usd: %v", err)
	}
}
