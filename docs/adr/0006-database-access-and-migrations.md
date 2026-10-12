# ADR-0006: Database access and migrations

- Status: Accepted
- Date: 2026-10-12

## Decision

- **pgx v5** connection pool (`pgxpool`); no `database/sql` layer.
- **sqlc** generates typed Go from hand-written SQL. Generated code is
  committed so builds do not require sqlc. *Amended 2026-10-12:* queries
  live in `internal/store/queries`, one file per owning module, generating
  a single `store` package, because transactions that must span modules
  (transition + outbox event) need one `Queries` type. CI rebuilds sqlc
  from source and fails if the committed code is stale.
- **goose** SQL migrations in `migrations/`, numbered sequentially
  (`00001_create_merchants.sql`). sqlc reads the same directory as its schema
  source, so there is one schema definition.
- Migrations are forward-only in production. `-- +goose Down` sections are
  written for local development but never run against production data.
- Migrations run as a separate step (CLI / CI job) with a migration role,
  never automatically on API start.
- Application roles: the runtime role has no DDL rights and no `UPDATE` /
  `DELETE` on append-only tables (`journal_lines`, `audit_log`,
  `payment_transitions`). Enforced with grants and, where useful, triggers.
- Integration tests run against real PostgreSQL (Testcontainers when Docker is
  available; a `TEST_DATABASE_URL` otherwise). Mocks of the database are not
  used to test SQL behaviour.

## Consequences

- SQL is reviewed as SQL; constraints and uniqueness live in the schema, not
  only in Go.
- Two tools (sqlc, goose) required for schema work; documented in
  `docs/development.md`.
