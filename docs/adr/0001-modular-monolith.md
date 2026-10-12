# ADR-0001: Modular monolith with separate worker process

- Status: Accepted
- Date: 2026-10-12

## Context

Axene is greenfield with a small team and no production traffic. The core
correctness requirement — a payment state change, its ledger posting, and its
outbound event must be atomic — is trivially satisfied inside one database
transaction and very hard to satisfy across services.

## Decision

Build one Go codebase producing two binaries:

- `api` — merchant REST API and provider callback ingress.
- `worker` — outbox relay, provider status polling, webhook delivery,
  reconciliation and settlement jobs.

Both share `internal/` modules and one PostgreSQL database. Module boundaries
are enforced by package structure and code review (see
[domain-boundaries.md](../domain-boundaries.md)).

## Consequences

- Atomic multi-module writes without sagas or 2PC.
- One deploy pipeline, one migration stream.
- Risk: boundaries erode. Mitigation: modules own their tables; no
  cross-module SQL; dependency rules checked in review (a lint rule can be
  added later if erosion appears).
- Extracting a service later is possible along module lines, if a concrete
  scaling or isolation need arises.
