# Known Limitations

What the platform does **not** do yet. Keep this current; a feature is not
claimed until it is implemented and verified.

## Payments and providers

- **No real payment rail.** The only provider is `testprovider`, a
  simulation that moves no money and is refused in production. M-Pesa
  Express is Phase 2 and blocked on external dependencies X-01…X-06
  ([decisions.md](decisions.md)).
- Callback responses are a generic `{"received": true}`. Provider-specific
  acknowledgement formats (e.g. what Daraja expects) arrive with each
  adapter.
- If a provider never returns its reference for a submission (timeout before
  response), the attempt can only be resolved by a status query keyed on
  Axene's attempt ID. Whether M-Pesa Express supports that is unknown
  (X-06); otherwise such attempts end `UNRESOLVED` for reconciliation.
- Only M-Pesa-style collection in KES is modelled (`payment_method.type =
  mpesa`). No cards, bank transfers, refunds, reversals, or payouts.
- No list/search endpoint for payments yet (`GET /v1/payments` with
  pagination).
- Verification timings are code defaults (see
  [development.md](development.md)), not yet tuned to real provider
  latency.

## Financial records

- **Automatic fee collection is not active.** The requirement is confirmed
  (ADR-0011), but no provider-supported mechanism is confirmed (D-12c,
  X-09). Fees accrue as receivables with status `accrued`; only
  `none_configured` (and `simulated`, in development) can be selected. Axene
  does not claim it can collect fees from direct-to-merchant payments.
- No commercial rates exist in the system until an operator creates and
  activates a schedule with an approval reference (D-12a).
- Customer-borne fees (surcharges) are modelled but cannot be activated
  until an approved flow exists.
- No invoicing (a possible fallback, not the intended mechanism), no
  merchant payouts, and no Axene-controlled collection (needs D-12c/d and
  compliance approval).
- No refunds: no provider adapter supports them yet. Reversals can only be
  recorded through reconciliation; reversal fee policy defaults to
  `pending_approval` (D-12e).
- Settlement statements are accepted only in Axene's normalized CSV format;
  converters for real provider statement formats arrive with each provider.
- Reconciliation is run and resolved through `axenectl` only (no operations
  portal until Phase 4) and is triggered manually, not scheduled.
- No financial reports beyond ledger balances, the trial balance query, and
  fee totals by status; no statements for merchants yet.
- Absorbed provider fees are recorded as owed to the merchant; how they are
  repaid is not modelled (D-12b).

## Notifications and events

- Webhook endpoints are managed by API only (no portal until Phase 4). No
  per-endpoint rate limiting or automatic disabling of persistently failing
  endpoints.
- Webhook signing-secret rotation is not exposed yet (create a new endpoint
  and disable the old one). `WEBHOOK_SECRETS_KEY` rotation has a version
  column but no re-encryption tooling.
- Published outbox rows, delivered webhook bodies, and attempt history are
  not yet archived or pruned (retention is D-09).

## Security and operations

- No rate limiting.
- Merchant setup actions in `axenectl` (merchants, destinations, provider
  configs, API keys) are not yet in `audit_log`; fee, settlement and
  reconciliation actions are, and payment state changes are in
  `payment_transitions`.
- One database role is used for migrations and runtime; append-only tables
  are protected by triggers. Separate migration/runtime roles with
  restricted grants come with the first deployed environment (D-05).
- No tracing (OpenTelemetry) yet; structured logs with request IDs and
  Prometheus metrics only. No dashboards or alert rules yet.
- `/readyz` checks PostgreSQL only. Redis is not used by the application.
- No cloud infrastructure or Terraform (blocked on D-05).
- No frontend applications yet (Phase 4).

## Verification environment

- The Docker compose stack has not been run on the bootstrap machine (no
  Docker). Local verification uses a native PostgreSQL 18; CI uses
  PostgreSQL 17 and RabbitMQ 4.1 service containers.
- RabbitMQ is not installed on the bootstrap machine: the worker has not
  been run end to end locally. The RabbitMQ publisher is tested in CI only.
- The race detector (`go test -race`) needs cgo and runs in CI only.
