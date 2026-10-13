// Package fees implements Axene's fee engine (ADR-0011): versioned fee
// schedules, exact fee calculation, fee accrual on successful payments, the
// evidence-based fee lifecycle, and the pluggable collection framework.
//
// No commercial rate is defined in code. Rates are schedule data entered and
// approved by operators; test fixtures are labelled as such.
package fees

import (
	"errors"
	"fmt"

	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
)

// PPM is the denominator of percentage rates: parts per million
// (1.5% = 15,000 ppm).
const PPM = 1_000_000

// Terms are the pricing terms of a fee schedule.
type Terms struct {
	PercentagePPM int64
	Fixed         int64  // minor units
	Minimum       *int64 // minor units; nil = no minimum
	Maximum       *int64 // minor units; nil = no maximum
	Rounding      money.RoundingMode
}

// Breakdown is a calculated fee.
type Breakdown struct {
	Percentage money.Amount // percentage component after rounding
	Fixed      money.Amount
	Total      money.Amount // after minimum/maximum, never above the amount
}

// ErrInvalidTerms wraps schedule validation failures.
var ErrInvalidTerms = errors.New("fees: invalid terms")

// Validate checks terms independently of any payment.
func (t Terms) Validate() error {
	switch {
	case t.PercentagePPM < 0 || t.PercentagePPM > PPM:
		return fmt.Errorf("%w: percentage must be 0-1,000,000 ppm", ErrInvalidTerms)
	case t.Fixed < 0:
		return fmt.Errorf("%w: fixed fee must not be negative", ErrInvalidTerms)
	case t.Minimum != nil && *t.Minimum < 0, t.Maximum != nil && *t.Maximum < 0:
		return fmt.Errorf("%w: minimum and maximum must not be negative", ErrInvalidTerms)
	case t.Minimum != nil && t.Maximum != nil && *t.Minimum > *t.Maximum:
		return fmt.Errorf("%w: minimum exceeds maximum", ErrInvalidTerms)
	case !t.Rounding.Valid():
		return fmt.Errorf("%w: unknown rounding mode %q", ErrInvalidTerms, t.Rounding)
	}
	return nil
}

// Calculate computes the fee for amount:
//
//	percentage = round(amount × ppm / 1,000,000, rounding)
//	total      = clamp(percentage + fixed, minimum, maximum), at most amount
//
// The fee can never exceed the payment amount: a fee that would is capped at
// the amount (the schedule's minimum or fixed component cannot make a
// payment cost the merchant more than it collected).
func Calculate(t Terms, amount money.Amount) (Breakdown, error) {
	if err := t.Validate(); err != nil {
		return Breakdown{}, err
	}
	if !amount.IsPositive() {
		return Breakdown{}, fmt.Errorf("fees: amount must be positive")
	}
	pct, err := amount.MulRatio(t.PercentagePPM, PPM, t.Rounding)
	if err != nil {
		return Breakdown{}, err
	}
	fixed, err := money.New(t.Fixed, amount.Currency())
	if err != nil {
		return Breakdown{}, err
	}
	total, err := pct.Add(fixed)
	if err != nil {
		return Breakdown{}, err
	}
	minor := total.Minor()
	if t.Minimum != nil && minor < *t.Minimum {
		minor = *t.Minimum
	}
	if t.Maximum != nil && minor > *t.Maximum {
		minor = *t.Maximum
	}
	if minor > amount.Minor() {
		minor = amount.Minor()
	}
	total, _ = money.New(minor, amount.Currency())
	return Breakdown{Percentage: pct, Fixed: fixed, Total: total}, nil
}
