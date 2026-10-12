# Decisions and External Dependencies

Decisions that have been made are recorded with their conditions; open ones
have an owner and a proposed default so work is not blocked unnecessarily.
Architectural decisions also get an ADR.

## Decided (2026-10-12)

| ID | Decision | Record | Condition before production |
|----|----------|--------|-----------------------------|
| D-01 | First integration targets **M-Pesa Express (STK Push)** | [mpesa.md](providers/mpesa.md) | Confirm the product is enabled for Axene |
| D-02 | **Direct merchant settlement**: funds go to the merchant's own shortcode; Axene holds no merchant funds and does no payouts initially; architecture stays extensible to centralized collection | [ADR-0008](adr/0008-direct-merchant-settlement.md) | Confirm against actual Daraja capabilities, merchant agreements, and regulatory requirements |
| D-07 | Callbacks are untrusted; terminal outcomes require an authenticated status query or reconciliation | [ADR-0009](adr/0009-callback-verification.md) | Confirm the STK status query exists with adequate semantics and rate limits |
| D-03 | Phase 1 merchant model: `pending_review → approved → suspended`; live API keys only for `approved` merchants; KYC/KYB fields added when compliance defines them | [roadmap.md](roadmap.md) | Compliance defines onboarding data and approval steps |
| D-10 | Idempotency keys retained at least 24 hours (configurable) | [ADR-0007](adr/0007-idempotency.md) | — |
| D-11 | Public IDs: type prefix + 26-char Crockford base32 of 128 random bits (`pay_…`); UUID primary keys internally | [conventions.md](conventions.md) | — |

## External dependencies (blocking real M-Pesa traffic, not development)

Development continues against the clearly labelled `testprovider`. None of
these values may be guessed or committed.

| ID | Dependency | Owner |
|----|------------|-------|
| X-01 | Exact Daraja products enabled for Axene | CEO / Safaricom |
| X-02 | Sandbox consumer key and secret | CEO / engineering |
| X-03 | Shortcode configuration per merchant (and whether merchant-owned shortcodes can be used with Axene's credentials) | Safaricom / merchants |
| X-04 | Passkey and other per-shortcode configuration values | Safaricom / merchants |
| X-05 | Callback URLs, Safaricom source IP ranges, required network configuration | Engineering / Safaricom |
| X-06 | Availability, semantics and rate limits of the STK status query (and transaction status API) | Safaricom docs / account manager |
| X-07 | Production onboarding requirements | CEO / Safaricom |
| X-08 | Regulatory/compliance confirmation of the direct-settlement model | CEO / legal |

## Open

| ID | Decision | Needed by | Proposed default |
|----|----------|-----------|------------------|
| D-05 | Cloud provider, region, managed secret store | First shared environment | — |
| D-06 | Operator SSO / identity provider | Phase 4 | — |
| D-08 | Physical separation of sandbox and live data | Before live launch | Separate databases |
| D-09 | Data retention periods and data residency (Kenya DPA, CBK) | Before live launch | — |
| D-12 | Fee model: Axene pricing, who bears provider fees, rounding, and how fees are collected when Axene never holds funds | Phase 3 | — |
| D-13 | Centralized collection + merchant payouts (future funds-flow model) | When commercially/legally justified | Not built |
| D-14 | Operational ownership: on call, approval of reconciliation corrections | Before live launch | — |
| D-15 | Card acquiring partner | Phase 5 | — |
| D-16 | Production security requirements: pen test, PCI scope, certifications | Phase 6 | — |
| D-17 | Merchant "applications" (multiple integrations per merchant) | Phase 4 | Not in Phase 1: API keys belong directly to a merchant + environment |

## Regulatory note

A licence or business agreement does not automatically grant access to every
rail or authorize every activity. Holding customer funds for merchants,
payouts, and card acquiring may each require separate authorization.
Engineering will not enable such flows in production until the relevant
authorization is confirmed in writing and recorded here.
