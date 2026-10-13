# Roadmap and Acceptance Criteria

A phase is complete only when every acceptance criterion is met and verified.
Status values: ✅ done · 🟡 in progress · ⬜ not started.

## Phase 0 — Repository assessment and foundation 🟡

| Criterion | Status |
|-----------|--------|
| Repository inspected; environment gaps recorded | ✅ |
| Architecture, domain boundaries, ADRs, lifecycle, ledger, provider, security, threat model documented | ✅ |
| Open decisions listed with blocking/deferrable split | ✅ |
| Local dependencies defined (PostgreSQL, Redis, RabbitMQ via docker compose) | ✅ |
| Go module builds; `api` serves `/healthz` and `/readyz` with validated config, structured logging, correlation IDs | ✅ |
| Money type with exhaustive unit tests | ✅ |
| OpenAPI skeleton with error envelope and conventions | ✅ |
| CI runs format check, vet, tests, vulnerability scan | ✅ |
| Local stack verified end-to-end on a machine with Docker | ⬜ (no Docker on the bootstrap machine; PostgreSQL verified with a native install, RabbitMQ in CI) |

## Phase 1 — Core payment foundation ✅

Built against the clearly labelled `testprovider`; no real provider traffic.
Decisions: [ADR-0008](adr/0008-direct-merchant-settlement.md),
[ADR-0009](adr/0009-callback-verification.md),
[payment-lifecycle.md](payment-lifecycle.md).

Acceptance criteria (all verified locally against PostgreSQL and in CI; see
[known-limitations.md](known-limitations.md) for what Phase 1 does not do):

1. ✅ Migrations create merchants, settlement destinations, merchant provider
   configurations, API keys, idempotency keys, payments, payment attempts,
   payment transitions, provider callbacks, outbox events, and
   reconciliation exceptions. Append-only tables reject `UPDATE`/`DELETE`.
2. ✅ API key authentication with hashed keys; live keys only for approved
   merchants; isolation test proves merchant A gets `404` for merchant B's
   payment.
3. ✅ `POST /v1/payments` requires `Idempotency-Key`; replay returns the
   identical response; conflicting reuse returns `422`; concurrent duplicate
   requests produce exactly one payment (real PostgreSQL).
4. ✅ `GET /v1/payments/{id}` returns state, settlement destination, and
   attempt summary; `POST /v1/payments/{id}/cancel` only succeeds from
   `CREATED`.
5. ✅ State machine rejects every (from, to, evidence) combination not in
   the documented table; callbacks can never produce terminal states
   (exhaustive table-driven test).
6. ✅ Routing is deterministic from merchant provider configuration and
   records the selected provider and reason. *Refined:* when no provider is
   eligible the request is rejected with `422 no_eligible_provider` and no
   payment is created, because nothing has happened financially; the
   `CREATED → FAILED (routing)` transition remains for providers that become
   unavailable between creation and submission.
7. ✅ Callback ingestion stores the raw callback before interpretation,
   deduplicates, correlates, and moves the payment to
   `PENDING_VERIFICATION`; unknown references create an `unmatched_callback`
   exception.
8. ✅ Worker verifies by status query with backoff: success/failure only on a
   matching definitive result; timeouts never fail a payment; exhaustion or
   conflict → `UNRESOLVED` + reconciliation exception; payments with no
   callback are polled; stale `PENDING_SUBMISSION` attempts are submitted.
9. ✅ Each transition writes exactly one transition row and one outbox event,
   even under duplicate callbacks and concurrent processing (tested).
10. ✅ Outbox relay publishes to RabbitMQ at least once and marks events
    published (integration-tested in CI with a RabbitMQ service).
11. ✅ Every API response and error matches the OpenAPI contract (contract
    test).
12. ✅ Logs carry correlation IDs; a canary secret never appears in logs;
    Prometheus metrics exposed for requests, transitions, provider calls,
    verification, and outbox backlog.
13. ✅ Admin CLI creates merchants, settlement destinations, provider
    configurations, and API keys (key printed once).

## Phase 2 — Notifications and M-Pesa Express sandbox 🟡

### 2a. Merchant webhooks (no external dependency) ✅

Design: [ADR-0010](adr/0010-merchant-webhooks.md).

1. ✅ Merchants manage webhook endpoints by API; signing secrets are shown
   once and stored encrypted.
2. ✅ Every payment event is fanned out exactly once per subscribed endpoint,
   via RabbitMQ and a database sweep that covers lost messages.
3. ✅ Deliveries are signed (timestamped HMAC), retried with exponential
   backoff, marked dead after the maximum attempts, and replayable.
4. ✅ Delivery history (every attempt) is visible to the merchant by API.
5. ✅ The delivery client refuses private, loopback, link-local and metadata
   addresses and follows no redirects.

### 2b. M-Pesa Express adapter ⬜ (blocked)

Prerequisites: external dependencies X-01…X-06 ([decisions.md](decisions.md))
and the official Daraja definitions confirmed (see
[mpesa.md](providers/mpesa.md) §0).

1. M-Pesa adapter: OAuth token caching, STK Push submit, callback parsing,
   STK Query verification, result-code mapping from official docs.
2. Provider request/response and correlation identifiers persisted.
3. Environment-specific configuration (local fake, sandbox, production);
   credentials only from the secret store.
4. Tests against recorded sandbox fixtures plus an opt-in live sandbox job.
5. Not production-ready until tested against the real Daraja environment.

## Phase 3 — Financial foundation 🟡

Decisions: [ADR-0011](adr/0011-fees-and-automatic-collection.md),
[ledger.md](ledger.md). Rates and the collection mechanism stay configurable
(D-12a–g).

1. ⬜ Ledger primitives: accounts, balanced immutable journal entries
   (enforced at commit), idempotent posting by source and reason,
   compensating reversals, derived balances.
2. ⬜ Fee schedules: merchant-specific, versioned, effective-dated,
   percentage (ppm) + fixed, min/max, rounding mode, fee payer,
   provider-fee bearer, reversal policy; immutable once active; operator CLI.
3. ⬜ Fee calculation is integer-exact and tested across rounding modes,
   caps, and boundaries.
4. ⬜ Fees accrue exactly once when a payment succeeds, with ledger postings
   in the same transaction; no schedule → exception, never a silent zero.
5. ⬜ Fee lifecycle with evidence-based collection; collection mechanism
   pluggable, `none_configured` by default, unverified mechanisms cannot be
   enabled.
6. ⬜ Settlement statements import (normalized format) into settlement
   batches and lines; settlement and provider-fee postings.
7. ⬜ Reconciliation runs classify matches and discrepancies; operators
   resolve exceptions (including `UNRESOLVED` payments) with an audited
   reason; reversals apply the configured fee policy.
8. ⬜ Payment API shows the accrued Axene fee and its collection status.

Not in Phase 3: refunds through providers (no provider supports them yet),
invoicing (fallback only), payouts and Axene-controlled collection (need
D-12c/d and compliance approval).

## Phase 4 — Merchant & operations applications ⬜

SvelteKit portals, RBAC, transaction investigation timeline, webhook
management, reconciliation exception workflow.

## Phase 5 — Additional capabilities ⬜

Card acquirer, more mobile money, bank rails, payouts, configurable routing,
provider performance analytics.

## Phase 6 — Production hardening ⬜

Security review, load and failure-injection tests, backup/restore drills,
DR plan, alerting, runbooks, compliance review, readiness assessment.
