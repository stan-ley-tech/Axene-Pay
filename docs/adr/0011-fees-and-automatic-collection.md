# ADR-0011: Fee engine and automatic fee collection

- Status: Accepted (requirement confirmed; collection mechanism pending)
- Date: 2026-10-13
- Updates: D-12; reopens the funds-flow part of [ADR-0008](0008-direct-merchant-settlement.md)

## Context

Confirmed business requirement: **Axene automatically collects its
transaction fee for each successful payment.** Manual invoicing is not the
primary mechanism. Commercial rates, who bears the M-Pesa fee, and the
collection mechanism are not yet decided.

Under direct merchant settlement (ADR-0008) the customer's money goes to the
merchant's own shortcode, so Axene can only collect automatically if the
provider officially supports allocating part of each payment to Axene.

### Evidence gathered (2026-10-13)

- No official Safaricom source describing split settlement or automatic
  third-party fee deduction for M-Pesa Express was found; the official
  developer portal was unreachable from the build environment (see
  [mpesa.md](../providers/mpesa.md) §0).
- Third-party guides (not authoritative) describe payment aggregators as
  settling to merchants 1–3 business days after collection, i.e. collecting
  centrally and paying out, and describe a Daraja B2B product that moves
  funds between shortcodes. None documents a provider-side fee split.

These facts are recorded as **unverified** and must be confirmed with
Safaricom before any option is selected.

## Options

| | A. Provider-supported split / fee allocation | B. Axene-controlled collection, then merchant settlement | C. Other official mechanism |
|---|---|---|---|
| Funds flow | Direct to merchant (ADR-0008 holds) | Axene's shortcode, then payouts | Depends |
| Automatic fee | Provider allocates fee to Axene | Axene retains fee, pays net | Depends |
| Needs | Confirmed Daraja product and agreement that supports it | Payout rail (e.g. B2C/B2B), merchant payables ledger, safeguarding, licensing/compliance approval | Documentation |
| Evidence of collection | Provider settlement record showing Axene's share | Axene's settlement record (gross) and payout record (net) | Provider record |
| Status | Unconfirmed | Changes funds flow; requires legal/compliance assessment | Unknown |

A fourth path, separate invoicing, remains possible as a fallback but is
**not** the intended mechanism and is not built.

## Decision

1. **Fee engine (built now):** versioned, effective-dated, merchant-specific
   fee schedules with percentage (parts per million) and fixed components,
   optional minimum and maximum, explicit rounding mode, fee payer, and
   provider-fee bearer. Integer arithmetic only; KES first. Rates are data
   entered by an operator; no rate is hard-coded and test fixtures are
   labelled as such.
2. **Fees accrue only on `SUCCEEDED`** payments, exactly once per payment
   (unique per payment, written in the same transaction as the transition).
   Failed, cancelled, expired, and duplicate attempts never accrue a fee.
3. **Fee lifecycle is explicit and evidence-based:**
   `accrued → collection_pending → collected`, with `collection_failed`,
   `waived`, `refunded`, and `review_required`. A fee is `collected` only
   when collection evidence (a provider or settlement record, reconciled) is
   recorded. Calculation or an internal deduction is never evidence.
4. **Collection mechanism is pluggable and off by default.** Each merchant's
   fee terms name a mechanism: `none_configured` (default), `provider_split`
   (option A), `axene_collection` (option B). Only `none_configured` can be
   selected today: `provider_split` requires a provider adapter declaring a
   verified fee-split capability (none does), and `axene_collection` requires
   the Axene-collection funds flow, which is not permitted until approved
   (settlement destinations may only be merchant-owned). Fees therefore
   accrue and remain visibly `accrued` (awaiting a mechanism) — the platform
   does **not** claim automatic collection works.
5. **Provider fees are separate data.** Recorded from provider settlement
   records when available, with the configured bearer (merchant — the
   provisional default —, Axene, or customer). Never assumed identical across
   products, shortcodes, or agreements.
6. **Customer-borne fees** (surcharges) are modelled but a schedule with
   `fee_payer = customer` or `provider_fee_bearer = customer` cannot be
   activated until the permitted flow and disclosure rules are approved.
7. **Reversal and refund fee policy** is a schedule setting:
   `refund_fee`, `retain_fee`, or `pending_approval` (default). With
   `pending_approval`, a reversal marks the fee `review_required` and opens a
   reconciliation exception instead of guessing.

## Consequences

- ADR-0008's direct-settlement model stays in force for payment processing,
  but it is now **conditional on option A being confirmed**. If Safaricom
  offers no supported split, automatic collection requires option B, which
  needs a new ADR, payouts, and compliance approval before activation.
- Accrued, uncollected fees accumulate as receivables in Axene's ledger book
  and are reported as such; they are revenue earned but not cash received.
- The collection mechanism is decision D-12c; until then the reconciliation
  path (settlement statements) is the only way fee collection evidence can
  be recorded.
