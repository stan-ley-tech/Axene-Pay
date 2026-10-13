# Ledger and Accounting Rules

Status: **Implemented in Phase 3 for the direct-settlement model
([ADR-0008](adr/0008-direct-merchant-settlement.md)) and the fee engine
([ADR-0011](adr/0011-fees-and-automatic-collection.md)).** Rates, the
provider-fee bearer, and the fee-collection mechanism remain open (D-12a–g).
No posting exists for a funds flow that is not in force.

## 1. Invariants (enforced in code *and* schema)

1. Every journal entry has ≥ 2 lines and debits equal credits **per
   currency**. Enforced by a deferred constraint trigger at commit.
2. Posted entries and lines are immutable (`UPDATE`/`DELETE` rejected by
   trigger); corrections are new entries that reference the original
   (`reverses_entry_id`).
3. Amounts are positive `bigint` minor units with an explicit `debit` /
   `credit` direction. No floats anywhere.
4. Each account has exactly one currency and one book; a line's currency must
   equal its account's currency and its entry's book must equal the account's
   book.
5. Each entry has a `posting_reason` from a closed set and a source reference.
   A unique constraint on `(source_type, source_id, posting_reason)` makes
   every financial effect exactly-once: replayed callbacks, retried jobs, and
   concurrent workers cannot post twice.
6. Balances are derived from lines. There is no mutable balance column.
7. Postings commit in the same transaction as the business change that
   causes them (payment transition, fee status change, settlement record).

## 2. Books

Under direct settlement Axene never holds the customer's money, so the gross
payment is not Axene's asset or liability. Two books, each balanced:

### Book `axene` — Axene's own positions

| Account | Type | Meaning |
|---------|------|---------|
| `fees_receivable:{merchant}` | asset | Fees accrued for a merchant, not yet collected |
| `fee_revenue` | revenue | Axene fees earned (recognized on accrual) |
| `fee_collections:{mechanism}` | asset | Fees collected, held by the collection mechanism until matched to cash |
| `provider_fee_expense` | expense | Provider fees Axene bears (only if D-12b says so) |
| `provider_fees_payable:{provider}` | liability | Provider fees Axene owes |

### Book `settlement_memo` — funds Axene does not hold

Tracks what the provider should deliver to each merchant destination, for
reconciliation. Never reported as Axene's assets or liabilities.

| Account | Meaning |
|---------|---------|
| `processed_volume:{merchant}` | Gross verified payments |
| `settlement_expected:{merchant}:{destination}` | Amount the provider should settle to the destination |
| `provider_fees_withheld:{merchant}` | Provider fees deducted from the merchant's settlement |
| `settled:{merchant}:{destination}` | Amount confirmed settled by statement |

## 3. Postings

| Business event | Reason | Book | Debit | Credit |
|----------------|--------|------|-------|--------|
| Payment verified `SUCCEEDED`, gross G | `payment_succeeded` | memo | `settlement_expected:{m}:{d}` G | `processed_volume:{m}` G |
| Axene fee F accrued (same transaction) | `fee_accrued` | axene | `fees_receivable:{m}` F | `fee_revenue` F |
| Fee collected, with evidence | `fee_collected` | axene | `fee_collections:{mechanism}` F | `fees_receivable:{m}` F |
| Fee waived or refunded | `fee_reversed` | axene | reverses `fee_accrued` | |
| Settlement statement line: provider fee P withheld from merchant | `provider_fee_withheld` | memo | `provider_fees_withheld:{m}` P | `settlement_expected:{m}:{d}` P |
| Provider fee P borne by Axene (D-12b) | `provider_fee_axene` | axene | `provider_fee_expense` P | `provider_fees_payable:{provider}` P |
| Settlement statement line: net N settled | `settlement_confirmed` | memo | `settled:{m}:{d}` N | `settlement_expected:{m}:{d}` N |
| Payment `REVERSED` | `payment_reversed` | memo | reverses `payment_succeeded` | |

The fee is **not** recorded as a deduction from the merchant's payment:
under direct settlement it is a separate receivable from the merchant until
collection evidence exists. If option B (Axene-controlled collection) is
approved, a new posting policy adds merchant payables and payouts
(ADR-0011); nothing in this table changes.

## 4. Fee lifecycle

```
accrued ──mechanism assigned──▶ collection_pending ──evidence──▶ collected
   │                                   └──▶ collection_failed ──▶ collection_pending
   ├──▶ waived          (operator, with reason; reverses the accrual)
   ├──▶ review_required (reversal under pending_approval policy)
   └──▶ refunded        (reversal under refund_fee policy; reverses the accrual)
```

- `collected` requires a collection reference and evidence source
  (settlement item or provider record). Calculation is never evidence.
- With mechanism `none_configured` (the only selectable value today) fees
  stay `accrued` and appear as receivables awaiting a mechanism.
- Every status change is an append-only `fee_transitions` row.

## 5. Fee calculation

```
percentage = round(amount × percentage_ppm / 1,000,000, rounding_mode)
fee        = clamp(percentage + fixed, minimum, maximum)
```

- `percentage_ppm`: parts per million (1.5% = 15,000); integers only.
- Rounding modes: `half_up`, `half_even`, `up`, `down`, applied once, to the
  percentage component, in the currency's minor unit.
- The fee may not exceed the payment amount.
- The schedule used is the one effective when the payment was **created**
  (the terms agreed at initiation); its ID and version are stored on the fee.
- Selection: merchant + provider + method, then merchant + method, then
  platform default + provider, then platform default. No match → no fee is
  accrued and a `fee_schedule_missing` reconciliation exception is opened
  (never a silent zero fee).

## 6. Settlement and reconciliation

Settlement statements are imported in Axene's normalized format (one line per
provider transaction: reference, receipt, gross, provider fee, net, settled
at, and, where the mechanism reports it, the Axene fee collected). Provider
specific formats are converted by adapters once their real formats are known.
A reconciliation run matches lines to payments and fees and records:

| Result | Meaning |
|--------|---------|
| matched | Line agrees with a `SUCCEEDED` payment |
| amount_mismatch | Gross differs |
| status_mismatch | Statement shows a settled payment Axene has as `UNRESOLVED`/`FAILED` |
| unmatched_statement_line | No Axene payment for the line |
| duplicate_statement_line | Same provider reference twice |
| missing_settlement | `SUCCEEDED` payment absent from statements after the grace period |

Each non-match opens a reconciliation exception. Exceptions are resolved by an
operator with a recorded reason; resolving a payment's outcome uses
reconciliation evidence through the payment state machine. Reconciliation
never edits records to make reports agree.

## 7. Reporting basis

- Operational figures (verified payments, accrued fees) are provisional and
  labelled as such.
- Financial reports come only from posted ledger entries and state their
  basis (book, cut-off, reconciliation status).
