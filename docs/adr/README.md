# Architecture Decision Records

One file per decision, numbered, never renumbered. A superseded ADR stays in
place with its status changed to `Superseded by ADR-NNNN`.

Template:

```markdown
# ADR-NNNN: Title

- Status: Proposed | Accepted | Superseded by ADR-NNNN
- Date: YYYY-MM-DD

## Context
## Decision
## Consequences
```

| ADR | Title | Status |
|-----|-------|--------|
| [0001](0001-modular-monolith.md) | Modular monolith with separate worker process | Accepted |
| [0002](0002-repository-layout.md) | Repository layout | Accepted |
| [0003](0003-technology-stack.md) | Technology stack | Accepted |
| [0004](0004-postgres-authoritative-outbox.md) | PostgreSQL as authority; transactional outbox | Accepted |
| [0005](0005-money-representation.md) | Money representation | Accepted |
| [0006](0006-database-access-and-migrations.md) | Database access and migrations | Accepted |
| [0007](0007-idempotency.md) | Idempotency for financial operations | Accepted |
| [0008](0008-direct-merchant-settlement.md) | Direct merchant settlement as the initial funds flow | Accepted (conditional) |
| [0009](0009-callback-verification.md) | Provider callbacks are untrusted; outcomes require verification | Accepted |
| [0010](0010-merchant-webhooks.md) | Merchant webhooks | Accepted |
