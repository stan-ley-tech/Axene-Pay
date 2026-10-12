package money

import (
	"errors"
	"math"
	"testing"
)

var (
	kes = MustCurrency("KES")
	ugx = MustCurrency("UGX")
)

func mustNew(t *testing.T, minor int64, c Currency) Amount {
	t.Helper()
	a, err := New(minor, c)
	if err != nil {
		t.Fatalf("New(%d, %s): %v", minor, c, err)
	}
	return a
}

func TestParseCurrency(t *testing.T) {
	t.Parallel()
	tests := []struct {
		code    string
		wantExp uint8
		wantErr bool
	}{
		{"KES", 2, false},
		{"UGX", 0, false},
		{"kes", 0, true}, // no normalization
		{"", 0, true},
		{"XXX", 0, true},
		{"KES ", 0, true},
	}
	for _, tt := range tests {
		c, err := ParseCurrency(tt.code)
		if tt.wantErr {
			if !errors.Is(err, ErrUnknownCurrency) {
				t.Errorf("ParseCurrency(%q) err = %v, want ErrUnknownCurrency", tt.code, err)
			}
			continue
		}
		if err != nil || c.Exponent() != tt.wantExp || c.Code() != tt.code {
			t.Errorf("ParseCurrency(%q) = %v/%d, %v", tt.code, c, c.Exponent(), err)
		}
	}
}

func TestNewRejectsZeroCurrency(t *testing.T) {
	t.Parallel()
	if _, err := New(100, Currency{}); !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("err = %v, want ErrInvalidCurrency", err)
	}
}

func TestAddSub(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		a, b    int64
		wantAdd int64
		addErr  error
		wantSub int64
		subErr  error
	}{
		{"simple", 1050, 250, 1300, nil, 800, nil},
		{"negative result", 100, 250, 350, nil, -150, nil},
		{"add overflow", math.MaxInt64, 1, 0, ErrOverflow, math.MaxInt64 - 1, nil},
		{"add underflow", math.MinInt64, -1, 0, ErrOverflow, math.MinInt64 + 1, nil},
		{"sub overflow", math.MaxInt64, -1, math.MaxInt64 - 1, nil, 0, ErrOverflow},
		{"sub underflow", math.MinInt64, 1, math.MinInt64 + 1, nil, 0, ErrOverflow},
		{"max boundary", math.MaxInt64 - 5, 5, math.MaxInt64, nil, math.MaxInt64 - 10, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, b := mustNew(t, tt.a, kes), mustNew(t, tt.b, kes)

			sum, err := a.Add(b)
			if !errors.Is(err, tt.addErr) {
				t.Fatalf("Add err = %v, want %v", err, tt.addErr)
			}
			if err == nil && sum.Minor() != tt.wantAdd {
				t.Fatalf("Add = %d, want %d", sum.Minor(), tt.wantAdd)
			}

			diff, err := a.Sub(b)
			if !errors.Is(err, tt.subErr) {
				t.Fatalf("Sub err = %v, want %v", err, tt.subErr)
			}
			if err == nil && diff.Minor() != tt.wantSub {
				t.Fatalf("Sub = %d, want %d", diff.Minor(), tt.wantSub)
			}
		})
	}
}

func TestCurrencyMismatch(t *testing.T) {
	t.Parallel()
	a, b := mustNew(t, 100, kes), mustNew(t, 100, ugx)
	if _, err := a.Add(b); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Add err = %v", err)
	}
	if _, err := a.Sub(b); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Sub err = %v", err)
	}
	if _, err := a.Cmp(b); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Cmp err = %v", err)
	}
	if _, err := a.Add(Amount{}); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("Add zero-value err = %v", err)
	}
}

func TestCmp(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		a, b int64
		want int
	}{{1, 2, -1}, {2, 1, 1}, {5, 5, 0}, {-1, 0, -1}} {
		got, err := mustNew(t, tt.a, kes).Cmp(mustNew(t, tt.b, kes))
		if err != nil || got != tt.want {
			t.Errorf("Cmp(%d,%d) = %d, %v; want %d", tt.a, tt.b, got, err, tt.want)
		}
	}
}

func TestPredicates(t *testing.T) {
	t.Parallel()
	if !mustNew(t, 0, kes).IsZero() || mustNew(t, 1, kes).IsZero() {
		t.Error("IsZero")
	}
	if !mustNew(t, 1, kes).IsPositive() || mustNew(t, 0, kes).IsPositive() {
		t.Error("IsPositive")
	}
	if !mustNew(t, -1, kes).IsNegative() || mustNew(t, 0, kes).IsNegative() {
		t.Error("IsNegative")
	}
}

