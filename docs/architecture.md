# System Architecture

## 1. Shape

A **modular monolith** in Go with two process types built from the same
codebase, backed by PostgreSQL as the authoritative store.

```
                   ┌────────────────────────────────────────────┐
 Merchant ──HTTPS──▶  api process (cmd/api)                      │
                   │   chi router → auth → idempotency → handler │
 Provider ──HTTPS──▶   /v1/*  merchant API                       │
 (callbacks)       │   /callbacks/{provider}  provider ingress   │
                   └──────────────┬─────────────────────────────┘
                                  │ single DB transaction:
                                  │ state change + ledger + outbox row
                   ┌──────────────▼─────────────────────────────┐
                   │            PostgreSQL (authoritative)      │
                   │ payments, attempts, ledger, outbox, audit  │
                   └──────────────┬─────────────────────────────┘
                                  │ outbox relay (polls, publishes)
                   ┌──────────────▼─────────────────────────────┐
                   │  worker process (cmd/worker)               │
                   │   status polling for unresolved attempts   │
                   │   merchant webhook delivery + retries      │
                   │   reconciliation / settlement jobs         │
                   └──────────────┬─────────────────────────────┘
                                  │
                       RabbitMQ (work distribution only)
                       Redis (rate limits, short-lived locks only)
```

## 2. Why this shape

- One deployable keeps transactions local: a payment state change, its ledger
  posting, and its outbound event commit atomically in one PostgreSQL
  transaction. No distributed transactions. See
  [ADR-0001](adr/0001-modular-monolith.md).
- The worker is separate because its work (polling providers, delivering
  webhooks, batch reconciliation) has different scaling, timeout, and failure
  characteristics from request handling.
- RabbitMQ distributes work; it never holds the only copy of financial state.
  If the broker loses a message, the outbox row and the attempt row still
  exist and the work is re-driven. See [ADR-0004](adr/0004-postgres-authoritative-outbox.md).
- Redis is optional infrastructure. Nothing that must survive a Redis flush
  may live only in Redis. Idempotency records live in PostgreSQL.

## 3. The five states that must not be conflated

| State | Owner module | Example values | Changes when |
|-------|--------------|----------------|--------------|
| Payment lifecycle | `payments` | `PROCESSING`, `SUCCEEDED` | Authoritative outcome is known |
| Provider execution | `providers` / attempts | `SENT`, `UNKNOWN`, `CONFIRMED_SUCCESS` | Adapter gets a provider response |
| Financial ledger | `ledger` | journal entries | A business event with financial effect is recorded |
| Settlement | `settlement` | `EXPECTED`, `SETTLED`, `DISCREPANT` | Provider/bank settlement data arrives |
| Merchant notification | `webhooks` | `PENDING`, `DELIVERED`, `DEAD` | Delivery attempts to merchant endpoint |

A payment can be `SUCCEEDED` while its settlement is `EXPECTED` and its
webhook is `RETRYING`. Each has its own table and state machine.

## 4. Request path for a payment (target design, Phase 1–2)

1. Authenticate API key → resolve merchant + environment (sandbox/live).
2. Idempotency check (`Idempotency-Key` header, scoped to merchant + route).
3. Validate request; risk pre-checks.
4. Persist `payment` (`CREATED`) and idempotency record in one transaction.
5. Orchestration selects provider; persist `payment_attempt` with routing
   decision and reason.
6. Adapter calls provider **outside** any DB transaction. Response mapped to
   provider execution state; timeout → `UNKNOWN`.
7. Persist attempt result + payment transition + outbox event in one
   transaction.
8. Final outcome arrives by callback or by worker status query; the same
   transition function applies, guarded by the state machine and a row lock.

Details: [payment-lifecycle.md](payment-lifecycle.md).

## 5. Cross-cutting concerns

| Concern | Approach |
|---------|----------|
| Correlation | `X-Request-Id` accepted or generated at the edge; propagated in logs, outbox events, provider attempts, webhooks |
| Logging | `log/slog` JSON; redaction of secrets/PII enforced at the logging boundary |
| Tracing/metrics | OpenTelemetry SDK; Prometheus scrape endpoint; Grafana dashboards (Phase 2+) |
| Config | Environment variables parsed once at startup into a typed struct; process refuses to start on invalid config |
| Errors | One JSON error envelope across the API (see `api-spec/openapi.yaml`) |
| Time | All timestamps UTC, `timestamptz` in DB, RFC 3339 in API |
| Money | `int64` minor units + ISO 4217 currency; see [ADR-0005](adr/0005-money-representation.md) |
| IDs | Prefixed, opaque string IDs in the API (e.g. `pay_…`); UUIDs internally |

## 6. Deployment (initial)

Managed containers (e.g. a cloud container service) running the `api` and
`worker` images, managed PostgreSQL with PITR backups, managed RabbitMQ or a
small broker instance, and a managed secret store. Terraform for all cloud
resources. Kubernetes is explicitly deferred. The cloud provider and region
are an open decision — see [decisions.md](decisions.md).
