# ADR-0009: Provider callbacks are untrusted; outcomes require verification

- Status: Accepted
- Date: 2026-10-12
- Resolves: D-07

## Context

Daraja callbacks are believed to be unsigned (to be verified). Anyone who
learns the callback URL could post a forged "success". Callbacks can also be
duplicated, delayed, or never arrive.

## Decision

A callback is an **untrusted notification that something may have
happened**. It can never move a payment to a terminal state on its own.

Processing a callback:

1. Authenticate the transport as far as the provider allows (secret URL
   token; source-IP allow-list at the edge when Safaricom's ranges are
   confirmed).
2. Persist the raw callback **before** any interpretation (append-only,
   deduplicated by a provider-specific key).
3. Parse and correlate it to exactly one payment attempt. Unknown
   references are stored and alerted on; they never create payments.
4. If the payment is `PENDING_PROVIDER`, record the claimed outcome on the
   attempt and transition the payment to `PENDING_VERIFICATION`.
5. Respond 2xx to the provider once the callback is durably stored, whether
   or not verification has happened.

Verification (worker):

6. Perform an authenticated status query for the attempt.
7. Compare the response with the expected transaction (reference, amount,
   currency where available, outcome).
8. Only a matching, definitive status-query result (or an authorized
   reconciliation resolution) can move a payment to `SUCCEEDED` or
   `FAILED`. This is enforced in the domain: terminal transitions require
   *verified evidence*, and callbacks are not verified evidence.
9. The transition, any financial effect, and the outbox event commit in one
   transaction, guarded by a row lock and uniqueness constraints, so
   duplicates cannot produce a second effect or notification.

Failure handling:

- Status-query timeout or provider error → retry with backoff; the payment
  stays `PENDING_VERIFICATION`. A timeout is never a failure.
- Retries exhausted, or callback and status query disagree → `UNRESOLVED`
  plus a reconciliation exception. Only reconciliation/operators resolve it.
- No callback at all → the worker queries status on a schedule after
  submission; same rules apply.
- An uncertain attempt is never retried through another provider.

## If a product lacks a status query

The verification step depends on the provider declaring `status_query`
capability. If the enabled Daraja product does not support an adequate
status query, we will **not** substitute an invented mechanism. The
adapter will declare no `status_query` capability, payments will remain in
`PENDING_VERIFICATION` / `UNRESOLVED` until reconciliation against provider
statements resolves them, and the limitation will be recorded as a new ADR
with the business impact (delayed confirmation).

## Consequences

- Confirmed success has at least one extra provider round trip.
- Daraja status-query rate limits (unknown — external dependency) may cap
  throughput; if so, revisit with a new ADR rather than silently trusting
  callbacks.
