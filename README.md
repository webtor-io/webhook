# webhook

Stores webhooks from different providers and manages the resulting paid
memberships.

## Webhook receivers (public, whitelisted on the ingress)

1. Patreon — `POST /patreon`, raw events stored in `patreon.message`,
   membership derived via the `patreon.member` materialized view.
2. NOWPayments — `POST /nowpayments` IPN (HMAC-SHA512 over the key-sorted
   JSON body, `x-nowpayments-sig` header). Raw callbacks stored in
   `nowpayments.message`; on `finished` the payment grants/extends
   `billing.member` and publishes `user.updated` to NATS. Empty
   `NOWPAYMENTS_IPN_SECRET` disables processing (503).

## Invoice API (cluster-internal, consumed by web-ui)

Provider-agnostic prepaid-membership purchases; the provider is selected per
request (`"provider": "nowpayments"` for now, dispatched via the
`InvoiceProvider` interface in `services/invoice.go`):

- `PUT /invoice/{id}` — create an invoice for a caller-generated uuid. A
  repeated PUT with an existing id returns the stored invoice (idempotent, no
  double-charge). Body: `provider`, `user_id`, `email`, `tier_id`,
  `period_days`; the amount always comes from the `price` table.
- `GET /invoice/{id}` — payment state.
- `GET /prices` — purchasable (tier, period, amount) plans.

These routes carry no auth (cluster-internal, matching the other webtor
services) — the ingress path whitelist, which covers only the receivers
above, is what keeps them off the internet. Never widen it back to `/`.
Empty `NOWPAYMENTS_API_KEY` disables invoice creation at the provider.
