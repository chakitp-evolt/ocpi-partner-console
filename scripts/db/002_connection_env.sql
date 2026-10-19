-- evolt-ocpi-partner-console: one connection per page and leg in every Evolt environment (PostgreSQL 14+)
--
-- Run once, by hand, after 001_init.sql and before deploying the version that reads CONSOLE_ENVS:
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f scripts/db/002_connection_env.sql
--
-- - The whole script is one transaction: any error rolls everything back.
-- - It stops if partner_console_connections.env already exists, so it never runs twice.
-- - Existing rows become the dev environment; the running version keeps working until it is replaced.

BEGIN;

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = current_schema() AND table_name = 'partner_console_connections' AND column_name = 'env'
  ) THEN
    RAISE EXCEPTION 'partner_console_connections.env already exists in schema %', current_schema()
      USING HINT = 'This script runs once; nothing was changed.';
  END IF;
END
$$;

-- ---- connections ----------------------------------------------------------------------------

ALTER TABLE partner_console_connections
  ADD COLUMN env text NOT NULL DEFAULT 'dev' CHECK (env ~ '^[a-z][a-z0-9-]*$');

ALTER TABLE partner_console_connections DROP CONSTRAINT partner_console_connections_leg_profile_key;
ALTER TABLE partner_console_connections ADD CONSTRAINT partner_console_connections_env_leg_profile_key UNIQUE (env, leg, profile);

COMMENT ON TABLE partner_console_connections IS 'One row per environment, page and leg: /emsp (emsp, emsp), /cpo (cpo, cpo), /dual (emsp, dual) and (cpo, dual), under /<env> for every environment but dev. Reset deletes the row and everything of that connection cascades.';
COMMENT ON COLUMN partner_console_connections.env IS 'Evolt environment the connection talks to: dev at the root URL, any other under /<env>';

-- ---- exchanges ------------------------------------------------------------------------------

ALTER TABLE partner_console_exchanges ADD COLUMN env text;
UPDATE partner_console_exchanges SET env = 'dev' WHERE saved_request_id IS NOT NULL;
ALTER TABLE partner_console_exchanges ADD CONSTRAINT partner_console_exchanges_env_check CHECK (connection_id IS NULL OR env IS NULL);

COMMENT ON COLUMN partner_console_exchanges.env IS 'Saved request only: environment the request was sent from; a connection exchange takes it from its connection';

COMMIT;
