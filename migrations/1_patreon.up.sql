CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE SCHEMA patreon;

CREATE TABLE patreon.message (
    message_id uuid DEFAULT uuid_generate_v4() NOT NULL,
    payload jsonb NOT NULL,
    event text NOT NULL,
    signature bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE patreon.message OWNER TO webhook;

ALTER TABLE ONLY patreon.message
    ADD CONSTRAINT message_pkey PRIMARY KEY (message_id);

CREATE INDEX created_at_idx ON patreon.message USING btree (created_at);