-- lava.top integration was discontinued before launch (2026-07). Drops the
-- webhook audit schema created by migration 5; the only rows ever written
-- were deploy smoke tests.
DROP SCHEMA IF EXISTS lavatop CASCADE;
