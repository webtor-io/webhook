-- Prepaid memberships sold through external payment providers.
-- billing.*      — provider-agnostic payment/membership state
-- nowpayments.*  — provider-specific raw callbacks (mirrors patreon.message)
-- public.price   — reference price list, lives next to the tier table

CREATE SCHEMA billing;
CREATE SCHEMA nowpayments;

-- Payment attempt lifecycle: one row per created checkout, the id doubles as
-- the provider-side order id, so callbacks bind to a concrete user without
-- email matching.
CREATE TABLE billing.payment (
    payment_id uuid DEFAULT uuid_generate_v4() NOT NULL,
    provider text DEFAULT 'nowpayments' NOT NULL,
    user_id text NOT NULL,
    email text NOT NULL,
    tier_id smallint NOT NULL,
    period_days integer NOT NULL,
    amount_usd numeric(10,2) NOT NULL,
    status text DEFAULT 'created' NOT NULL,
    provider_invoice_id text,
    provider_payment_id text,
    pay_currency text,
    actually_paid numeric,
    invoice_url text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY billing.payment
    ADD CONSTRAINT payment_pkey PRIMARY KEY (payment_id);

CREATE INDEX payment_email_lower_idx ON billing.payment USING btree (lower(email));
CREATE INDEX payment_created_at_idx ON billing.payment USING btree (created_at);

-- Active prepaid membership per (email, tier); public.claim picks the highest
-- active tier across all sources. A renewal extends expire_at, an upgrade adds
-- a second row with the higher tier — the remaining lower-tier period survives
-- underneath it.
CREATE TABLE billing.member (
    email text NOT NULL,
    tier_id smallint NOT NULL,
    expire_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY billing.member
    ADD CONSTRAINT member_pkey PRIMARY KEY (email, tier_id);

-- Raw NOWPayments IPN callbacks, stored verbatim after signature verification
-- (audit trail, mirrors patreon.message).
CREATE TABLE nowpayments.message (
    message_id uuid DEFAULT uuid_generate_v4() NOT NULL,
    payload jsonb NOT NULL,
    signature text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY nowpayments.message
    ADD CONSTRAINT message_pkey PRIMARY KEY (message_id);

CREATE INDEX message_created_at_idx ON nowpayments.message USING btree (created_at);

-- Price list: which (tier, period) combos are purchasable and for how much.
-- Reference data in public, next to tier. Seeded manually (see
-- infra/helmfile/charts/webhook/sql) — tier ids live in the manually-managed
-- tier table, so no seed here.
CREATE TABLE public.price (
    tier_id smallint NOT NULL,
    period_days integer NOT NULL,
    amount_usd numeric(10,2) NOT NULL
);

ALTER TABLE ONLY public.price
    ADD CONSTRAINT price_pkey PRIMARY KEY (tier_id, period_days);

-- Ownership mirrors migration 1's `OWNER TO webhook`, but only when the role
-- exists AND the migrating user can actually assign it (local dev DBs and
-- managed-PG admin users often can't) — an unassignable role must not fail
-- the migration.
DO $$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'webhook')
       AND pg_has_role(current_user, 'webhook', 'member') THEN
        EXECUTE 'ALTER SCHEMA billing OWNER TO webhook';
        EXECUTE 'ALTER SCHEMA nowpayments OWNER TO webhook';
        EXECUTE 'ALTER TABLE billing.payment OWNER TO webhook';
        EXECUTE 'ALTER TABLE billing.member OWNER TO webhook';
        EXECUTE 'ALTER TABLE nowpayments.message OWNER TO webhook';
        EXECUTE 'ALTER TABLE public.price OWNER TO webhook';
    END IF;
END $$;
