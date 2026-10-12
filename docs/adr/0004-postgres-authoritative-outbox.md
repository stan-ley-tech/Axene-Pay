# ADR-0004: PostgreSQL as authority; transactional outbox

- Status: Accepted
- Date: 2026-10-12

## Context

State changes must trigger asynchronous work (merchant webhooks, ledger-driven
jobs, status polling). Publishing to a broker after commit can be lost if the
process dies between commit and publish; publishing before commit can announce
a change that is then rolled back.

## Decision

- Every state change that other components must react to writes an
  `outbox_events` row **in the same transaction** as the change.
- A relay in the worker reads unpublished rows (`FOR UPDATE SKIP LOCKED`),
  publishes to RabbitMQ, and marks them published.
- Delivery is at-least-once. Every consumer is idempotent, keyed by event ID.
- Events carry IDs and minimal state, not sensitive payloads; consumers load
  current state from PostgreSQL when they need more.
- If RabbitMQ is unavailable, events accumulate in the outbox and drain on
  recovery. No financial state is lost.

## Consequences

- Exactly-once is not claimed anywhere in the system.
- Outbox table needs retention/archival (published rows pruned after a
  documented period; audit-relevant data lives elsewhere).
- Queue backlog and outbox age become key operational metrics.
- For low volume the relay could dispatch directly without RabbitMQ; we keep
  RabbitMQ as mandated so worker scaling does not depend on DB polling by
  every worker.
