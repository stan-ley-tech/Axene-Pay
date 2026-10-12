# Provider Integration Contract

How a payment provider is plugged into Axene. Status: **Proposed** — the Go
interface is introduced in Phase 1 together with a clearly labelled test
provider; M-Pesa is the first real implementation (Phase 2).

## 1. Principles

- The payments/orchestration domain talks to providers only through this
  contract. Provider wire formats, auth, and error codes never leak upward.
- Providers declare capabilities; the domain checks them before calling.
  An adapter never implements an operation by faking it.
- Adapters are stateless apart from caches (e.g. OAuth token). Persistence
  of attempts and callbacks is done by the caller.
- Simulations are named as such: the in-repo test double is
  `providers/testprovider`, is only registrable in non-production
  environments, and the process refuses to start in `production` if it is
  configured.

## 2. Capabilities

```go
type Capability string

const (
    CapCollect        Capability = "collect"         // customer-initiated/prompted collection
    CapStatusQuery    Capability = "status_query"    // query outcome of an attempt
    CapRefundFull     Capability = "refund_full"
    CapRefundPartial  Capability = "refund_partial"
    CapReversal       Capability = "reversal"
    CapPayout         Capability = "payout"
    CapCancel         Capability = "cancel"          // cancel an in-flight attempt
)
```

Plus static data per provider configuration: supported currencies, min/max
amount per transaction, amount granularity (e.g. whole units only),
supported countries, and expected callback latency.

## 3. Interface sketch

```go
type Provider interface {
    Name() string
    Capabilities() CapabilitySet
    Limits() Limits

    // Submit sends a collection request. It must return an Outcome with
    // Status UNKNOWN (and a nil error) when it cannot determine whether the
    // provider received the request. Errors are reserved for programming /
    // configuration faults where nothing was sent.
    Submit(ctx context.Context, req SubmitRequest) (SubmitOutcome, error)

    // QueryStatus is only called when CapStatusQuery is declared.
    QueryStatus(ctx context.Context, ref AttemptRef) (StatusOutcome, error)

    // ParseCallback authenticates and parses an inbound callback.
    // It returns ErrCallbackRejected when authenticity cannot be established.
    ParseCallback(ctx context.Context, r CallbackRequest) (CallbackEvent, error)
}
```

Refund/payout operations are separate optional interfaces
(`Refunder`, `PayoutExecutor`) checked with a type assertion **and** the
capability set, so providers that cannot refund do not carry stub methods.

## 4. Error mapping

Every provider response maps to one of:

| Class | Meaning | Attempt state |
|-------|---------|---------------|
| `accepted` | Provider took the request; outcome pending | `SUBMITTED` |
| `succeeded` | Provider confirms completion | `CONFIRMED_SUCCESS` |
| `declined` | Customer/issuer declined, insufficient funds, cancelled by user | `CONFIRMED_FAILURE` (not failover-eligible) |
| `rejected` | Provider refused before execution (validation, limits, config) | `CONFIRMED_FAILURE` (failover-eligible if reason allows) |
| `unknown` | Timeout, connection error, 5xx after send, unparseable response | `UNKNOWN` |

The raw provider code and message are stored on the attempt for
investigation; only the class and a normalized reason drive logic.

## 5. Checklist before implementing a real provider

From the master definition §14 — each item must be answered in
`docs/providers/<name>.md` with links to official documentation:

1. Official API documentation  2. Authentication  3. Supported operations
4. Callback behaviour  5. Status-query behaviour  6. Limits
7. Refund/reversal semantics  8. Timeout/retry behaviour
9. Sandbox availability  10. Unresolved assumptions
