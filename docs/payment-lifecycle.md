# Payment Lifecycle

Status: **Proposed** — to be finalized in Phase 1 and validated against
confirmed M-Pesa behaviour in Phase 2. The master definition lists candidate
states; below is the reduced model and the reasoning.

## 1. Three separate state machines

1. **Payment** — what the merchant sees. One per merchant payment intent.
2. **Payment attempt** — one per provider submission. A payment has 0..n
   attempts; at most one may be non-terminal at any time.
3. **Refund**, **payout**, **settlement** — their own machines (Phase 3/5).

## 2. Payment states

| State | Meaning | Terminal |
|-------|---------|----------|
| `CREATED` | Accepted and persisted; not yet submitted to a provider | no |
| `PROCESSING` | An attempt exists that has been sent, or whose outcome is not yet known | no |
| `REQUIRES_ACTION` | Merchant/customer must perform a step we cannot drive (redirect, 3DS). **Not used by M-Pesa STK Push**; reserved for card flows | no |
| `SUCCEEDED` | Provider has confirmed the funds were collected (verified callback or status query) | yes |
| `FAILED` | Provider has confirmed the payment did not and will not complete | yes |
| `CANCELLED` | Cancelled by merchant **before** any attempt was submitted | yes |
| `EXPIRED` | The payment's validity window passed and the provider has confirmed no successful completion | yes |

Dropped from the candidate list: `PENDING`. It is indistinguishable from
`PROCESSING` from the merchant's point of view in every flow we have
identified; two names for one state invites inconsistent handling.

### Allowed transitions

```
CREATED ──submit──▶ PROCESSING ──confirmed success──▶ SUCCEEDED
   │                    │  ├──confirmed failure─────▶ FAILED
   │                    │  ├──window passed + confirmed not paid──▶ EXPIRED
   │                    │  └──needs action──▶ REQUIRES_ACTION ──▶ PROCESSING
   │                    └──attempt failed definitively, safe failover──▶ PROCESSING (new attempt)
   ├──cancel──▶ CANCELLED
   └──routing found no eligible provider──▶ FAILED
```

Any transition not in this graph is rejected by the backend. Re-applying the
current state (e.g. duplicate success callback on `SUCCEEDED`) is a no-op,
recorded as a duplicate, not an error.

## 3. Attempt (provider execution) states

| State | Meaning |
|-------|---------|
| `PENDING_SUBMISSION` | Row persisted, provider not yet called |
| `SUBMITTED` | Provider acknowledged receipt (not success) |
| `UNKNOWN` | We cannot tell whether the provider received/executed the request (timeout, connection reset, 5xx after send) |
| `CONFIRMED_SUCCESS` | Provider confirmed completion |
| `CONFIRMED_FAILURE` | Provider confirmed it did not complete |

Rules:

- A timeout or ambiguous error moves the attempt to `UNKNOWN`, never
  `CONFIRMED_FAILURE`. The payment stays `PROCESSING`.
- `UNKNOWN` and `SUBMITTED` attempts are resolved by callback or by the
  worker's status query with backoff. Until resolved, **no new attempt is
  created** for that payment through any provider.
- Failover to another provider is allowed only when the current attempt is
  `CONFIRMED_FAILURE` with a failure reason classified as safe to retry
  (e.g. provider rejected before execution). Customer-declined outcomes are
  not failed over.

## 4. Conflicting and late outcomes

Providers can report success after we considered a payment over (e.g. the
customer completes the prompt late, or a status query was wrong). Rules:

- A terminal payment state is never overwritten.
- If a verified provider outcome contradicts a terminal state (success on
  `FAILED`/`EXPIRED`), the platform records the callback, creates a
  **reconciliation exception** of type `late_success_on_terminal_payment`,
  raises an alert, and leaves resolution (refund/reversal or manual
  correction with audit) to an authorized operator.
- To minimize this, `FAILED` and `EXPIRED` require positive confirmation from
  the provider, not just the passage of time.

## 5. Concurrency

All transitions for a payment go through one function that:

1. Begins a transaction.
2. `SELECT … FOR UPDATE` on the payment row.
3. Validates the transition against the graph.
4. Writes the new state, a `payment_transitions` row (from, to, cause,
   actor, correlation ID, provider event ref), any ledger posting, and an
   outbox event.
5. Commits.

Callbacks, status-poll results, and merchant cancels all use this function,
so concurrent arrivals serialize on the row lock.

## 6. Per-workflow failure analysis (create payment, M-Pesa STK Push)

| Failure point | Authoritative state | Safe behaviour |
|---------------|---------------------|----------------|
| Crash before payment row commits | none | Merchant retries with same idempotency key → created fresh |
| Crash after payment commit, before provider call | payment `CREATED`, attempt `PENDING_SUBMISSION` | Worker picks up stale `PENDING_SUBMISSION` and submits (attempt ID is the idempotency reference) |
| Provider call times out | attempt `UNKNOWN` | Do not retry submission; status query with backoff |
| Provider returns error before acceptance | attempt `CONFIRMED_FAILURE` (if error class is definitive) | Payment `FAILED` or failover if safe |
| Callback never arrives | attempt `SUBMITTED` | Status query after provider-specific delay; alert if unresolved past threshold |
| Duplicate callback | unchanged | No-op, recorded |
| Callback for unknown reference | n/a | Persist raw callback, alert, do not create payments from callbacks |
| DB down when callback arrives | provider retries or not (provider-specific) | Return non-2xx so provider may retry; status query later covers loss |
| Queue down | outbox accumulates | Drains on recovery |

Operational alerts: attempts in `UNKNOWN` > N minutes; `SUBMITTED` without
callback > provider SLA; any `late_success_on_terminal_payment`.
