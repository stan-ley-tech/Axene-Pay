# Security Architecture

Status: **Baseline.** A dedicated security assessment is required before
production (Phase 6). This document records controls designed in from the
start and the assumptions behind them.

## 1. Identity and access

| Actor | Authentication | Authorization |
|-------|----------------|---------------|
| Merchant API client | Secret API key (`sk_test_…` / `sk_live_…`) over TLS, `Authorization: Bearer` | Key bound to one merchant + environment; optional scopes (Phase 4) |
| Merchant portal user | Session (HttpOnly, Secure, SameSite=Lax cookie) after password + MFA (MFA mandatory for live access) | Role per merchant: owner, admin, developer, viewer, finance |
| Axene operator | SSO with MFA (provider TBD, decision D-06) | Fine-grained roles; high-impact actions require reason + (where configured) second approver |
| Payment provider | Per-provider: callback URL secret, IP allow-list, signatures where offered | Callback can only affect attempts it references |
| Worker / services | Workload identity from the hosting platform; DB roles per process | Least privilege |

API keys:

- Generated with `crypto/rand`, ≥ 32 bytes entropy, prefixed for type and
  environment, shown once.
- Stored as a keyed hash (HMAC-SHA-256 with a server-side pepper from the
  secret store) plus a short non-secret prefix for lookup and display.
- Rotation: multiple active keys per merchant; revocation is immediate.
- Last-used timestamp and IP recorded (coarsely, for audit).

Passwords: argon2id via `golang.org/x/crypto/argon2` with documented
parameters. No custom cryptography.

## 2. Merchant isolation

- Every merchant-owned table carries `merchant_id`; every query in a
  merchant context filters by it. sqlc queries take `merchant_id` as a
  required parameter — there are no "get by ID" queries without it on
  merchant-facing paths.
- Resource IDs are unguessable but **authorization never relies on that**.
- Sandbox and live data are separated by an `environment` column at minimum;
  physical separation (separate DBs) is an open question (D-08).
- Cross-merchant access tests are part of the security test suite.

## 3. Transport and edge

- TLS 1.2+ everywhere external; HSTS on portals.
- Rate limiting per API key and per IP at the edge and in-app (Redis token
  bucket); stricter limits on auth endpoints.
- Request body size limits; strict JSON decoding (unknown fields rejected on
  financial endpoints).
- CORS: API is server-to-server; no wildcard CORS. Portals talk to their own
  SvelteKit server, which calls the API.

## 4. Secrets

- No secrets in the repository, images, or client-side code.
  `.env` is git-ignored; `.env.example` holds placeholders only.
- Runtime secrets come from the managed secret store (D-05), injected as
  env vars or fetched at startup; rotation procedures per secret type in
  runbooks.
- Provider credentials are scoped per environment; sandbox credentials can
  never authenticate to production endpoints because endpoints are bound to
  environment in config validation.

## 5. Data protection and logging

- Logs never contain: API keys, tokens, passwords, provider credentials,
  full card data, full MSISDNs (log masked `2547****1234`), full request
  bodies of financial endpoints.
- Redaction is enforced by a `slog` handler wrapper plus typed `Secret`
  values whose `LogValue` / `String` return `[REDACTED]`.
- Metric labels never contain personal or payment data.
- Encryption at rest via managed database/storage encryption; application-
  level encryption for especially sensitive fields (e.g. provider security
  credentials) using envelope encryption with a KMS.
- Retention periods for PII and financial records: decision D-09 (depends on
  Kenyan Data Protection Act obligations and financial record-keeping rules).

## 6. Integrity of financial and audit records

- Append-only tables (`journal_lines`, `audit_log`, `payment_transitions`):
  application DB role has `INSERT`/`SELECT` only.
- Audit entries record actor, action, target, before/after (where relevant),
  reason, IP, user agent, correlation ID, timestamp.
- Database backups are encrypted, access-controlled, and restore-tested
  (Phase 6).

## 7. Webhooks to merchants

- Signed with HMAC-SHA-256 over `timestamp.body` using a per-endpoint secret;
  header carries timestamp and signature; merchants reject stale timestamps
  (replay protection).
- Endpoints must be HTTPS; SSRF protection: resolve and block private,
  loopback, link-local, and metadata IP ranges; no redirects followed.

## 8. Card payments (future)

Use the acquirer's hosted fields / redirect so raw PAN never touches Axene
systems. This **reduces but does not remove** PCI DSS obligations (SAQ type
must be determined with the acquirer/QSA).

## 9. Supply chain and CI

- `govulncheck` and `npm audit` in CI; dependency updates via reviewed PRs.
- GitHub Actions pinned by commit SHA (to be applied when workflows touch
  secrets); least-privilege `GITHUB_TOKEN` permissions.
- Branch protection on `main` (configure in GitHub — recorded as an action
  item, not enforceable from the repo).

## 10. Unresolved security risks

See [threat-model.md](threat-model.md) §4 and [decisions.md](decisions.md).
