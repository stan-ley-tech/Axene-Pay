package money

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
)

// RoundingMode selects how a fractional minor unit is resolved.
type RoundingMode string

const (
	// RoundHalfUp rounds halves away from zero (0.5 -> 1).
	RoundHalfUp RoundingMode = "half_up"
	// RoundHalfEven rounds halves to the nearest even unit (banker's rounding).
	RoundHalfEven RoundingMode = "half_even"
	// RoundUp rounds any fraction up (ceiling).
	RoundUp RoundingMode = "up"
	// RoundDown discards any fraction (floor).
	RoundDown RoundingMode = "down"
)

// Valid reports whether m is a known rounding mode.
func (m RoundingMode) Valid() bool {
	switch m {
	case RoundHalfUp, RoundHalfEven, RoundUp, RoundDown:
		return true
	}
	return false
}

// ErrNegative is returned by operations defined only for non-negative values.
var ErrNegative = errors.New("money: negative value")

// MulRatio returns a × num / den rounded to a whole minor unit with mode.
// It is exact for every non-negative int64 amount and ratio: the product is
// computed in 128 bits, so no intermediate overflow or floating point is
// involved. Used for percentage fees (num = rate in parts per million,
// den = 1,000,000).
func (a Amount) MulRatio(num, den int64, mode RoundingMode) (Amount, error) {
	if a.currency.IsZero() {
		return Amount{}, ErrInvalidCurrency
	}
	if !mode.Valid() {
		return Amount{}, fmt.Errorf("money: unknown rounding mode %q", mode)
	}
	if a.minor < 0 || num < 0 {
		return Amount{}, ErrNegative
	}
	if den <= 0 {
		return Amount{}, errors.New("money: ratio denominator must be positive")
	}
	hi, lo := bits.Mul64(uint64(a.minor), uint64(num))
	d := uint64(den)
	if hi >= d {
		return Amount{}, ErrOverflow
	}
	q, r := bits.Div64(hi, lo, d)
	// r < d <= 2^63, so 2r cannot overflow uint64.
	switch mode {
	case RoundHalfUp:
		if 2*r >= d && r != 0 {
			q++
		}
	case RoundHalfEven:
		if 2*r > d || (2*r == d && q%2 == 1) {
			q++
		}
	case RoundUp:
		if r != 0 {
			q++
		}
	}
	if q > math.MaxInt64 {
		return Amount{}, ErrOverflow
	}
	return Amount{minor: int64(q), currency: a.currency}, nil
}