func TestDecimalAndString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		minor int64
		c     Currency
		want  string
	}{
		{1050, kes, "10.50"},
		{5, kes, "0.05"},
		{-5, kes, "-0.05"},
		{0, kes, "0.00"},
		{100, kes, "1.00"},
		{1500, ugx, "1500"},
		{-1500, ugx, "-1500"},
		{math.MinInt64, kes, "-92233720368547758.08"},
		{math.MaxInt64, kes, "92233720368547758.07"},
	}
	for _, tt := range tests {
		a := mustNew(t, tt.minor, tt.c)
		if got := a.Decimal(); got != tt.want {
			t.Errorf("Decimal(%d %s) = %q, want %q", tt.minor, tt.c, got, tt.want)
		}
	}
	if got := mustNew(t, 1050, kes).String(); got != "KES 10.50" {
		t.Errorf("String = %q", got)
	}
	if got := (Amount{}).String(); got != "<invalid amount>" {
		t.Errorf("zero String = %q", got)
	}
}

func TestParseDecimal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		c       Currency
		want    int64
		wantErr error
	}{
		{"10.50", kes, 1050, nil},
		{"10.5", kes, 1050, nil},
		{"10", kes, 1000, nil},
		{"0.01", kes, 1, nil},
		{"-3.25", kes, -325, nil},
		{"007.00", kes, 700, nil},
		{"1500", ugx, 1500, nil},
		{"92233720368547758.07", kes, math.MaxInt64, nil},
		{"-92233720368547758.08", kes, math.MinInt64, nil},
		{"92233720368547758.08", kes, 0, ErrOverflow},
		{"-92233720368547758.09", kes, 0, ErrOverflow},
		{"99999999999999999999999", kes, 0, ErrOverflow},
		{"10.505", kes, 0, ErrPrecision}, // never rounded
		{"1500.5", ugx, 0, ErrPrecision},
		{"", kes, 0, ErrInvalidDecimal},
		{"-", kes, 0, ErrInvalidDecimal},
		{".50", kes, 0, ErrInvalidDecimal},
		{"10.", kes, 0, ErrInvalidDecimal},
		{"+10", kes, 0, ErrInvalidDecimal},
		{"1e3", kes, 0, ErrInvalidDecimal},
		{"1,000", kes, 0, ErrInvalidDecimal},
		{" 10", kes, 0, ErrInvalidDecimal},
		{"10.5.0", kes, 0, ErrInvalidDecimal},
		{"--1", kes, 0, ErrInvalidDecimal},
	}
	for _, tt := range tests {
		got, err := ParseDecimal(tt.in, tt.c)
		if !errors.Is(err, tt.wantErr) {
			t.Errorf("ParseDecimal(%q, %s) err = %v, want %v", tt.in, tt.c, err, tt.wantErr)
			continue
		}
		if err == nil && got.Minor() != tt.want {
			t.Errorf("ParseDecimal(%q, %s) = %d, want %d", tt.in, tt.c, got.Minor(), tt.want)
		}
	}
	if _, err := ParseDecimal("1", Currency{}); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("zero currency err = %v", err)
	}
}

func TestParseDecimalRoundTrip(t *testing.T) {
	t.Parallel()
	for _, minor := range []int64{0, 1, -1, 99, 100, 123456789, math.MaxInt64, math.MinInt64} {
		for _, c := range []Currency{kes, ugx} {
			a := mustNew(t, minor, c)
			back, err := ParseDecimal(a.Decimal(), c)
			if err != nil || back != a {
				t.Errorf("round trip %d %s: got %v, %v", minor, c, back, err)
			}
		}
	}
}

func TestWholeUnits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		minor   int64
		c       Currency
		want    int64
		wantErr error
	}{
		{1000, kes, 10, nil},
		{1050, kes, 0, ErrPrecision},
		{1, kes, 0, ErrPrecision},
		{0, kes, 0, nil},
		{1500, ugx, 1500, nil},
	}
	for _, tt := range tests {
		got, err := mustNew(t, tt.minor, tt.c).WholeUnits()
		if !errors.Is(err, tt.wantErr) || (err == nil && got != tt.want) {
			t.Errorf("WholeUnits(%d %s) = %d, %v; want %d, %v", tt.minor, tt.c, got, err, tt.want, tt.wantErr)
		}
	}
}
