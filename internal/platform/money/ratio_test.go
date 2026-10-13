package money

import (
	"errors"
	"math"
	"math/big"
	"testing"
)

func TestMulRatioRoundingModes(t *testing.T) {
	t.Parallel()
	// 1.5% of 1,000.33 KES (100033 minor) = 1500.495 minor units.
	// 2.5% of 100 minor = 2.5 (exact half), 3.5% of 100 = 3.5.
	tests := []struct {
		minor, num, den int64
		mode            RoundingMode
		want            int64
	}{
		{100033, 15000, 1_000_000, RoundHalfUp, 1500},
		{100033, 15000, 1_000_000, RoundHalfEven, 1500},
		{100033, 15000, 1_000_000, RoundUp, 1501},
		{100033, 15000, 1_000_000, RoundDown, 1500},
		{100, 25000, 1_000_000, RoundHalfUp, 3},
		{100, 25000, 1_000_000, RoundHalfEven, 2}, // 2.5 -> 2 (even)
		{100, 35000, 1_000_000, RoundHalfEven, 4}, // 3.5 -> 4 (even)
		{100, 25000, 1_000_000, RoundUp, 3},
		{100, 25000, 1_000_000, RoundDown, 2},
		{100, 26000, 1_000_000, RoundHalfUp, 3},   // 2.6
		{100, 24000, 1_000_000, RoundHalfUp, 2},   // 2.4
		{100, 24000, 1_000_000, RoundHalfEven, 2}, // 2.4
		{100, 26000, 1_000_000, RoundHalfEven, 3}, // 2.6
		{0, 15000, 1_000_000, RoundUp, 0},
		{12345, 0, 1_000_000, RoundUp, 0},
		{10000, 1_000_000, 1_000_000, RoundDown, 10000}, // 100%
		{1, 1, 3, RoundUp, 1},
		{2, 1, 3, RoundHalfUp, 1}, // 0.667
		{1, 1, 3, RoundHalfUp, 0}, // 0.333
	}
	kes := MustCurrency("KES")
	for _, tt := range tests {
		a, _ := New(tt.minor, kes)
		got, err := a.MulRatio(tt.num, tt.den, tt.mode)
		if err != nil || got.Minor() != tt.want || got.Currency() != kes {
			t.Errorf("%d × %d/%d %s = %v, %v; want %d", tt.minor, tt.num, tt.den, tt.mode, got.Minor(), err, tt.want)
		}
	}
}

// TestMulRatioMatchesBigRational cross-checks every mode against exact
// rational arithmetic across magnitudes, including near-overflow values.
func TestMulRatioMatchesBigRational(t *testing.T) {
	t.Parallel()
	kes := MustCurrency("KES")
	amounts := []int64{1, 7, 99, 100, 101, 12345, 999_999, 1_000_000_007, math.MaxInt64 / 1_000_000, math.MaxInt64 / 2, math.MaxInt64}
	rates := []int64{0, 1, 3, 9_999, 15_000, 25_000, 333_333, 999_999, 1_000_000}
	for _, minor := range amounts {
		for _, num := range rates {
			for _, mode := range []RoundingMode{RoundHalfUp, RoundHalfEven, RoundUp, RoundDown} {
				a, _ := New(minor, kes)
				got, err := a.MulRatio(num, 1_000_000, mode)
				want := exact(minor, num, 1_000_000, mode)
				if err != nil {
					if want.IsInt64() {
						t.Errorf("%d × %d %s: unexpected %v", minor, num, mode, err)
					}
					continue
				}
				if !want.IsInt64() || got.Minor() != want.Int64() {
					t.Errorf("%d × %d %s = %d, want %s", minor, num, mode, got.Minor(), want)
				}
			}
		}
	}
}

func exact(minor, num, den int64, mode RoundingMode) *big.Int {
	p := new(big.Int).Mul(big.NewInt(minor), big.NewInt(num))
	q, r := new(big.Int).QuoRem(p, big.NewInt(den), new(big.Int))
	twoR := new(big.Int).Mul(r, big.NewInt(2))
	cmp := twoR.Cmp(big.NewInt(den))
	up := false
	switch mode {
	case RoundHalfUp:
		up = r.Sign() != 0 && cmp >= 0
	case RoundHalfEven:
		up = cmp > 0 || (cmp == 0 && q.Bit(0) == 1)
	case RoundUp:
		up = r.Sign() != 0
	}
	if up {
		q.Add(q, big.NewInt(1))
	}
	return q
}

func TestMulRatioErrors(t *testing.T) {
	t.Parallel()
	kes := MustCurrency("KES")
	a, _ := New(100, kes)
	neg, _ := New(-100, kes)
	big, _ := New(math.MaxInt64, kes)
	cases := map[string]error{}
	if _, err := neg.MulRatio(1, 2, RoundUp); !errors.Is(err, ErrNegative) {
		cases["negative amount"] = err
	}
	if _, err := a.MulRatio(-1, 2, RoundUp); !errors.Is(err, ErrNegative) {
		cases["negative ratio"] = err
	}
	if _, err := a.MulRatio(1, 0, RoundUp); err == nil {
		cases["zero denominator"] = err
	}
	if _, err := a.MulRatio(1, 2, "sideways"); err == nil {
		cases["bad mode"] = err
	}
	if _, err := big.MulRatio(3, 2, RoundDown); !errors.Is(err, ErrOverflow) {
		cases["overflow"] = err
	}
	if _, err := (Amount{}).MulRatio(1, 2, RoundUp); !errors.Is(err, ErrInvalidCurrency) {
		cases["zero currency"] = err
	}
	for name, err := range cases {
		t.Errorf("%s: err = %v", name, err)
	}
	for _, m := range []RoundingMode{RoundHalfUp, RoundHalfEven, RoundUp, RoundDown} {
		if !m.Valid() {
			t.Errorf("%s invalid", m)
		}
	}
}
