# ADR-0008: Direct merchant settlement as the initial funds flow

- Status: Accepted (conditional — must be confirmed before production, see below)
- Date: 2026-10-12
- Resolves: D-02 (provisionally)

## Context

The funds-flow model decides what Axene's ledger records, whether Axene holds
money it owes to merchants, whether payouts exist, and which licences apply.
Two models were documented in [ledger.md](../ledger.md): technical gateway
(merchant's own shortcode) and aggregator (Axene's shortcode + payouts).

## Decision

Use **direct merchant settlement** initially:

```
Customer ──pays──▶ M-Pesa (provider) ──settles──▶ Merchant's own shortcode
                ▲
   Axene initiates the request, tracks the lifecycle, verifies the outcome,
   records fees and expected settlement — but never holds the funds.
```

1. Every merchant has explicit **settlement destinations** (e.g. an M-Pesa
   Paybill or Till number) per environment, stored as configuration data,
   not code.
2. Every payment records the merchant **and** the settlement destination it
   was initiated against. A later change to merchant configuration never
   rewrites history.
3. Axene does **not** maintain merchant balances, perform payouts, or hold
   customer funds in an Axene-controlled shortcode in the initial flow.
4. The ledger (Phase 3) separates Axene's own financial positions (fees it
   is owed, provider costs it bears) from a memorandum record of funds that
   flow between customer, provider and merchant. See [ledger.md](../ledger.md).
5. Extensibility toward centralized collection is preserved by:
   - modelling the settlement destination as a typed, owned entity
     (`owner = merchant` today; `owner = axene` is a future value),
   - keeping ledger postings driven by a funds-flow policy selected per
     settlement destination, so a future aggregator policy adds postings
     without changing the payment domain,
   - keeping payouts as a separate, unimplemented module boundary.

## External dependencies (not assumed)

- Whether the enabled Daraja product(s) can initiate STK Push against a
  merchant-owned shortcode under Axene's credentials, or whether each
  merchant must supply its own Daraja credentials/passkey.
- Merchant agreements authorizing Axene to initiate requests for their
  shortcode.
- Regulatory/compliance confirmation that this model fits Axene's
  authorization.

Until these are confirmed in writing (recorded in
[decisions.md](../decisions.md)), the model is used for design and sandbox
work only.

## Consequences

- No merchant-payable liability and no payout workflow in Phases 1–3.
- Axene's fee collection cannot be "deducted at source" because Axene never
  holds the funds; how fees are collected (invoicing, separate charge) is
  business decision D-12.
- Provider fees deducted by Safaricom from the merchant are recorded in the
  memorandum book for reconciliation, not as Axene expense, unless D-12
  says Axene bears them.
