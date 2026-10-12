package providers_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
	"github.com/stan-ley-tech/axene-pay/internal/providers"
	"github.com/stan-ley-tech/axene-pay/internal/providers/testprovider"
)

func TestRegistryRefusesSimulatedProviderInProduction(t *testing.T) {
	t.Parallel()
	r := providers.NewRegistry(true)
	if err := r.Register(testprovider.New("s")); !errors.Is(err, providers.ErrSimulatedInProduction) {
		t.Fatalf("err = %v, want ErrSimulatedInProduction", err)
	}
	if _, err := r.Get(testprovider.Name); !errors.Is(err, providers.ErrUnknownProvider) {
		t.Fatalf("refused provider is retrievable: %v", err)
	}
}

func TestRegistryRegisterAndGet(t *testing.T) {
	t.Parallel()
	r := providers.NewRegistry(false)
	tp := testprovider.New("s")
	if err := r.Register(tp); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(testprovider.New("s")); err == nil {
		t.Fatal("duplicate registration accepted")
	}
	got, err := r.Get(testprovider.Name)
	if err != nil || got != tp {
		t.Fatalf("Get = %v, %v", got, err)
	}
	if !reflect.DeepEqual(r.Names(), []string{testprovider.Name}) {
		t.Fatalf("Names = %v", r.Names())
	}
	if _, err := r.Get("nope"); !errors.Is(err, providers.ErrUnknownProvider) {
		t.Fatalf("unknown err = %v", err)
	}
}

func TestCapabilitiesAndLimits(t *testing.T) {
	t.Parallel()
	tp := testprovider.New("s")
	caps := tp.Capabilities()
	if !caps.Has(providers.CapCollect) || !caps.Has(providers.CapStatusQuery) || caps.Has(providers.CapPayout) {
		t.Fatalf("caps = %v", caps)
	}
	kes, usd := money.MustCurrency("KES"), money.MustCurrency("USD")
	if !tp.Limits().Supports(providers.MethodMpesa, kes) {
		t.Error("should support mpesa/KES")
	}
	if tp.Limits().Supports(providers.MethodMpesa, usd) || tp.Limits().Supports("card", kes) {
		t.Error("should not support USD or card")
	}
}
