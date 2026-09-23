-- Discount codes created at the membership provider. The provider holds the
-- real code and honours it; a row here states what the storefront may
-- promise about it, the way price.trial_days does for a trial, and must be
-- changed together with the provider side.
--
-- A code discounts the first billing period of a NEW membership on every
-- tier: period_days is that plan length, in the same units as
-- price.period_days (Patreon: 30 = first month, 365 = first year; members
-- without a paid membership only). It is typed in at the provider's checkout
-- — Patreon gives no link that carries it. expires_at is when the provider stops honouring it; rows
-- are kept after that as history, the catalog lists only live ones.
CREATE TABLE public.discount (
    code text NOT NULL,
    percent_off smallint NOT NULL,
    period_days smallint NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT discount_pk PRIMARY KEY (code),
    CONSTRAINT discount_percent_off_check CHECK (percent_off BETWEEN 5 AND 90),
    CONSTRAINT discount_period_days_check CHECK (period_days IN (30, 365))
);
