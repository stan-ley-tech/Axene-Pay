# Ledger and Accounting Rules

Status: **Principles accepted; chart of accounts blocked on decision D-02
(funds flow).** The ledger code in Phase 3 must not be written until D-02 is
answered, because the accounts Axene may legitimately hold depend on it.

## 1. Invariants (enforced in code *and* schema)

1. Every journal entry has ≥ 2 lines and the sum of debits equals the sum of
   credits **per currency**.
2. Posted entries and lines are immutable: no `UPDATE`/`DELETE` grants for the
   application role; corrections are new entries (`reversal` or
   `adjustment`) that reference the original.
3. Amounts are positive `bigint` minor units; direction is a separate
   `debit`/`credit` column. No floats, no signed-amount ambiguity.
4. Each account has exactly one currency; a line's currency must equal its
   account's currency.
5. Each entry has a `posting_reason` from a closed enum and a source
   reference `(source_type, source_id)`. Unique constraint on
   `(source_type, source_id, posting_reason)` prevents duplicate postings
   from replayed events.
6. Balances are derived from lines. A cached balance table, if introduced
   for performance, is updated in the same transaction as the lines and is
   verifiable by recomputation.
7. Ledger posting happens in the same DB transaction as the business state
   change that causes it.

## 2. Funds-flow models (decision D-02)

How Axene's ledger looks depends on who legally receives the customer's money.

### Model A — Technical gateway (merchant owns the collection account)

The merchant has its own M-Pesa Paybill/Till; money settles to the merchant
directly. Axene never holds merchant funds.

- Axene's ledger tracks: fees receivable from merchants, Axene revenue,
  and memo (off-balance) records of merchant transaction volume.
- Simplest regulatory position; settlement is between provider and merchant.

### Model B — Aggregator / collecting agent (Axene's collection account)

Customers pay into an Axene-controlled Paybill/Till; Axene owes the merchant
the net amount and pays out.

- Axene's ledger tracks: provider receivable, cash at bank (per account),
  merchant payable (liability), fee revenue, refunds payable, payout
  clearing.
- Usually requires specific licensing/authorization (e.g. payment service
  provider authorization, possibly trust/segregated accounts). The CEO's
  licensing work determines whether this is allowed.

### Provisional postings for Model B (illustrative, not final)

| Event | Debit | Credit |
|-------|-------|--------|
| Payment succeeded (gross G) | `provider_receivable:mpesa` G | `merchant_payable:{merchant}` G |
| Fee charged (F) | `merchant_payable:{merchant}` F | `fee_revenue` F |
| Provider settles to bank (S) | `cash:{bank_account}` S | `provider_receivable:mpesa` S |
| Provider fee withheld (P) | `provider_fees_expense` P | `provider_receivable:mpesa` P |
| Payout to merchant (N) | `merchant_payable:{merchant}` N | `cash:{bank_account}` N |
| Refund executed (R) | `merchant_payable:{merchant}` R | `provider_receivable:mpesa` R (or cash, depending on how the provider debits) |

These only become real after the funds-flow and fee-bearing questions in
[decisions.md](decisions.md) are answered.

## 3. Fees

- A fee schedule is versioned data, not code constants. Every fee line
  records which schedule version produced it.
- Expected provider cost and actual (reconciled) provider cost are recorded
  separately so routing cost analysis compares real numbers.
- Rounding rule for percentage fees (half-up, half-even, always-up toward
  Axene) is a business decision; it must be stated in the schedule and
  tested.

## 4. Reporting basis

- Operational dashboards may show provisional numbers (payments
  `SUCCEEDED`, not yet settled/reconciled). They must be labelled
  provisional.
- Financial reports come only from posted ledger entries and state their
  basis (e.g. "posted entries up to T, reconciliation status as of R").
