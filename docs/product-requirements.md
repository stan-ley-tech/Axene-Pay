# Product Requirements

Source: *Axene Payments — Master Project Definition* (2026-10-12). This file
condenses that document into testable requirements and records the
interpretation choices made. Where this file and the master definition
disagree, the master definition wins until this file is corrected.

## 1. Product

An Axene-owned payment infrastructure platform that lets businesses accept,
process, track, reconcile, and manage payments through one API. Axene controls
the gateway, orchestration, transaction lifecycle, financial records, merchant
integrations, reconciliation, and operational tooling. Underlying providers
(mobile money operators, card acquirers, banks) execute or authorize the
actual money movement.

## 2. Business objectives and how we will measure them

| # | Objective | Measured by (platform must record) |
|---|-----------|-------------------------------------|
| 1 | Reduce avoidable processing cost | Per-attempt provider fee (expected vs. reconciled), platform operating cost |
| 2 | Unified API across payment methods | One payment resource, method-specific details isolated in sub-objects |
| 3 | Multiple providers | Provider adapter registry with explicit capabilities |
| 4 | Configurable routing | Persisted routing decision + reason per payment attempt |
| 5 | Reliable internal financial records | Double-entry ledger, balanced and immutable |
| 6 | Automated reconciliation and settlement | Reconciliation runs with classified exceptions |
| 7 | Professional merchant platform | Merchant portal (Phase 4) |
| 8 | Full operational visibility | Payment timeline: attempts, callbacks, transitions, postings, notifications |
| 9 | Foundation for commercial services | Multi-merchant isolation, pricing/fee model per merchant |
| 10 | Add providers/methods/markets without rewrite | Adapter + capability model; currency-aware money type |

Owning the gateway does **not** automatically lower cost. Objective 1 is only
provable if objectives 4, 5 and 6 produce data that can be compared.

## 3. Domains in scope

Merchant management · Unified payment API · Orchestration/routing · Provider
integrations · Transaction lifecycle · Ledger · Reconciliation · Settlement ·
Webhooks/events · Refunds · Payouts · Risk · Reporting · Audit.

Each domain's responsibilities and boundaries are in
[domain-boundaries.md](domain-boundaries.md).

## 4. Non-negotiable behavioural rules

These are the rules from the master definition that most directly protect
money. Every one of them must be covered by a test once the relevant code
exists.

1. A payment is never reported as succeeded because a request was accepted or
   a provider acknowledged it. Only a verified final provider result (callback
   or status query) moves a payment to `SUCCEEDED`.
2. A timeout is an **unknown** outcome, never a failure.
3. A payment attempt with an unknown outcome is never retried through any
   provider until the original attempt is resolved.
4. A provider callback cannot overwrite a terminal payment state.
5. Payment, provider attempt, refund, settlement, and payout are separate
   records with separate lifecycles.
6. Ledger journal entries balance per currency, are immutable once posted, and
   are corrected only by reversing or adjusting entries.
7. Monetary amounts are integers in minor units. No floating point anywhere in
   the money path, including JSON.
8. Every operation that can create a financial effect is idempotent.
9. Reconciliation never edits records to make reports agree; corrections are
   explicit, authorized, and audited.
10. Payment success, provider settlement, and merchant payout are three
    different events.
11. Merchants are not live-enabled on registration; live processing requires
    an explicit approval state.
12. Authorization is enforced in the backend for every sensitive operation.

## 5. Out of scope until explicitly scheduled

- Machine-learning fraud scoring (rules first).
- Kafka, Kubernetes, microservices.
- Storing or transmitting raw card data (card integration will use an
  acquirer's hosted/tokenized flow; PCI scope still has to be assessed).
- Any production connection to a payment rail before Phase 6 readiness.

## 6. Users

| User | Interface | Phase |
|------|-----------|-------|
| Merchant developer | REST API, developer portal | 1 (API), 4 (portal) |
| Merchant operator | Merchant portal | 4 |
| Axene operations | Operations portal | 4 |
| Axene finance | Reconciliation / settlement views, reports | 3–4 |
| Payment provider | Callback endpoints | 2 |
