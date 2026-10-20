-- evolt-ocpi-partner-console: initial schema (PostgreSQL 14+)
--
-- Applied automatically at startup on an empty database. Optional standalone use:
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f scripts/db/001_init.sql
--
-- - The whole script is one transaction: any error rolls everything back.
-- - It stops if a partner_console_* table already exists, so it never runs over an existing schema.
-- - Check afterwards: \dt partner_console_*   (5 tables)

BEGIN;

DO $$
DECLARE
  existing text;
BEGIN
  SELECT string_agg(table_name, ', ' ORDER BY table_name) INTO existing
  FROM information_schema.tables
  WHERE table_schema = current_schema() AND table_name LIKE 'partner\_console\_%';
  IF existing IS NOT NULL THEN
    RAISE EXCEPTION 'partner console tables already exist in schema %: %', current_schema(), existing
      USING HINT = 'This script creates the schema once; nothing was changed.';
  END IF;
END
$$;

-- ---- connections ----------------------------------------------------------------------------

CREATE TABLE partner_console_connections (
  id text PRIMARY KEY,
  leg text NOT NULL CHECK (leg IN ('emsp', 'cpo')),
  profile text NOT NULL,
  name text NOT NULL DEFAULT '',
  status text NOT NULL DEFAULT 'SETUP',
  config jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(config) = 'object'),
  credentials jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(credentials) = 'object'),
  secrets jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(secrets) = 'object'),
  endpoints jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(endpoints) = 'array'),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (leg, profile),
  UNIQUE (id, leg),
  CHECK (profile = leg OR profile = 'dual'),
  CHECK (leg = 'emsp' OR name = ''),
  CHECK (leg = 'cpo' OR credentials = '{}'::jsonb)
);

COMMENT ON TABLE partner_console_connections IS 'One row per page and leg: /emsp (emsp, emsp), /cpo (cpo, cpo), /dual (emsp, dual) and (cpo, dual). Reset deletes the row and everything of that connection cascades.';
COMMENT ON COLUMN partner_console_connections.leg IS 'Role the mock plays: emsp or cpo';
COMMENT ON COLUMN partner_console_connections.profile IS 'Page: emsp, cpo or dual';
COMMENT ON COLUMN partner_console_connections.config IS 'Settings typed in the page: URLs, country/party, business name, meter and send cadence';
COMMENT ON COLUMN partner_console_connections.credentials IS 'CPO leg only: Credentials Evolt answered at registration';
COMMENT ON COLUMN partner_console_connections.secrets IS 'OCPI tokens sealed with ENCRYPTION_KEY: {"<key>": {"ciphertext", "updated_at"}}';
COMMENT ON COLUMN partner_console_connections.endpoints IS 'Evolt endpoints from Version Details: [{"identifier", "role", "url", ...}]';

-- ---- objects --------------------------------------------------------------------------------

CREATE TABLE partner_console_objects (
  connection_id text NOT NULL,
  leg text NOT NULL,
  module text NOT NULL CHECK (module IN ('locations', 'tariffs', 'sessions', 'cdrs', 'tokens')),
  country_code text NOT NULL DEFAULT '',
  party_id text NOT NULL DEFAULT '',
  object_id text NOT NULL,
  token_type text NOT NULL DEFAULT '',
  body jsonb NOT NULL CHECK (jsonb_typeof(body) = 'object'),
  last_updated timestamptz,
  snapshot jsonb NOT NULL DEFAULT '{}'::jsonb,
  delivery text NOT NULL DEFAULT '',
  findings jsonb NOT NULL DEFAULT '[]'::jsonb,
  cdr_location text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (connection_id, module, country_code, party_id, object_id, token_type),
  FOREIGN KEY (connection_id, leg) REFERENCES partner_console_connections(id, leg) ON DELETE CASCADE,
  CHECK (token_type = '' OR (leg = 'emsp' AND module = 'tokens')),
  CHECK (leg = 'emsp' OR (country_code = '' AND party_id = '')),
  CHECK (leg = 'cpo' OR (snapshot = '{}'::jsonb AND delivery = '' AND findings = '[]'::jsonb AND cdr_location = ''))
);

COMMENT ON TABLE partner_console_objects IS 'OCPI objects of both legs. eMSP leg: what Evolt pushed or the mock pulled, plus the tokens the mock issues. CPO leg: the catalogue the mock sends to Evolt and the tokens it received.';
COMMENT ON COLUMN partner_console_objects.body IS 'The OCPI object as JSON';
COMMENT ON COLUMN partner_console_objects.token_type IS 'eMSP tokens only: an OCPI token uid is unique per type';
COMMENT ON COLUMN partner_console_objects.snapshot IS 'CPO leg only: Location and Tariff captured when a Session started, for pricing';
COMMENT ON COLUMN partner_console_objects.delivery IS 'CPO leg only: NOT_SENT, SENT, FAILED, RECEIVED, PULLED, ...';
COMMENT ON COLUMN partner_console_objects.cdr_location IS 'CPO leg only: Location header Evolt answered for a sent CDR';

