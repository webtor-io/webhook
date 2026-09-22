DROP INDEX IF EXISTS public.price_one_promo_idx;
ALTER TABLE public.price DROP CONSTRAINT IF EXISTS price_trial_days_check;
ALTER TABLE public.price DROP COLUMN IF EXISTS is_promo;
ALTER TABLE public.price DROP COLUMN IF EXISTS trial_days;
