# M-Pesa (Safaricom Daraja) — Integration Notes

Status: **Research — not implemented.** Everything below is drawn from
Safaricom's publicly documented Daraja APIs as generally known at the time of
writing and **must be verified against the official portal
(https://developer.safaricom.co.ke) before any code is written.** Items
marked ⚠ are known areas of ambiguity.

## 1. Candidate operations

| Need | Daraja API (to verify) | Notes |
|------|------------------------|-------|
| Collect from customer | Lipa Na M-Pesa Online / **M-Pesa Express (STK Push)** | Customer gets a PIN prompt on their phone |
| Query STK outcome | STK Push Query | For resolving attempts with no callback |
| Customer-initiated pay to Paybill/Till | C2B (register URLs, validation/confirmation) | Possible second collection flow |
| General transaction status | Transaction Status | Asynchronous: result delivered to a callback URL |
| Reversal | Reversal API | Asynchronous; semantics and eligibility windows ⚠ |
| Payout to customer | B2C | Phase 5; requires separate product enablement |
| Account balance | Account Balance | Useful for reconciliation (Model B) |

## 2. Authentication (to verify)

- OAuth2 client-credentials: consumer key + secret → short-lived bearer
  token. Token must be cached and refreshed before expiry; secrets live in the
  secret store.
- STK Push additionally requires a password derived from shortcode +
  passkey + timestamp.
- Some APIs (B2C, reversal, transaction status) require an initiator name
  and a **security credential** (initiator password encrypted with
  Safaricom's public certificate).

## 3. Callback security ⚠

Daraja callbacks are, to our knowledge, **not signed**. A callback alone is
therefore not proof of payment. Proposed mitigations:

1. Callback URL path contains a high-entropy, per-environment secret token;
   rotate on suspicion.
2. Restrict source IPs to Safaricom's published ranges (must be obtained
   from Safaricom, not guessed) at the load balancer.
3. **Treat a success callback as a trigger, not as truth**: before marking
   `SUCCEEDED`, either confirm via STK Push Query or match the callback
   fields (CheckoutRequestID, amount, MSISDN, receipt number) against the
   stored attempt, and require later reconciliation against statements.
   The exact policy (query-on-every-callback vs. match-and-reconcile) is
   decision D-07.

## 4. Amount representation ⚠

STK Push documentation shows `Amount` as a whole number of shillings. If so,
the adapter must reject KES amounts with non-zero cents rather than rounding
(ADR-0005).

## 5. Known behaviours that affect our state machine (to verify)

- STK Push synchronous response = request accepted (`ResponseCode` 0) — this
  is `accepted`, **not success**.
- The callback carries a `ResultCode`; 0 = success, non-zero codes include
  user cancelled, timeout waiting for PIN, insufficient funds, etc. The full
  code list must be taken from official docs and mapped to
  `declined` / `rejected` / `unknown`.
- STK prompts expire after a short window on the handset; a query
  immediately after may report "still processing". The polling schedule must
  respect this.
- Duplicate and delayed callbacks are possible.

## 6. Sandbox

Daraja provides a sandbox with test credentials and test shortcodes. Sandbox
behaviour differs from production in places (e.g. callbacks, test MSISDNs) ⚠.
Sandbox credentials are needed from the Axene Daraja account — decision D-04.

## 7. Open questions for Safaricom / account manager

1. Which product(s) will Axene be enabled for (STK, C2B, B2C, reversal)?
2. Shortcode ownership model (Axene vs. merchant) — ties to D-02.
3. Official callback source IP ranges and any signing/mTLS options.
4. Settlement timing and statement format for reconciliation.
5. Transaction limits per transaction/day applicable to our shortcode type.
6. Reversal eligibility window and whether partial reversals exist.
