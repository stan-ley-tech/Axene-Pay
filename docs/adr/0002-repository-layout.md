# ADR-0002: Repository layout

- Status: Accepted
- Date: 2026-10-12

## Context

The master definition proposes `apps/api` and `apps/worker` alongside the
frontend apps. Go tooling works best with a single module whose binaries live
under `cmd/` and whose private code lives under `internal/` (the compiler
enforces that `internal/` is not importable from outside the module).

## Decision

```
go.mod                 single Go module: github.com/stan-ley-tech/axene-pay
cmd/api/               api binary (main package only — wiring, no logic)
cmd/worker/            worker binary (added when first background job exists)
internal/<module>/     domain modules
internal/platform/     config, logging, HTTP middleware, db, money, ids
migrations/            SQL migrations (goose format)
api-spec/              OpenAPI 3.1 contract
apps/<portal>/         SvelteKit apps (Phase 4), each its own package.json
packages/              shared TS packages (design tokens, typed API client) — Phase 4
deployments/local/     docker-compose for local dependencies
deployments/terraform/ cloud infrastructure — when target cloud is decided
docs/                  documentation and ADRs
```

Directories are created when they receive real content, not in advance.

## Consequences

- `go build ./...` and `go test ./...` work from the repository root.
- Frontend apps are independent npm projects; an npm workspace can be added
  once there are two apps sharing packages.
- Deviation from the master definition's proposed `apps/api` is deliberate
  and limited to Go binaries.
