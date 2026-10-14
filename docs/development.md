# Local Development

## Prerequisites

| Tool | Version | Needed for |
|------|---------|------------|
| Go | 1.27.x (see `go.mod`) | Backend |
| PostgreSQL | 16+ | Everything (Docker image or a local install) |
| RabbitMQ | 4.x | The worker's outbox relay |
| Docker + Compose v2 | recent | Optional: runs PostgreSQL, Redis, RabbitMQ for you |
| sqlc | v1.31.1 | Only when changing SQL in `internal/store/queries` |
| GNU make | any | Optional convenience; targets are plain `go` commands |

On Windows, run `make` targets from Git Bash or WSL, or run the commands
listed in the `Makefile` directly.

## First run

```sh
cp .env.example .env                 # development values only
make deps-up                         # or point DATABASE_URL/RABBITMQ_URL at your own servers
make migrate                         # apply migrations (never automatic on startup)
make run-api                         # http://localhost:8080
go run ./cmd/worker                  # in a second terminal (needs RabbitMQ)
```

Without Docker, create the role and databases in any PostgreSQL 16+:

```sql
CREATE ROLE axene LOGIN PASSWORD 'axene_dev_only' CREATEDB;
CREATE DATABASE axene OWNER axene;
CREATE DATABASE axene_test OWNER axene;
```

## Creating a merchant and making a test payment

The only provider in development is **`testprovider`, a simulation**. It
moves no money and is refused in production.

```sh
set -a; . ./.env; set +a
go run ./cmd/axenectl merchant create --name "Duka Ltd"
go run ./cmd/axenectl destination create --merchant mer_… --env sandbox --type mpesa_paybill --identifier 600123
go run ./cmd/axenectl provider-config create --merchant mer_… --env sandbox --provider testprovider --destination dst_…
go run ./cmd/axenectl apikey create --merchant mer_… --env sandbox     # prints the secret once

curl -s localhost:8080/v1/payments \
  -H "Authorization: Bearer sk_test_…" -H "Idempotency-Key: order-1" \
  -H "Content-Type: application/json" \
  -d '{"amount":10000,"currency":"KES","payment_method":{"type":"mpesa","phone_number":"254700000000"}}'
```

The simulated provider's behaviour depends on the payer phone number:

| Phone | Behaviour |
|-------|-----------|
| 254700000000 | Accepted; status query reports success |
| 254700000001 | Accepted; status query reports declined |
| 254700000002 | Submission times out (outcome unknown); status query reports success |
| 254700000003 | Rejected synchronously |
| 254700000004 | Accepted; status query always inconclusive → `UNRESOLVED` |
| 254700000005 | Accepted; status query reports a different amount → `UNRESOLVED` |
| 254700000006 | Accepted; provider reports "still pending" forever → `UNRESOLVED` |
| anything else | Accepted; status query reports success |

To simulate a provider callback, post to the callback URL with the
simulated provider's shared secret (the provider reference is on the
attempt row):

```sh
curl -s localhost:8080/callbacks/testprovider/$CALLBACK_TOKEN \
  -H "X-Testprovider-Secret: $TESTPROVIDER_CALLBACK_SECRET" \
  -d '{"notification_id":"n-1","reference":"tp_…","result":"succeeded"}'
```

The payment moves to `PENDING_VERIFICATION`; the worker then confirms it by
status query (ADR-0009). Without a callback the worker queries after
the initial verification delay.

## Receiving webhooks locally

With `WEBHOOK_ALLOW_PRIVATE_TARGETS=true` (development only) you can point an
endpoint at a local receiver:

```sh
curl -s localhost:8080/v1/webhook_endpoints -H "Authorization: Bearer sk_test_…" \n  -H "Content-Type: application/json" -d '{"url":"http://127.0.0.1:9000/hooks"}'
# the response contains "secret": "whsec_…" — shown once
```

The worker fans out each payment event and POSTs it with an
`Axene-Signature` header (see ADR-0010 and the OpenAPI description for the
verification recipe; `webhooks.Verify` is the reference implementation).
Inspect deliveries with `GET /v1/webhook_endpoints/{id}/deliveries` and
`GET /v1/webhook_deliveries/{id}`; replay with
`POST /v1/webhook_deliveries/{id}/replay`.

## Fees, settlement and reconciliation (operators)

No rate is built in. A fee schedule is operator data: create it as a draft,
then activate it with the approver's name and an approval reference. Every
change is written to `audit_log`. The values below are **test fixtures only**.

```sh
go run ./cmd/axenectl fee-schedule create --env sandbox --currency KES \
  --percentage-ppm 10000 --fixed 0 --rounding half_up --fee-payer merchant \
  --provider-fee-bearer merchant --reversal-policy pending_approval \
  --effective-from 2026-10-13T00:00:00Z --description "TEST FIXTURE"
go run ./cmd/axenectl fee-schedule activate --schedule fsc_… --operator you --approval-ref TEST-FIXTURE
```

