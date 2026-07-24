-- lava.top storefront webhooks. Purchases happen entirely on the lava.top
-- storefront; we only receive webhooks and grant billing.member access, so
-- there is no payment/invoice state here — just the raw callback audit trail
-- (mirrors nowpayments.message, minus signature: lava.top has no HMAC, the
-- webhook is authenticated by a shared header key).

CREATE SCHEMA lavatop;

CREATE TABLE lavatop.message (
    message_id uuid DEFAULT uuid_generate_v4() NOT NULL,
    payload jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY lavatop.message
    ADD CONSTRAINT message_pkey PRIMARY KEY (message_id);

CREATE INDEX message_created_at_idx ON lavatop.message USING btree (created_at);

-- Ownership mirrors migration 3's guarded OWNER TO webhook: only when the role
-- exists AND the migrating user can actually assign it.
DO $$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'webhook')
       AND pg_has_role(current_user, 'webhook', 'member') THEN
        EXECUTE 'ALTER SCHEMA lavatop OWNER TO webhook';
        EXECUTE 'ALTER TABLE lavatop.message OWNER TO webhook';
    END IF;
END $$;
