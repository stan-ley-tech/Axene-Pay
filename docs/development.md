# Local Development

## Prerequisites

| Tool | Version | Needed for |
|------|---------|------------|
| Go | 1.27.x (see `go.mod`) | Backend |
| Docker + Compose v2 | recent | Local PostgreSQL/Redis/RabbitMQ, Testcontainers |
| GNU make | any | Convenience targets (optional; commands are plain `go`) |
| sqlc | pinned when Phase 1 adds queries | Regenerating DB code |
| goose | pinned when Phase 1 adds migrations | Running migrations |
| Node.js | 22 LTS or newer | Frontend (Phase 4) |

On Windows, run `make` targets from Git Bash or WSL, or run the underlying
commands directly (they are listed in the `Makefile`).

## First run

```sh
cp .env.example .env
make deps-up          # PostgreSQL :5432, Redis :6379, RabbitMQ :5672 (UI :15672)
make check            # gofmt check, go vet, unit tests
make run-api          # http://localhost:8080/healthz
```

Without Docker you can point `DATABASE_URL` at any PostgreSQL 16+ instance.

## Endpoints available today

| Path | Purpose |
|------|---------|
| `GET /healthz` | Liveness: process is up. Never touches dependencies. |
| `GET /readyz` | Readiness: PostgreSQL reachable within 2 s. Returns 503 otherwise. |

## Configuration

All configuration is via environment variables; see `.env.example` for the
full list and `internal/platform/config` for validation rules. The API
refuses to start with invalid configuration and reports every problem at
once.

## Tests

- `make test` — unit tests, no external dependencies.
- `make test-integration` — tests tagged `integration`, which need a real
  PostgreSQL (`TEST_DATABASE_URL`) or Docker for Testcontainers. None exist
  yet; they arrive with the Phase 1 schema.
