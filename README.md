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
- `GET /invoices?user_id=` — one user's payment history, newest first.
- `GET /prices` — the storefront catalog, read by web-ui to build every offer
  (tier cards, in-app upsells):
  - `prices` — plans on sale, one per (tier, period): `amount_usd`,
    `available`, and the offer terms `trial_days` (> 0 = the plan starts with
    a free trial of that length; 0 = no trial) and `is_promo` (the one plan
    in-app offers sell and the storefront recommends; a partial unique index
    allows at most one). Offer terms are how a plan is sold and live on
    `price`; what a tier grants lives on `tier`.
  - `tiers` — what each tier grants, including tiers without a price (free):
    `download_rate` (Mbit/s), `vault_points`, `site_noads`, `embed_noads`,
    the same columns the claims are built from. `null` rate or Vault Points
    = unlimited.
  - `discounts` — discount codes the membership provider honours right now
    (`expires_at` in the future): `code`, `percent_off`, `period_days` (the
    plan length whose first billing period is discounted, same units as
    `price.period_days`: 30 = first month, 365 = first year of a new
    membership, on every tier) and `expires_at`. The code is typed in at the provider's checkout (Patreon
    gives no link that carries it). web-ui hands a code out only while it has
    at least 72 h left.

  Every list is always an array; a missing key means a webhook older than
  that part of the catalog.

  The promo index is checked per row, so move the promo in two statements —
  `UPDATE price SET is_promo = false WHERE is_promo;` then set it on the new
  plan; a single statement that clears and sets at once can abort on the index
  depending on row order.

  `trial_days` states what the storefront may promise — the trial itself is
  configured at the membership provider, change both together.

  A discount code is created at the provider first, then recorded here. The
  row states what the letter may promise, so it must match the provider's
  settings exactly; `expires_at` is the moment the provider stops honouring
  the code (Patreon codes live at most 180 days):

  ```sql
  INSERT INTO discount (code, percent_off, period_days, expires_at)
  VALUES ('<code>', 50, 30, '2027-03-22 00:00+03');
  ```

  To withdraw a code early, move its `expires_at` to now; keep the row.

These routes carry no auth (cluster-internal, matching the other webtor
services) — the ingress path whitelist, which covers only the receivers
above, is what keeps them off the internet. Never widen it back to `/`.
Empty `NOWPAYMENTS_API_KEY` disables invoice creation at the provider.
