# Payment Lifecycle

Status: **Accepted for Phase 1** (decision of 2026-10-12, see
[ADR-0009](adr/0009-callback-verification.md)). To be re-validated against
confirmed M-Pesa Express behaviour in Phase 2.

Payment status is an explicit, persisted state machine. It is never derived
from callback events.

## 1. Separate records, separate machines

| Record | Machine | Owner |
|--------|---------|-------|
| Payment | merchant-visible lifecycle (below) | `internal/payments` |
| Payment attempt | provider execution state (§3) | `internal/payments` (data), `internal/providers` (protocol) |
| Provider callback | append-only inbound notification, no state machine | `internal/payments` |
| Refund, reversal, settlement, payout | their own records (Phase 3+) | own modules |

A payment has 0..n attempts; at most one attempt may be unresolved at any
time.

## 2. Payment states

| State | Meaning | Terminal |
|-------|---------|----------|
| `CREATED` | Persisted; no provider request sent yet | no |
| `PENDING_PROVIDER` | A provider request was sent (or its sending is in doubt); waiting for the provider/customer | no |
| `PENDING_VERIFICATION` | A callback claims an outcome; Axene is confirming it with an authenticated status query | no |
| `UNRESOLVED` | Automatic resolution exhausted or contradictory evidence; a reconciliation exception is open | no |
| `SUCCEEDED` | Verified: the provider confirmed the funds were collected | yes* |
| `FAILED` | Verified: the provider confirmed the payment did not and will not complete, or it was rejected before any request was sent | yes |
| `CANCELLED` | Cancelled by the merchant before any provider request was sent | yes |
| `REVERSED` | A confirmed reversal undid a succeeded payment (reversal workflow is Phase 3+; the state exists so history is never rewritten) | yes |

\* `SUCCEEDED` can only move to `REVERSED`.

Candidate states from the master definition that were **not** adopted:

- `PENDING` / `PROCESSING` — replaced by the more precise `PENDING_PROVIDER`
  and `PENDING_VERIFICATION`.
- `REQUIRES_ACTION` — M-Pesa Express needs no merchant-driven step (the
  customer acts on their handset). Reintroduced when a flow needs it (cards,
  3-D Secure).
- `EXPIRED` — an M-Pesa prompt timing out is reported by the provider as a
  result code and is a verified `FAILED` with reason `customer_timeout`.
  Axene's own clock never fails a payment.

### Allowed transitions and required evidence

| From | To | Allowed evidence |
|------|----|------------------|
| `CREATED` | `PENDING_PROVIDER` | `submission` (request sent, accepted, or outcome unknown) |
| `CREATED` | `FAILED` | `submission_rejected` (provider's authenticated synchronous rejection), `routing` (no eligible provider) |
| `CREATED` | `CANCELLED` | `merchant` |
| `PENDING_PROVIDER` | `PENDING_VERIFICATION` | `callback` |
| `PENDING_PROVIDER` | `SUCCEEDED` / `FAILED` | `status_query` (worker polled without a callback), `reconciliation` |
| `PENDING_PROVIDER` | `UNRESOLVED` | `verification_exhausted` |
| `PENDING_VERIFICATION` | `SUCCEEDED` / `FAILED` | `status_query`, `reconciliation` |
| `PENDING_VERIFICATION` | `UNRESOLVED` | `verification_exhausted`, `evidence_conflict` |
| `UNRESOLVED` | `SUCCEEDED` / `FAILED` | `reconciliation` |
| `SUCCEEDED` | `REVERSED` | `reconciliation`, `reversal` (Phase 3+) |

Invariants enforced by `internal/payments` and tested exhaustively:

1. Any (from, to) pair not in the table is rejected.
2. A transition is rejected if its evidence kind is not listed for it. In
   particular **a callback can never produce a terminal state**.
3. Re-applying the current state is a no-op recorded as a duplicate, never
   an error and never a second side effect.
4. Terminal states are never left, except `SUCCEEDED → REVERSED`.

## 3. Attempt (provider execution) states

| State | Meaning |
|-------|---------|
| `PENDING_SUBMISSION` | Persisted; provider not called yet |
| `SUBMITTED` | Provider acknowledged receipt (not success) |
| `SUBMISSION_UNKNOWN` | Request may or may not have reached the provider (timeout, reset, 5xx after send) |
| `REJECTED` | Provider refused synchronously before execution |
| `CONFIRMED_SUCCESS` | Verified success |
| `CONFIRMED_FAILURE` | Verified failure |
| `UNRESOLVED` | Verification exhausted or contradictory |

Each attempt also stores: the routing decision and reason, the provider's
request/response references (e.g. checkout request ID), the outcome claimed
by the latest callback, verification attempt count, `next_verification_at`,
and the last provider result code/message (raw, for investigation).

## 4. Edge cases

| Situation | Behaviour |
|-----------|-----------|
| Provider request times out | Attempt `SUBMISSION_UNKNOWN`, payment `PENDING_PROVIDER`; worker resolves by status query. Never resubmitted, never failed over. |
| Status query times out / errors | Retry with exponential backoff; state unchanged. |
| Verification retries exhausted | `UNRESOLVED` + reconciliation exception `verification_exhausted`. |
| Callback claims success, status query says failure (or vice versa) | `UNRESOLVED` + exception `evidence_conflict`. |
| Status query amount/reference mismatch | `UNRESOLVED` + exception `evidence_mismatch`. |
| Duplicate callback | Persisted as a duplicate (dedup key), no transition, no effects. |
| Late callback for a terminal payment, agreeing | Recorded; no-op. |
| Late callback contradicting a terminal payment | Recorded; exception `late_callback_conflict`; terminal state unchanged. |
| Callback for unknown reference | Recorded unmatched; exception `unmatched_callback`; nothing else. |
| Merchant cancels after a provider request was sent | Rejected (`409 payment_not_cancellable`): STK cannot be withdrawn once sent. |
| Process crashes after creating payment, before calling provider | Worker submits stale `PENDING_SUBMISSION` attempts; the attempt ID is the provider-side idempotency reference where supported. |

## 5. Concurrency and atomicity

Every transition — from the API, callback ingestion, or the worker — goes
through one function that, in a single database transaction:

1. locks the payment row (`SELECT … FOR UPDATE`),
2. validates the transition and evidence,
3. updates the payment and attempt,
4. appends a `payment_transitions` row (from, to, evidence, actor,
   correlation ID, reference to the callback or status query),
5. writes any financial effect (Phase 3) and an `outbox_events` row.

Provider network calls never happen inside a transaction.

## 6. Operational alerts

- Attempts in `SUBMISSION_UNKNOWN` or `PENDING_VERIFICATION` older than the
  configured threshold.
- Any payment entering `UNRESOLVED`.
- Any `late_callback_conflict` or `unmatched_callback` exception.
