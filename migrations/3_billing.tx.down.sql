-- ВНИМАНИЕ: если в public.claim уже добавлена UNION-ветка по billing.member
-- (charts/webhook/sql/2026-07-20_claim_billing_apply.sql), CASCADE снесёт view
-- И обе функции get_member_claims_* — claims-provider ляжет для ВСЕХ юзеров.
-- После down-миграции ОБЯЗАТЕЛЬНО восстановить manual_ddl_snapshot.sql.
DROP SCHEMA billing CASCADE;
DROP SCHEMA nowpayments CASCADE;
DROP TABLE IF EXISTS public.price;
