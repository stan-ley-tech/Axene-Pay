# ADR-0005: Money representation

- Status: Accepted
- Date: 2026-10-12

## Context

Floating point cannot represent most decimal fractions exactly. Currencies
have different minor-unit exponents (KES 2, UGX 0, BHD 3). Providers may use
different representations (M-Pesa STK Push takes whole shillings as a
number).

## Decision

- In Go: `money.Amount{Minor int64, Currency Currency}`. Arithmetic is only
  defined between amounts of the same currency and returns an error on
  overflow or currency mismatch.
- Currencies are an explicit allow-list with their ISO 4217 exponent; an
  unknown currency is a validation error, not a default.
- In PostgreSQL: `bigint` minor units + `char(3)` currency column, with a
  `CHECK` constraint on the currency code format.
- In the API: `"amount": 1050` as an integer in minor units plus
  `"currency": "KES"`. Never a JSON float, never a decimal string in the
  public API.
- Conversion to a provider's representation happens only inside that
  provider's adapter, and must fail (not round) when the amount is not
  representable (e.g. KES 10.50 for an API that accepts only whole shillings).
- Fee calculation rounding rules are a business decision and will be defined
  explicitly per fee schedule; there is no implicit rounding in `money`.

## Consequences

- Maximum representable amount is ~9.2×10^18 minor units — sufficient.
- FX is not supported; multi-currency means separate balances per currency.
