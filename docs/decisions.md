# Open Decisions

Genuinely unresolved decisions. Each has an owner and a proposed default so
work is not blocked unnecessarily. When resolved, record the answer here and,
if architectural, in an ADR.

## Blocking the first working integration (Phase 2)

| ID | Decision | Why it matters | Proposed default | Owner |
|----|----------|----------------|------------------|-------|
| D-01 | Which Daraja products is Axene enabled for, under which shortcode(s)? | Determines collect flow (STK vs. C2B), refunds (reversal), payouts (B2C) | Build STK Push + STK Query first | CEO / Safaricom |
| D-02 | **Funds flow: technical gateway (merchant's shortcode) or aggregator (Axene's shortcode)?** | Determines chart of accounts, settlement, payouts, licensing, and whether Axene holds merchant funds | Design for both; implement ledger only after decision | CEO / legal |
| D-04 | Daraja sandbox credentials (consumer key/secret, passkey, test shortcode) | Phase 2 cannot start without them | Request from Axene's Daraja developer account | CEO / engineering |
| D-07 | Callback trust policy: confirm every success callback with STK Query, or match fields + reconcile? | Trade-off between forged-callback risk and query rate limits / latency | Query-confirm every success callback until volume makes it impractical | Engineering + risk |

## Blocking Phase 1 (can be decided by engineering with sign-off)

| ID | Decision | Proposed default |
|----|----------|------------------|
| D-03 | Merchant onboarding: which KYC/KYB data and approval steps gate live access? | Phase 1 models states `pending_review → approved → suspended`; live keys only for `approved`; data fields added when compliance defines them |
| D-10 | Idempotency key retention period | 24 hours minimum, configurable |
| D-11 | Public ID format | Prefixed random IDs (`pay_` + 26 chars base32 of 128-bit random); UUIDv7 primary keys internally |

## Deferrable

| ID | Decision | Needed by |
|----|----------|-----------|
| D-05 | Cloud provider, region, managed secret store | Before first shared environment (end of Phase 2) |
| D-06 | Operator SSO / identity provider | Phase 4 |
| D-08 | Physical separation of sandbox and live data (separate DB/cluster?) | Before live launch; default: separate databases |
| D-09 | Data retention periods and data-residency obligations (Kenya DPA, CBK requirements) | Before live launch |
| D-12 | Fee model (who bears provider fees, Axene pricing, rounding rule) | Phase 3 |
| D-13 | Settlement schedule and payout rails for merchants (Model B only) | Phase 3/5 |
| D-14 | Operational ownership: who is on call, who approves reconciliation corrections and payouts | Before live launch |
| D-15 | Card acquiring partner | Phase 5 |
| D-16 | Production security requirements: pen test, PCI scope, SOC2/ISO aspirations | Phase 6 |

## Regulatory note

A license or business agreement does not automatically grant access to every
rail or authorize every activity (master definition §2). In particular,
holding customer funds for merchants (Model B), making payouts, and card
acquiring may each require separate authorization. Engineering will not
enable these flows in production until the relevant authorization is
confirmed in writing and recorded here.
