# ADR-0010: Merchant webhooks

- Status: Accepted
- Date: 2026-10-12

## Context

Merchants need to learn about payment outcomes without polling. Delivery
must survive merchant downtime, never expose Axene's network to the
merchant-supplied URL (SSRF), let merchants verify authenticity, and never
depend on the broker as the only record (ADR-0004).

## Decision

### Model

- `webhook_endpoints`: per merchant and environment, an HTTPS URL, the
  event types it subscribes to (empty = all payment events), a status, and
  a signing secret.
- `webhook_event_fanouts`: one row per outbox event once deliveries have
  been created for it — makes fan-out idempotent and auditable.
- `webhook_deliveries`: one row per (endpoint, event), unique, holding the
  exact JSON body to send (a snapshot taken at fan-out), status
  (`pending`, `delivered`, `dead`), attempt count, next attempt time, and the
  last result.
- `webhook_delivery_attempts`: append-only history of every HTTP attempt
  (status code or error, duration).

### Flow

1. A payment transition writes an outbox event (existing).
2. The worker fans out each event to the merchant's subscribed active
   endpoints. Two triggers, both idempotent through `webhook_event_fanouts`:
   a RabbitMQ consumer for low latency, and a database sweep for events
   published more than a few minutes ago without a fan-out row, so a lost
   or unconsumed message only delays notification, never loses it.
3. The worker claims due deliveries with a lease and POSTs them. 2xx is
   success. Anything else (including timeouts and redirects) is retried
   with exponential backoff; after the maximum attempts the delivery is
   `dead` and visible to the merchant, who can request a replay.

### Payload and signature

```
POST <endpoint url>
Content-Type: application/json
Axene-Event-Id: evt_…
Axene-Event-Type: payment.succeeded
Axene-Delivery-Id: whd_…
Axene-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>

{"id":"evt_…","object":"event","type":"payment.succeeded","api_version":"v1",
 "created_at":"…","data":{"payment":{ …same shape as GET /v1/payments/{id}… }}}
```

- Merchants verify by recomputing the HMAC over `t + "." + raw body` and
  rejecting timestamps older than their tolerance (recommended 5 minutes)
  — replay protection.
- The `data.payment` object is the payment as it was when the event was
  fanned out. Webhooks are notifications: merchants that need the current
  state call `GET /v1/payments/{id}`. Delivery order is not guaranteed;
  merchants order by the payment's status semantics, not arrival order.
- Phone numbers are masked exactly as in the API.

### Secrets

Signing secrets (`whsec_` + 32 random bytes) are shown once at creation.
They must be stored recoverably (HMAC needs the key), so they are encrypted
with AES-256-GCM (Go standard library) under `WEBHOOK_SECRETS_KEY` from the
secret store, with the endpoint ID as additional authenticated data so a
ciphertext cannot be moved to another endpoint. A key version column allows
rotation.

### Network safety

The delivery client resolves the host itself and refuses loopback, private,
link-local (including cloud metadata), multicast, unspecified, and
carrier-grade NAT addresses, follows no redirects, and times out after 10
seconds. Endpoint URLs must be `https` in deployed environments. Private
targets can be allowed only in `development`/`test`
(`WEBHOOK_ALLOW_PRIVATE_TARGETS`) so local receivers work.

## Consequences

- At-least-once delivery: merchants must deduplicate by `Axene-Event-Id`.
- Delivery bodies are stored, so webhook data retention follows payment
  data retention (D-09).
- Endpoint management is API-only until the merchant portal (Phase 4).