-- ---- commands -------------------------------------------------------------------------------

CREATE TABLE partner_console_commands (
  connection_id text NOT NULL,
  leg text NOT NULL,
  id text NOT NULL,
  type text NOT NULL,
  status text NOT NULL,
  request jsonb NOT NULL,
  response_url text NOT NULL,
  direct_response jsonb,
  async_result jsonb,
  timeout_seconds int,
  result jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (connection_id, id),
  FOREIGN KEY (connection_id, leg) REFERENCES partner_console_connections(id, leg) ON DELETE CASCADE,
  CHECK (leg = 'emsp' OR (direct_response IS NULL AND async_result IS NULL AND timeout_seconds IS NULL)),
  CHECK (leg = 'cpo' OR result IS NULL)
);
CREATE INDEX partner_console_commands_id_idx ON partner_console_commands(id);

COMMENT ON TABLE partner_console_commands IS 'START/STOP commands. eMSP leg: sent to Evolt. CPO leg: received from Evolt.';
COMMENT ON COLUMN partner_console_commands.direct_response IS 'eMSP leg only: Evolt''s immediate answer';
COMMENT ON COLUMN partner_console_commands.async_result IS 'eMSP leg only: the result Evolt posted back later';
COMMENT ON COLUMN partner_console_commands.result IS 'CPO leg only: the result the mock sent back to Evolt';

-- ---- saved requests -------------------------------------------------------------------------

CREATE TABLE partner_console_saved_requests (
  id text PRIMARY KEY,
  lane text NOT NULL CHECK (lane IN ('EMSP', 'CPO', 'DUAL_EMSP', 'DUAL_CPO', 'COMMON')),
  module text NOT NULL,
  name text NOT NULL,
  content text NOT NULL,
  method text NOT NULL DEFAULT 'POST',
  target_url text NOT NULL DEFAULT '',
  headers jsonb NOT NULL DEFAULT '{}'::jsonb,
  use_auth boolean NOT NULL DEFAULT true,
  expected_http integer,
  expected_ocpi integer,
  valid_json boolean NOT NULL DEFAULT true,
  findings jsonb NOT NULL DEFAULT '[]'::jsonb,
  updated_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE partner_console_saved_requests IS 'Requests saved in the Requests panel. Not tied to a connection, so Reset keeps them.';
COMMENT ON COLUMN partner_console_saved_requests.lane IS 'Whose token the request is sent with';

-- ---- exchanges ------------------------------------------------------------------------------

CREATE TABLE partner_console_exchanges (
  seq bigserial PRIMARY KEY,
  connection_id text REFERENCES partner_console_connections(id) ON DELETE CASCADE,
  saved_request_id text REFERENCES partner_console_saved_requests(id) ON DELETE CASCADE,
  exchange_id text,
  direction text,
  module text,
  method text NOT NULL,
  url text NOT NULL,
  http_status int,
  ocpi_status_code int,
  duration_ms bigint NOT NULL DEFAULT 0,
  result text,
  error text,
  request_headers jsonb,
  request_body jsonb,
  response_headers jsonb,
  response_body jsonb,
  findings jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((connection_id IS NULL) <> (saved_request_id IS NULL)),
  CHECK (connection_id IS NULL OR (direction IS NOT NULL AND module IS NOT NULL AND http_status IS NOT NULL AND result IS NULL AND error IS NULL)),
  CHECK (saved_request_id IS NULL OR (result IS NOT NULL AND direction IS NULL AND exchange_id IS NULL AND findings IS NULL))
);
CREATE UNIQUE INDEX partner_console_exchanges_exchange_id_idx ON partner_console_exchanges(connection_id, exchange_id) WHERE exchange_id IS NOT NULL;
CREATE INDEX partner_console_exchanges_connection_idx ON partner_console_exchanges(connection_id, created_at DESC);
CREATE INDEX partner_console_exchanges_saved_request_idx ON partner_console_exchanges(saved_request_id, created_at DESC);

COMMENT ON TABLE partner_console_exchanges IS 'Request/response log: the OCPI traffic of a connection (connection_id), or one send of a saved request (saved_request_id).';
COMMENT ON COLUMN partner_console_exchanges.exchange_id IS 'CPO leg only: the id the mock gave the exchange';
COMMENT ON COLUMN partner_console_exchanges.result IS 'Saved request only: RECORDED, PASS, FAIL or ERROR';
COMMENT ON COLUMN partner_console_exchanges.error IS 'Saved request only: transport error of an ERROR send';
COMMENT ON COLUMN partner_console_exchanges.findings IS 'OCPI conformance findings of a connection exchange';

COMMIT;
