// Package money provides exact monetary amounts in integer minor units.
//
// Floating point is never used. Arithmetic is defined only between amounts
// of the same currency and reports overflow instead of wrapping. See
// docs/adr/0005-money-representation.md.
package money

import (
	"errors"
	"fmt"
)

// Currency is an ISO 4217 alphabetic code known to this package.
//
// The zero value is not a valid currency. Construct with ParseCurrency.
type Currency struct {
	code     string
	exponent uint8
}

// ErrUnknownCurrency is returned for codes not in the representation table.
var ErrUnknownCurrency = errors.New("money: unknown currency")

// currencies maps ISO 4217 codes to their minor-unit exponent.
//
// This table answers "how is this currency represented", not "which
// currencies does Axene accept". Enabled currencies are configuration of
// merchants and providers. Add entries only with their official ISO 4217
// exponent.
var currencies = map[string]uint8{
	"KES": 2, // Kenyan shilling
	"UGX": 0, // Ugandan shilling
	"TZS": 2, // Tanzanian shilling
	"RWF": 0, // Rwandan franc
	"USD": 2, // US dollar
}

// ParseCurrency returns the Currency for an upper-case ISO 4217 code.
// Lower-case input is rejected rather than normalized so that stored and
// transmitted codes have exactly one spelling.
func ParseCurrency(code string) (Currency, error) {
	exp, ok := currencies[code]
	if !ok {
		return Currency{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, code)
	}
	return Currency{code: code, exponent: exp}, nil
}

// MustCurrency is ParseCurrency for compile-time constants in tests and
// static configuration. It panics on unknown codes.
func MustCurrency(code string) Currency {
	c, err := ParseCurrency(code)
	if err != nil {
		panic(err)
	}
	return c
}

// Code returns the ISO 4217 alphabetic code.
func (c Currency) Code() string { return c.code }

// Exponent returns the number of minor-unit digits (2 for KES, 0 for UGX).
func (c Currency) Exponent() uint8 { return c.exponent }

// IsZero reports whether c is the invalid zero value.
func (c Currency) IsZero() bool { return c.code == "" }

func (c Currency) String() string { return c.code }
