# Axene Payments

Axene-owned payment gateway, payment orchestration, and financial operations
infrastructure.

> **Status: Phase 0 — project foundation.** No payment rail is integrated.
> Nothing in this repository processes real money. See
> [docs/roadmap.md](docs/roadmap.md) for what exists and what does not.

## What this platform will do

Let businesses accept, process, track, reconcile, and manage payments through
one developer-friendly API, while Axene controls routing, the transaction
lifecycle, the ledger, reconciliation, and settlement. M-Pesa (Safaricom Daraja)
is the proposed first integration.

## Repository layout

| Path                | Purpose                                                        |
| ------------------- | -------------------------------------------------------------- |
| `cmd/api`           | HTTP API process (merchant-facing REST API + provider callbacks) |
| `cmd/worker`        | Background worker process (outbox relay, status polling, webhooks) — *not yet created* |
| `internal/`         | Go domain modules (one package tree per bounded context)       |
| `migrations/`       | PostgreSQL schema migrations — *created in Phase 1*            |
| `api-spec/`         | OpenAPI contract (source of truth for the public API)          |
| `apps/`             | SvelteKit frontends — *created in Phase 4*                     |
| `deployments/`      | Local and cloud deployment configuration                       |
| `docs/`             | Architecture, ADRs, domain rules, security, runbooks           |

The layout is explained in [ADR-0002](docs/adr/0002-repository-layout.md).

## Getting started

See [docs/development.md](docs/development.md) for prerequisites and local
setup. Short version:

```sh
cp .env.example .env
docker compose -f deployments/local/docker-compose.yml up -d
make test
make run-api
```

## Documentation index

- [Product requirements](docs/product-requirements.md)
- [System architecture](docs/architecture.md)
- [Domain boundaries](docs/domain-boundaries.md)
- [Payment lifecycle](docs/payment-lifecycle.md)
- [Ledger and accounting rules](docs/ledger.md)
- [Provider integration contract](docs/provider-integration.md)
- [M-Pesa integration notes](docs/providers/mpesa.md)
- [Security architecture](docs/security.md) and [threat model](docs/threat-model.md)
- [Engineering conventions](docs/conventions.md)
- [Roadmap and acceptance criteria](docs/roadmap.md)
- [Open decisions](docs/decisions.md)
- [Known limitations](docs/known-limitations.md)
- [Architecture decision records](docs/adr/)
