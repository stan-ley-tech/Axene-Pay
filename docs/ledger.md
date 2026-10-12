# Ledger and Accounting Rules

Status: **Design accepted for the direct-settlement model
([ADR-0008](adr/0008-direct-merchant-settlement.md)); implementation is
Phase 3.** Fee amounts and who bears provider fees remain open (D-12).

## 1. Invariants (enforced in code *and* schema)

1. Every journal entry has ≥ 2 lines and debits equal credits **per currency
   and per book**.
2. Posted entries and lines are immutable: the application role has no
   `UPDATE`/`DELETE`; corrections are new `reversal` or `adjustment` entries
   referencing the original.
3. Amounts are positive `bigint` minor units with an explicit
   `debit`/`credit` direction. No floats.
4. Each account has exactly one currency and belongs to exactly one book.
5. Each entry has a `posting_reason` from a closed enum and a source
   reference. A unique constraint on `(source_type, source_id,
   posting_reason)` makes financial effects exactly-once even when the
   triggering event is processed more than once.
6. Balances are derived from lines. Any cached balance is written in the same
   transaction as its lines and is verifiable by recomputation. There is no
   free-standing mutable `balance` column.
7. Postings commit in the same transaction as the business state change
   that causes them.

## 2. Two books

Under direct settlement Axene never holds the customer's money, so recording
the gross payment as Axene's asset or liability would be wrong. The ledger
therefore keeps two separate, individually balanced books:

### Book `axene` — Axene's own financial positions

What Axene earns, is owed, and owes.

| Account | Type | Meaning |
|---------|------|---------|
| `fees_receivable:{merchant}` | asset | Axene fees charged to a merchant, not yet collected |
| `fee_revenue` | revenue | Axene processing fees earned |
| `provider_costs_payable:{provider}` | liability | Provider charges Axene owes (only if D-12 says Axene bears them) |
| `provider_costs` | expense | Same, expense side |
| `cash:{axene_bank_account}` | asset | When fees are actually collected |

### Book `settlement_memo` — memorandum of funds Axene does not hold

Tracks what the provider should deliver to each merchant destination, so it
can be reconciled against provider statements. It is never reported as
Axene's assets or liabilities.

| Account | Meaning |
|---------|---------|
| `processed_volume:{merchant}` | Gross amount of verified payments |
| `settlement_expected:{merchant}:{destination}` | Amount the provider should settle to the merchant destination |
| `provider_fees_withheld:{merchant}` | Fees the provider deducts from the merchant |
| `settled:{merchant}:{destination}` | Amount confirmed settled by statement |

### Postings (Phase 3 target)

| Business event | Book | Debit | Credit |
|----------------|------|-------|--------|
| Payment verified `SUCCEEDED`, gross G | memo | `settlement_expected:{m}:{d}` G | `processed_volume:{m}` G |
| Axene fee F charged | axene | `fees_receivable:{m}` F | `fee_revenue` F |
| Provider fee P withheld from merchant (from statement) | memo | `provider_fees_withheld:{m}` P | `settlement_expected:{m}:{d}` P |
| Settlement N confirmed by statement | memo | `settled:{m}:{d}` N | `settlement_expected:{m}:{d}` N |
| Payment `REVERSED` | memo (and axene if fee refunded) | mirror of the original lines, referencing it |
| Fee collected | axene | `cash:{acct}` F | `fees_receivable:{m}` F |

The posting policy is selected per settlement destination (`owner =
merchant` today). A future aggregator model adds a different policy (merchant
payables, payouts) without changing the payment domain.

## 3. Settlement status

Settlement status is its own record (Phase 3), not a payment state: a payment
can be `SUCCEEDED` while its settlement is `expected`, `settled`, or
`discrepant`.

## 4. Fees

- Fee schedules are versioned data; each fee line records the schedule
  version that produced it.
- Expected provider cost and reconciled provider cost are stored separately
  so routing cost analysis compares real numbers.
- Rounding rules for percentage fees are part of each schedule and tested.
- How Axene collects fees when it never holds the funds (invoice, separate
  charge) is decision D-12.

## 5. Reporting basis

- Operational dashboards may show provisional figures (verified payments not
  yet settled/reconciled) and must label them provisional.
- Financial reports come only from posted ledger entries and state their
  basis.
