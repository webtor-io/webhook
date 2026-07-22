-- Plans stay listed even when they cannot currently be purchased (e.g. the
-- payment provider's minimum payment amount exceeds the plan price) — the
-- storefront shows a footnote instead of silently hiding them.
ALTER TABLE public.price ADD COLUMN available boolean DEFAULT true NOT NULL;
