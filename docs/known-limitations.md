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

- **No ledger yet** (Phase 3). Payment success, fees, and settlement are not
  posted to a double-entry ledger; the design is in [ledger.md](ledger.md).
- No fee schedules (D-12), settlement records, or statement import.
- Reconciliation exceptions are recorded and counted, but there is no
  workflow, API, or UI to investigate and resolve them yet. `UNRESOLVED`
  payments stay unresolved until that exists (Phase 3/4).

## Notifications and events

- No merchant webhooks (Phase 2). Outbox events are published to the
  `axene.events` exchange, but no queues are bound to it yet, so the broker
  discards them after confirming. The authoritative copy stays in
  `outbox_events`.
- Published outbox rows are not yet archived or pruned.

## Security and operations

- No rate limiting.
- Administrative actions (`axenectl`) are not written to an audit log yet;
  payment state changes are (`payment_transitions`).
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
