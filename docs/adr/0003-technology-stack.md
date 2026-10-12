# ADR-0003: Technology stack

- Status: Accepted
- Date: 2026-10-12

## Decision

Mandated by the master definition, recorded here so changes require a new ADR.

| Concern | Choice | Notes |
|---------|--------|-------|
| Backend language | Go (1.27.x at time of writing) | Version pinned in `go.mod` `toolchain` line |
| HTTP router | chi v5 | Only router-level dependency; handlers are `net/http` |
| Database | PostgreSQL (16+) | Authoritative store |
| DB access | pgx v5 + sqlc | No ORM |
| Migrations | goose (SQL files) | See ADR-0006 |
| API | REST/JSON, OpenAPI 3.1 | Spec in `api-spec/` is the contract |
| Logging | `log/slog` (stdlib) | JSON handler, redaction at boundary |
| Observability | OpenTelemetry, Prometheus, Grafana | Instrumentation added with first real workload |
| Cache/coordination | Redis, where needed | Never authoritative |
| Messaging | RabbitMQ | Work distribution; Kafka deferred |
| Frontend | SvelteKit, Svelte 5 runes, TypeScript strict, plain CSS + design tokens | No Tailwind, no Next.js |
| Frontend tests | Vitest, Playwright | |
| Containers | Docker | |
| CI/CD | GitHub Actions | |
| IaC | Terraform | |
| Secrets | Managed secret store or Vault | Choice depends on cloud decision |

## Dependency policy

- Prefer the standard library. A new third-party Go dependency needs a
  reason in the PR description.
- Versions are pinned (`go.sum`, `package-lock.json`); upgrades are
  deliberate PRs. `govulncheck` runs in CI.
- No dependency may be added to the money path (ledger, money, state machine)
  without review.
