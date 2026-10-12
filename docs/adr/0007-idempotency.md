# ADR-0007: Idempotency for financial operations

- Status: Accepted
- Date: 2026-10-12

## Context

Merchants retry on timeouts. Without idempotency a retry can create a second
payment, refund, or payout.

## Decision

- Every `POST` that can create a financial effect **requires** an
  `Idempotency-Key` header (1–255 printable ASCII chars). Missing key →
  `400 idempotency_key_required`.
- Scope: `(merchant_id, environment, key)`. Keys are not shared across
  merchants or between sandbox and live.
- Stored in PostgreSQL (`idempotency_keys`) with: request method + path, a
  SHA-256 fingerprint of the canonicalized request body, status
  (`in_progress` / `completed`), response status code and body, created/
  expires timestamps.
- Behaviour:
  - New key → insert `in_progress` row in the same transaction that creates
    the resource; on completion store the response.
  - Same key, same fingerprint, completed → replay the stored response
    byte-for-byte with header `Idempotent-Replayed: true`.
  - Same key, same fingerprint, in progress → `409 idempotency_in_progress`
    (client retries later).
  - Same key, different fingerprint or path → `422 idempotency_key_reused`.
- Retention: at least 24 hours; exact period to be confirmed (see
  decisions.md). Expired keys are pruned by the worker.
- Internally, provider calls carry our attempt ID as the provider-side
  reference where the provider supports it, and ledger postings carry a
  unique `(source_type, source_id, posting_reason)` constraint so a replayed
  event cannot double-post.

## Amendment (2026-10-12)

If the request that owns a key dies after creating its resource but before
storing the response, the key would otherwise answer `409` until it
expires. Keys therefore record the created resource's ID; an in-progress
key older than the recovery window (longer than a provider call) is answered
with the resource's current state. Requests rejected before any side effect
(validation, no eligible provider) store no key.

## Consequences

- Idempotency correctness depends on DB uniqueness, not on Redis or locks.
- Responses for errors that happen *before* any side effect (validation) are
  not stored, so a corrected request with the same key can proceed.
