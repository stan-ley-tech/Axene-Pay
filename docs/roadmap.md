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
| OpenAPI skeleton with error envelope and conventions | ⬜ |
| CI runs format check, vet, tests, vulnerability scan | ⬜ |
| Local stack verified end-to-end on a machine with Docker | ⬜ (no Docker on the bootstrap machine) |

## Phase 1 — Core payment foundation ⬜

Acceptance criteria:

1. Migrations create merchants, applications, api_keys, payments,
   payment_attempts, payment_transitions, idempotency_keys, outbox_events.
2. API key authentication; keys hashed; isolation test proves merchant A
   cannot read merchant B's payment (404, not 403).
3. `POST /v1/payments` requires `Idempotency-Key`; replay returns identical
   response; conflicting reuse returns 422; concurrent duplicate requests
   produce exactly one payment (tested against real PostgreSQL).
4. `GET /v1/payments/{id}` returns current state and attempt summary.
5. State machine rejects every transition not in the documented graph
   (table-driven test covering all state pairs).
6. Provider interface + `testprovider` (sandbox-only, refuses to load in
   production) drive a payment to each terminal state, including `UNKNOWN`
   → resolved by status query.
7. Every API error matches the OpenAPI error schema (contract test).
8. Logs carry correlation ID; no secret appears in logs (test with a canary
   secret).

## Phase 2 — M-Pesa sandbox integration ⬜

Prerequisites: D-01, D-04, D-07 resolved; `docs/providers/mpesa.md`
verified against official docs.

1. STK Push submit, callback ingestion, STK Query resolution in sandbox.
2. Timeouts produce `UNKNOWN` and are resolved without resubmission.
3. Duplicate/out-of-order callbacks are no-ops; forged callbacks rejected.
4. Merchant webhooks signed, retried with backoff, dead-lettered, visible.
5. Integration tests against recorded sandbox fixtures + opt-in live
   sandbox test job.

## Phase 3 — Financial foundation ⬜

Prerequisites: D-02, D-12. Ledger, fees, refunds, reconciliation framework,
settlement records, financial invariant tests (balanced entries, no
duplicate postings, refund ≤ refundable, concurrency).

## Phase 4 — Merchant & operations applications ⬜

SvelteKit portals, RBAC, transaction investigation timeline, webhook
management, reconciliation exception workflow.

## Phase 5 — Additional capabilities ⬜

Card acquirer, more mobile money, bank rails, payouts, configurable routing,
provider performance analytics.

## Phase 6 — Production hardening ⬜

Security review, load and failure-injection tests, backup/restore drills,
DR plan, alerting, runbooks, compliance review, readiness assessment.
