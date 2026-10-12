# Threat Model

Status: **Initial (STRIDE-style), Phase 0.** Revisit at the end of each phase
and before production.

## 1. Assets

1. Merchant funds and the integrity of payment outcomes.
2. Ledger and audit records.
3. Provider credentials (Daraja consumer secret, passkey, initiator
   credential) — compromise allows initiating payouts/reversals.
4. Merchant API keys and webhook secrets.
5. Customer personal data (MSISDN, names returned by providers).
6. Operator accounts.

## 2. Trust boundaries

Internet ↔ edge/load balancer ↔ api process ↔ PostgreSQL / Redis / RabbitMQ ↔
worker ↔ provider APIs. Portals: browser ↔ SvelteKit server ↔ api.

## 3. Threats and planned controls

| # | Threat | STRIDE | Control | Phase |
|---|--------|--------|---------|-------|
| T1 | Forged provider callback marks unpaid payment as succeeded | Spoofing/Tampering | Unguessable callback path, IP allow-list, field matching against attempt, status-query confirmation per ADR-0009, reconciliation | 2 |
| T2 | Replayed callback double-credits merchant | Tampering | State machine no-op on duplicates; ledger unique `(source, reason)` | 1–3 |
| T3 | Merchant reads/modifies another merchant's data | Info disclosure / EoP | `merchant_id` in every query, isolation tests | 1 |
| T4 | Stolen API key used to create refunds/payouts | Spoofing | Key hashing, scopes, IP allow-list option, payout approvals & limits, anomaly alerts | 1, 5 |
| T5 | Merchant retry creates duplicate payment | Tampering | Mandatory idempotency keys (ADR-0007) | 1 |
| T6 | Concurrent refunds exceed captured amount | Tampering | Row lock on payment + refundable-amount check in same tx; DB check constraint | 3 |
| T7 | Insider modifies ledger/audit to hide fraud | Repudiation/Tampering | Append-only grants, separate migration role, audit log export to write-once storage | 3, 6 |
| T8 | Provider credential leak via logs or repo | Info disclosure | Secret store, redacting logger, secret scanning in CI | 0–1 |
| T9 | SSRF via merchant webhook URL | EoP | IP range blocking after DNS resolution, no redirects | 2 |
| T10 | Credential stuffing on portals | Spoofing | MFA, rate limits, lockout with backoff | 4 |
| T11 | CSRF on portal actions | Tampering | SameSite cookies + SvelteKit CSRF origin checks; confirmations for high-impact actions | 4 |
| T12 | DoS on payment API exhausts provider quotas | DoS | Per-key rate limits, provider-call concurrency limits, circuit breakers | 2 |
| T13 | Timeout misread as failure → merchant re-charges customer | Tampering (business) | `SUBMISSION_UNKNOWN` attempt state, no failover until resolved, clear API semantics | 1–2 |
| T14 | Sandbox credentials/config used in production | Tampering | Config validation binds endpoints to environment; testprovider barred in production | 0–1 |
| T15 | Compromised dependency | Tampering | Pinned versions, govulncheck, minimal deps | 0 |

## 4. Implementation status (end of Phase 1)

- Implemented and tested: T3 (merchant-scoped queries, isolation tests),
  T5 (idempotency keys, concurrency test), T13 (`SUBMISSION_UNKNOWN`,
  no resubmission, no failover while uncertain), T14 (simulated provider
  refused in production, deployed-environment config rules), T8 in part
  (secret types, route-pattern access logs, canary log tests), T2 in part
  (duplicate callbacks are inert; ledger uniqueness arrives in Phase 3), and
  T1 in part (callbacks never confirm a payment; secret path token).
- Not yet implemented: IP allow-listing (needs Safaricom ranges, X-05),
  rate limiting (T12), audit log of administrative actions (T7), payout
  controls (T4), portal controls (T10, T11), webhook SSRF controls (T9).

## 5. Accepted / unresolved risks (Phase 0)

- **R1**: Daraja callbacks may be unsigned (to verify). Mitigated, not
  eliminated, by T1 controls.
- **R2**: Hosting provider not chosen; network controls (private DB, WAF)
  undefined until D-05.
- **R3**: No security review performed yet; this model is the author's
  assessment only.
- **R4**: Regulatory data-residency requirements unknown (D-09).
