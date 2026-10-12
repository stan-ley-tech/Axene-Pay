package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var (
	// ErrCurrencyMismatch is returned when combining different currencies.
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	// ErrOverflow is returned when a result does not fit in int64 minor units.
	ErrOverflow = errors.New("money: overflow")
	// ErrInvalidDecimal is returned by ParseDecimal for malformed input.
	ErrInvalidDecimal = errors.New("money: invalid decimal amount")
	// ErrPrecision is returned when a value has more precision than the
	// target representation allows. Values are never silently rounded.
	ErrPrecision = errors.New("money: precision loss")
	// ErrInvalidCurrency is returned when an amount is built with the zero Currency.
	ErrInvalidCurrency = errors.New("money: invalid currency")
)

// Amount is a signed quantity of a currency in minor units.
//
// Sign is allowed so that differences can be computed; whether a negative
// value is acceptable is decided by the caller (API amounts must be
// positive, ledger lines carry direction separately).
type Amount struct {
	minor    int64
	currency Currency
}

// New returns an Amount of minor units in currency c.
func New(minor int64, c Currency) (Amount, error) {
	if c.IsZero() {
		return Amount{}, ErrInvalidCurrency
	}
	return Amount{minor: minor, currency: c}, nil
}

// Minor returns the value in minor units.
func (a Amount) Minor() int64 { return a.minor }

// Currency returns the amount's currency.
func (a Amount) Currency() Currency { return a.currency }

// IsZero reports whether the amount is zero.
func (a Amount) IsZero() bool { return a.minor == 0 }

// IsPositive reports whether the amount is strictly greater than zero.
func (a Amount) IsPositive() bool { return a.minor > 0 }

// IsNegative reports whether the amount is strictly less than zero.
func (a Amount) IsNegative() bool { return a.minor < 0 }

// Add returns a+b. Both must share a currency.
func (a Amount) Add(b Amount) (Amount, error) {
	if err := a.sameCurrency(b); err != nil {
		return Amount{}, err
	}
	if (b.minor > 0 && a.minor > math.MaxInt64-b.minor) ||
		(b.minor < 0 && a.minor < math.MinInt64-b.minor) {
		return Amount{}, ErrOverflow
	}
	return Amount{minor: a.minor + b.minor, currency: a.currency}, nil
}

// Sub returns a-b. Both must share a currency.
func (a Amount) Sub(b Amount) (Amount, error) {
	if err := a.sameCurrency(b); err != nil {
		return Amount{}, err
	}
	if (b.minor < 0 && a.minor > math.MaxInt64+b.minor) ||
		(b.minor > 0 && a.minor < math.MinInt64+b.minor) {
		return Amount{}, ErrOverflow
	}
	return Amount{minor: a.minor - b.minor, currency: a.currency}, nil
}

// Cmp compares a and b, returning -1, 0, or +1. Both must share a currency.
func (a Amount) Cmp(b Amount) (int, error) {
	if err := a.sameCurrency(b); err != nil {
		return 0, err
	}
	switch {
	case a.minor < b.minor:
		return -1, nil
	case a.minor > b.minor:
		return 1, nil
	default:
		return 0, nil
	}
}

// WholeUnits returns the amount in major units, failing if it has a
// non-zero fractional part. Provider adapters that only accept whole units
// use this so that 10.50 is rejected instead of being rounded.
func (a Amount) WholeUnits() (int64, error) {
	div := pow10(a.currency.exponent)
	if a.minor%div != 0 {
		return 0, fmt.Errorf("%w: %s is not a whole number of %s", ErrPrecision, a, a.currency)
	}
	return a.minor / div, nil
}

// Decimal formats the amount as a plain decimal string with exactly
// Exponent fractional digits, e.g. "1050" KES -> "10.50", "-5" KES -> "-0.05".
func (a Amount) Decimal() string {
	exp := int(a.currency.exponent)
	neg := a.minor < 0
	// Work on the unsigned magnitude so MinInt64 does not overflow on negation.
	mag := uint64(a.minor)
	if neg {
		mag = -mag
	}
	digits := strconv.FormatUint(mag, 10)
	if exp > 0 {
		if len(digits) <= exp {
			digits = strings.Repeat("0", exp-len(digits)+1) + digits
		}
		digits = digits[:len(digits)-exp] + "." + digits[len(digits)-exp:]
	}
	if neg {
		return "-" + digits
	}
	return digits
}

// String formats the amount for humans and logs, e.g. "KES 10.50".
func (a Amount) String() string {
	if a.currency.IsZero() {
		return "<invalid amount>"
	}
	return a.currency.code + " " + a.Decimal()
}

// ParseDecimal parses a plain decimal string ("10", "10.5", "10.50",
// "-3.25") in major units. More fractional digits than the currency's
// exponent is an error, never rounded. Exponent notation, thousands
// separators, whitespace and a leading '+' are rejected.
func ParseDecimal(s string, c Currency) (Amount, error) {
	if c.IsZero() {
		return Amount{}, ErrInvalidCurrency
	}
	orig := s
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	intPart, fracPart, hasDot := strings.Cut(s, ".")
	if intPart == "" || (hasDot && fracPart == "") || !allDigits(intPart) || !allDigits(fracPart) {
		return Amount{}, fmt.Errorf("%w: %q", ErrInvalidDecimal, orig)
	}
	exp := int(c.exponent)
	if len(fracPart) > exp {
		return Amount{}, fmt.Errorf("%w: %q has more than %d fractional digits for %s", ErrPrecision, orig, exp, c)
	}
	fracPart += strings.Repeat("0", exp-len(fracPart))

	// Parse as unsigned so that the full int64 range including MinInt64 is reachable.
	mag, err := strconv.ParseUint(intPart+fracPart, 10, 64)
	if err != nil {
		return Amount{}, fmt.Errorf("%w: %q", ErrOverflow, orig)
	}
	var minor int64
	switch {
	case !neg && mag <= math.MaxInt64:
		minor = int64(mag)
	case neg && mag <= uint64(math.MaxInt64)+1:
		minor = int64(-mag) // two's-complement negation; exact for mag == 2^63
	default:
		return Amount{}, fmt.Errorf("%w: %q", ErrOverflow, orig)
	}
	return Amount{minor: minor, currency: c}, nil
}

func (a Amount) sameCurrency(b Amount) error {
	if a.currency.IsZero() || b.currency.IsZero() {
		return ErrInvalidCurrency
	}
	if a.currency.code != b.currency.code {
		return fmt.Errorf("%w: %s and %s", ErrCurrencyMismatch, a.currency, b.currency)
	}
	return nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func pow10(n uint8) int64 {
	r := int64(1)
	for i := uint8(0); i < n; i++ {
		r *= 10
	}
	return r
}
