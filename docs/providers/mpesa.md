# M-Pesa (Safaricom Daraja) — Integration Notes

Status: **Target product decided (M-Pesa Express / STK Push); adapter not
implemented.** Everything below about Daraja's API is drawn from publicly
documented behaviour as generally known at the time of writing and **must be
verified against the official portal (https://developer.safaricom.co.ke)
before the adapter is written (Phase 2).** Items marked ⚠ are known
ambiguities. External dependencies are tracked as X-01…X-08 in
[decisions.md](../decisions.md).

The M-Pesa integration is **not production-ready** and must not be described
as such until it has been tested against the actual Daraja environment.

## 1. Scope of the first integration

| Need | Daraja API (to verify) | Status |
|------|------------------------|--------|
| Collect from customer | M-Pesa Express (STK Push) | Phase 2 target |
| Verify outcome | STK Push Query ⚠ semantics/rate limits (X-06) | Phase 2 target |
| Customer-initiated pay to Paybill/Till | C2B | Not planned |
| Reversal | Reversal API | Not planned (Phase 3+) |
| Payout to customer | B2C | Not planned (no payouts under ADR-0008) |

## 2. How Axene's design maps onto STK Push

| Axene concept | STK Push concept (to verify) |
|---------------|------------------------------|
| Payment attempt submission | STK Push request; synchronous response = **accepted**, not success |
| Attempt provider reference | `CheckoutRequestID` (and `MerchantRequestID`) |
| Settlement destination | Business shortcode (Paybill/Till) the payment is made to (X-03) |
| Callback | Result callback to our `CallBackURL` carrying a `ResultCode` |
| Verification | STK Push Query by `CheckoutRequestID` (X-06) |
| Receipt for reconciliation | M-Pesa receipt number in the success callback/statement |

## 3. Authentication (to verify)

- OAuth2 client credentials: consumer key + secret → short-lived bearer token,
  cached and refreshed before expiry (X-02).
- STK Push password derived from shortcode + passkey + timestamp (X-04).
- Credentials are configuration loaded from the secret store at runtime,
  never in the repository, per environment (local fake, sandbox,
  production).

## 4. Callback security

Callbacks are, to our knowledge, unsigned ⚠. Handling follows
[ADR-0009](../adr/0009-callback-verification.md): secret per-environment URL
token, source-IP allow-list at the edge once Safaricom's ranges are confirmed
(X-05), durable persistence, then mandatory status-query verification before
any terminal state.

## 5. Amount representation ⚠

STK Push documentation shows `Amount` as whole shillings. The adapter must
reject KES amounts with non-zero cents (`money.Amount.WholeUnits`) rather
than rounding.

## 6. Result codes ⚠

The full `ResultCode` list (success, cancelled by user, timeout waiting for
PIN, insufficient funds, …) must be taken from official documentation and
mapped explicitly to Axene outcome classes (`succeeded`, `declined`,
`rejected`, `unknown`). Unmapped codes map to `unknown`, never to failure.

## 7. Open questions for Safaricom / account manager

1. Which products are enabled for Axene (X-01)?
2. Can Axene initiate STK Push for merchant-owned shortcodes with Axene's
   credentials, or does each merchant need its own credentials/passkey (X-03)?
3. Official callback source IP ranges; any signing/mTLS option (X-05).
4. STK Query semantics while the prompt is still open; rate limits (X-06).
5. Statement/report formats available to merchants and Axene for
   reconciliation.
6. Transaction limits applicable per shortcode type.
