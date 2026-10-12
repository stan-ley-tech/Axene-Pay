# Known Limitations

What the platform does **not** do yet. Keep this current; a feature is not
claimed until it is implemented and verified.

## Current (Phase 0)

- No payment processing of any kind. No provider is integrated, real or
  simulated.
- No database schema or migrations yet (Phase 1).
- No authentication; the only HTTP endpoints are `/healthz` and `/readyz`.
- `/readyz` checks PostgreSQL connectivity only; Redis and RabbitMQ are not
  yet used by the application and therefore not checked.
- No worker process yet.
- No frontend applications yet.
- No cloud infrastructure or Terraform (blocked on D-05).
- Observability: structured logs and request IDs only; no tracing or
  metrics exporter yet.
- Local stack (`deployments/local/docker-compose.yml`) has not been run on
  the bootstrap machine because Docker is not installed there.
- CI workflow has not yet run on GitHub.
