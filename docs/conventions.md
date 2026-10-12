# Engineering Conventions

## Workflow

For each change: understand → inspect → plan → acceptance criteria →
smallest complete increment → tests → `make check` → review failure cases
and security → update docs → report what changed, what was tested, what
remains.

- One concern per commit/PR. Commits are small and reviewable.
- Commit messages: Conventional Commits (`feat:`, `fix:`, `docs:`, `chore:`,
  `test:`, `refactor:`, `ci:`), imperative mood, body explains *why*.
- Never skip or delete a failing test to get green; never suppress errors to
  make a build pass.
- No placeholder financial logic presented as complete. Unfinished paths
  return an explicit `not implemented` error and are listed in
  [known-limitations.md](known-limitations.md).

## Go

- `gofmt`/`go vet` clean; `staticcheck` when added to CI.
- Errors: wrap with `%w` and context; sentinel errors for domain conditions
  (`payments.ErrInvalidTransition`); never `panic` for expected conditions.
- `context.Context` is the first parameter of anything doing I/O.
- No package-level mutable state; dependencies passed explicitly in
  constructors. `cmd/*/main.go` is the only composition root.
- Interfaces are declared by the consumer, kept small, and only where a real
  boundary exists (provider adapters, clock, ID generation for tests).
- Time comes from an injected clock in domain code so tests are
  deterministic.
- Tests: table-driven where it helps; `t.Parallel()` for pure tests;
  integration tests guarded by build tag `integration` and real PostgreSQL.
- Money: only via `internal/platform/money`. A lint/grep check for
  `float32`/`float64` in money-path packages will be added to CI.

## SQL

- Migrations are append-only once merged; never edit a merged migration.
- Every table: `id`, `created_at timestamptz not null default now()`;
  mutable tables add `updated_at`.
- Constraints in the schema: `NOT NULL`, `CHECK`, `UNIQUE`, foreign keys.
  Application validation is in addition to, not instead of, constraints.
- Use explicit transactions with `SELECT … FOR UPDATE` for state changes.

## API

- Paths: plural nouns, snake_case JSON fields, prefixed opaque IDs.
- Every response carries `X-Request-Id`.
- Errors use the envelope in `api-spec/openapi.yaml`:
  `{"error": {"type", "code", "message", "param?", "request_id"}}`.
- Backward compatibility within `/v1`: additive changes only (new optional
  request fields, new response fields, new enum values documented as
  possible). Breaking changes require `/v2`.
- Cursor-based pagination (`limit`, `starting_after`), max limit 100.

## Frontend (Phase 4)

SvelteKit + Svelte 5 runes, TypeScript strict, plain CSS with tokens in a
shared package, no Tailwind, no global stores unless justified, typed API
client generated from OpenAPI, Vitest + Playwright.

## Configuration

- All config from environment variables, validated at startup; invalid
  config is a fatal startup error with a clear message.
- `APP_ENV` ∈ {`development`, `test`, `sandbox`, `production`}. Production
  forbids insecure defaults (e.g. test providers, plaintext DB connections).
- No production values in the repository.