A schedule without `--merchant` is the platform default; a merchant (and
optionally provider) schedule takes precedence. Schedules are immutable once
active: end one (`fee-schedule end --at …`) and activate a new version.
Payments keep the schedule in effect when they were created.

Fees accrue when a payment succeeds and stay `accrued` until collected with
evidence. The collection mechanism defaults to `none_configured`
(ADR-0011). In local development only, `fee-terms set --mechanism simulated`
works because the worker registers the simulated collector there.

Settlement statements are imported in Axene's normalized CSV format (amounts
in minor units; `gross = provider_fee + net + axene_fee_collected`):

```csv
provider_reference,receipt,destination,currency,gross,provider_fee,net,settled_at,axene_fee_collected
tp_0123abcd,,600123,KES,100000,150,99850,2026-10-13T12:00:00Z,
```

```sh
go run ./cmd/axenectl settlement import --provider testprovider --env sandbox --file stmt.csv --source-ref stmt-2026-10-13 --operator you
go run ./cmd/axenectl recon run --provider testprovider --env sandbox --operator you
go run ./cmd/axenectl recon list --open
go run ./cmd/axenectl recon resolve --exception rex_… --action payment_succeeded --operator you --note "settled per stmt-2026-10-13"
```

Resolving with a payment outcome applies it through the payment state
machine (reconciliation evidence) and accrues or reverses fees exactly as any
other outcome would; the statement line is then matched on the next run.

## Endpoints

| Path | Auth | Purpose |
|------|------|---------|
| `GET /healthz` | none | Liveness; never touches dependencies |
| `GET /readyz` | none | Readiness: PostgreSQL reachable within 2 s |
| `POST /v1/payments` | API key + `Idempotency-Key` | Create a payment |
| `GET /v1/payments/{id}` | API key | Retrieve a payment |
| `POST /v1/payments/{id}/cancel` | API key | Cancel before submission |
| `POST/GET /v1/webhook_endpoints`, `GET /v1/webhook_endpoints/{id}`, `POST …/{id}/disable`, `GET …/{id}/deliveries` | API key | Webhook endpoint management |
| `GET /v1/webhook_deliveries/{id}`, `POST …/{id}/replay` | API key | Delivery history and replay |
| `POST /callbacks/{provider}/{token}` | path token + provider-specific | Provider callbacks |
| `GET /metrics` on `METRICS_ADDR` | network-restricted | Prometheus metrics (api `:9090`, worker `:9091`) |

The contract is `api-spec/openapi.yaml`.

## Configuration

All configuration is environment variables; see `.env.example` and
`internal/platform/config`. Both processes refuse to start with invalid
configuration and report every problem at once. Notable rules:

- `API_KEY_PEPPER` ≥ 32 characters; rotating it invalidates every API key.
- `CALLBACK_TOKEN` ≥ 32 URL-safe characters.
- Deployed environments (`sandbox`, `production`) require TLS to
  PostgreSQL, `https` for `PUBLIC_BASE_URL`, `amqps` for RabbitMQ, and
  forbid debug logging.
- `TESTPROVIDER_ENABLED=true` is rejected in `production`.

### Verification timing (code defaults, `payments.DefaultPolicy`)

| Parameter | Default | Meaning |
|-----------|---------|---------|
| Provider timeout | 30 s (`PROVIDER_TIMEOUT`) | Bound on each provider call |
| Initial verification delay | 60 s | First status query when no callback arrives |
| Retry backoff | 15 s doubling to 5 min | Between inconclusive status queries |
| Max verification attempts | 12 | Then `UNRESOLVED` + reconciliation exception |
| Submission lease | 2 min | Claimed-but-unrecorded submissions become `SUBMISSION_UNKNOWN` |
| Idempotency key retention | 24 h | Minimum per D-10 |
| Webhook attempt timeout | 10 s | Per HTTP attempt |
| Webhook retries | 16 attempts, 30 s doubling to 1 h | About 9 hours, then `dead` (replayable) |
| Webhook sweep delay | 2 min | Published events without fan-out are swept |

These are operational values, not business rules; revisit them with real
M-Pesa Express latency data in Phase 2.

## Tests

| Command | Needs |
|---------|-------|
| `make test` | nothing; unit tests |
| `make test-integration` | `TEST_DATABASE_URL` (creates and drops a database per test) |
| `go test -tags=rabbitmq ./internal/outbox` | `RABBITMQ_TEST_URL` |
| `make generate` | sqlc v1.31.1; CI fails if generated code is stale |

CI runs all of the above (PostgreSQL and RabbitMQ as service containers),
plus the race detector, `govulncheck`, and OpenAPI lint.
