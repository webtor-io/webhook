-- Offer terms of a plan, next to its amount: how it is sold, not what it
-- grants (that stays in tier and reaches users through the claims).
--
-- trial_days: the plan can be started with a free trial of this length;
-- 0 = no trial. One field instead of a has_trial flag plus a length, so
-- "trial without a length" cannot be stored. The trial itself is configured
-- at the membership provider — this column states what the storefront may
-- promise and must be changed together with it.
--
-- is_promo: the plan the in-app offers sell (download nudge, cap modal,
-- grace popup, promo banner) and the storefront marks as recommended. At
-- most one plan — enforced by the partial unique index below.
ALTER TABLE public.price
    ADD COLUMN trial_days smallint DEFAULT 0 NOT NULL,
    ADD COLUMN is_promo boolean DEFAULT false NOT NULL;

ALTER TABLE public.price
    ADD CONSTRAINT price_trial_days_check CHECK (trial_days >= 0);

CREATE UNIQUE INDEX price_one_promo_idx ON public.price ((true)) WHERE is_promo;

-- Current offer: Silver monthly with a 7-day trial, promoted. Keyed by tier
-- name because tier ids live in the manually-managed tier table, which no
-- migration creates — on a database without it (local dev) this is skipped.
DO $$
BEGIN
    IF to_regclass('public.tier') IS NOT NULL THEN
        UPDATE public.price
        SET trial_days = 7, is_promo = true
        WHERE period_days = 30
          AND tier_id = (SELECT tier_id FROM public.tier WHERE name = 'silver');
    END IF;
END
$$;
