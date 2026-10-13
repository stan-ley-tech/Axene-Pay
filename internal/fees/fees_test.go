package fees

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
)

// All rates below are TEST FIXTURES chosen to exercise arithmetic. They are
// not Axene pricing.

func ptr(v int64) *int64 { return &v }

func kes(minor int64) money.Amount {
	a, _ := money.New(minor, money.MustCurrency("KES"))
	return a
}

func TestCalculate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name               string
		terms              Terms
		amount             int64
		wantPct, wantTotal int64
	}{
		{"percentage only", Terms{PercentagePPM: 15_000, Rounding: money.RoundHalfUp}, 100000, 1500, 1500},
		{"fixed only", Terms{Fixed: 1000, Rounding: money.RoundHalfUp}, 100000, 0, 1000},
		{"percentage plus fixed", Terms{PercentagePPM: 10_000, Fixed: 500, Rounding: money.RoundHalfUp}, 100000, 1000, 1500},
		{"rounding half up", Terms{PercentagePPM: 15_000, Rounding: money.RoundHalfUp}, 100033, 1500, 1500},
		{"rounding up", Terms{PercentagePPM: 15_000, Rounding: money.RoundUp}, 100033, 1501, 1501},
		{"rounding half even on exact half", Terms{PercentagePPM: 25_000, Rounding: money.RoundHalfEven}, 100, 2, 2},
		{"minimum applies", Terms{PercentagePPM: 10_000, Minimum: ptr(500), Rounding: money.RoundHalfUp}, 10000, 100, 500},
		{"maximum applies", Terms{PercentagePPM: 50_000, Maximum: ptr(2000), Rounding: money.RoundHalfUp}, 100000, 5000, 2000},
		{"between min and max", Terms{PercentagePPM: 20_000, Minimum: ptr(100), Maximum: ptr(5000), Rounding: money.RoundHalfUp}, 100000, 2000, 2000},
		{"never above the amount", Terms{Fixed: 5000, Rounding: money.RoundHalfUp}, 3000, 0, 3000},
		{"minimum capped at amount", Terms{Minimum: ptr(1000), Rounding: money.RoundHalfUp}, 400, 0, 400},
		{"zero fee", Terms{Rounding: money.RoundDown}, 100000, 0, 0},
		{"one hundred percent", Terms{PercentagePPM: PPM, Rounding: money.RoundDown}, 777, 777, 777},
	}
	for _, tt := range tests {
		b, err := Calculate(tt.terms, kes(tt.amount))
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if b.Percentage.Minor() != tt.wantPct || b.Total.Minor() != tt.wantTotal || b.Fixed.Minor() != tt.terms.Fixed {
			t.Errorf("%s: got pct=%d fixed=%d total=%d, want pct=%d total=%d", tt.name,
				b.Percentage.Minor(), b.Fixed.Minor(), b.Total.Minor(), tt.wantPct, tt.wantTotal)
		}
		if b.Total.Currency().Code() != "KES" {
			t.Errorf("%s: currency %s", tt.name, b.Total.Currency())
		}
	}
}

func TestCalculateRejects(t *testing.T) {
	t.Parallel()
	bad := map[string]Terms{
		"negative ppm":     {PercentagePPM: -1, Rounding: money.RoundUp},
		"ppm over 100%":    {PercentagePPM: PPM + 1, Rounding: money.RoundUp},
		"negative fixed":   {Fixed: -1, Rounding: money.RoundUp},
		"negative minimum": {Minimum: ptr(-1), Rounding: money.RoundUp},
		"min above max":    {Minimum: ptr(10), Maximum: ptr(5), Rounding: money.RoundUp},
		"no rounding mode": {PercentagePPM: 10},
	}
	for name, terms := range bad {
		if _, err := Calculate(terms, kes(100)); !errors.Is(err, ErrInvalidTerms) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := Calculate(Terms{Rounding: money.RoundUp}, kes(0)); err == nil {
		t.Error("zero amount accepted")
	}
}

// The fee never exceeds the amount and never goes negative, for any terms.
func TestCalculateBounds(t *testing.T) {
	t.Parallel()
	for _, amount := range []int64{1, 99, 100, 101, 12345, 1_000_000, 50_000_000} {
		for _, ppm := range []int64{0, 1, 9_999, 35_000, PPM} {
			for _, fixed := range []int64{0, 1, 1_000, 100_000_000} {
				b, err := Calculate(Terms{PercentagePPM: ppm, Fixed: fixed, Rounding: money.RoundUp}, kes(amount))
				if err != nil {
					t.Fatal(err)
				}
				if b.Total.Minor() < 0 || b.Total.Minor() > amount {
					t.Fatalf("amount %d ppm %d fixed %d: total %d out of bounds", amount, ppm, fixed, b.Total.Minor())
				}
			}
		}
	}
}

var allowed = map[string]bool{
	"accrued>collection_pending": true, "accrued>collected": true, "accrued>waived": true,
	"accrued>refunded": true, "accrued>review_required": true,
	"collection_pending>collected": true, "collection_pending>collection_failed": true, "collection_pending>review_required": true,
	"collection_failed>collection_pending": true, "collection_failed>collected": true, "collection_failed>waived": true,
	"collection_failed>review_required": true,
	"collected>review_required":         true,
	"review_required>accrued":           true, "review_required>collected": true, "review_required>waived": true, "review_required>refunded": true,
}

func TestFeeTransitionsExhaustive(t *testing.T) {
	t.Parallel()
	for _, from := range AllStatuses {
		for _, to := range AllStatuses {
			key := fmt.Sprintf("%s>%s", from, to)
			noop, err := CheckTransition(from, to)
			switch {
			case from == to:
				if !noop || err != nil {
					t.Errorf("%s: want noop", key)
				}
			case allowed[key]:
				if err != nil {
					t.Errorf("%s: want allowed, got %v", key, err)
				}
			default:
				if !errors.Is(err, ErrInvalidTransition) {
					t.Errorf("%s: want rejection", key)
				}
			}
		}
	}
	for _, s := range []Status{StatusWaived, StatusRefunded} {
		if !s.IsTerminal() {
			t.Errorf("%s not terminal", s)
		}
	}
	// A collected fee can never be silently refunded or waived.
	if _, err := CheckTransition(StatusCollected, StatusRefunded); err == nil {
		t.Error("collected -> refunded allowed")
	}
}
