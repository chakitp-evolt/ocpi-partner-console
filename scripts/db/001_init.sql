-- evolt-ocpi-partner-console: initial schema (PostgreSQL 14+)
-- Run once by hand on an empty database before the first deploy:
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f scripts/db/001_init.sql
-- Every statement is IF NOT EXISTS, so a re-run is a no-op.

BEGIN;

-- ============================================================
-- eMSP mock (/emsp) and the dual-role partner (/dual, profile = 'dual')
-- ============================================================

CREATE TABLE IF NOT EXISTS emsp_connections (
  id text PRIMARY KEY,
  profile text NOT NULL DEFAULT 'emsp',
  name text NOT NULL,
  status text NOT NULL DEFAULT 'SETUP',
  config jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_emsp_connections_profile ON emsp_connections(profile);
CREATE TABLE IF NOT EXISTS emsp_secrets (
  connection_id text NOT NULL REFERENCES emsp_connections(id) ON DELETE CASCADE,
  key text NOT NULL,
  ciphertext text NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (connection_id, key)
);
CREATE TABLE IF NOT EXISTS emsp_endpoints (
  id bigserial PRIMARY KEY,
  connection_id text NOT NULL REFERENCES emsp_connections(id) ON DELETE CASCADE,
  platform text NOT NULL,
  identifier text NOT NULL,
  role text NOT NULL,
  url text NOT NULL,
  raw jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (connection_id, platform, identifier, role, url)
);
CREATE TABLE IF NOT EXISTS emsp_tokens (
  connection_id text NOT NULL REFERENCES emsp_connections(id) ON DELETE CASCADE,
  country_code text NOT NULL,
  party_id text NOT NULL,
  uid text NOT NULL,
  type text NOT NULL,
  body jsonb NOT NULL,
  last_updated timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (connection_id, country_code, party_id, uid, type)
);
CREATE TABLE IF NOT EXISTS emsp_locations (
  connection_id text NOT NULL REFERENCES emsp_connections(id) ON DELETE CASCADE,
  country_code text NOT NULL,
  party_id text NOT NULL,
  object_id text NOT NULL,
  body jsonb NOT NULL,
  last_updated timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (connection_id, country_code, party_id, object_id)
);
CREATE TABLE IF NOT EXISTS emsp_tariffs (
  connection_id text NOT NULL REFERENCES emsp_connections(id) ON DELETE CASCADE,
  country_code text NOT NULL,
  party_id text NOT NULL,
  object_id text NOT NULL,
  body jsonb NOT NULL,
  last_updated timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (connection_id, country_code, party_id, object_id)
);
CREATE TABLE IF NOT EXISTS emsp_sessions (
  connection_id text NOT NULL REFERENCES emsp_connections(id) ON DELETE CASCADE,
  country_code text NOT NULL,
  party_id text NOT NULL,
  object_id text NOT NULL,
  body jsonb NOT NULL,
  last_updated timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (connection_id, country_code, party_id, object_id)
);
CREATE TABLE IF NOT EXISTS emsp_cdrs (
  connection_id text NOT NULL REFERENCES emsp_connections(id) ON DELETE CASCADE,
  country_code text NOT NULL,
  party_id text NOT NULL,
  object_id text NOT NULL,
  body jsonb NOT NULL,
  last_updated timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (connection_id, country_code, party_id, object_id)
);
CREATE TABLE IF NOT EXISTS emsp_commands (
  id text PRIMARY KEY,
  connection_id text NOT NULL REFERENCES emsp_connections(id) ON DELETE CASCADE,
  type text NOT NULL,
  status text NOT NULL,
  request jsonb NOT NULL,
  direct_response jsonb,
  async_result jsonb,
  response_url text NOT NULL,
  timeout_seconds int,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS emsp_exchanges (
  id bigserial PRIMARY KEY,
  connection_id text NOT NULL REFERENCES emsp_connections(id) ON DELETE CASCADE,
  direction text NOT NULL,
  module text NOT NULL,
  method text NOT NULL,
  url text NOT NULL,
  http_status int NOT NULL,
  request_headers jsonb NOT NULL DEFAULT '{}'::jsonb,
  request_body jsonb,
  response_headers jsonb NOT NULL DEFAULT '{}'::jsonb,
  response_body jsonb,
  ocpi_status_code int,
  findings jsonb NOT NULL DEFAULT '[]'::jsonb,
  duration_ms bigint NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_emsp_exchanges_connection_created ON emsp_exchanges(connection_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_emsp_commands_connection_created ON emsp_commands(connection_id, created_at DESC);

-- ============================================================
-- CPO mock (/cpo) and the CPO leg of the dual-role partner
-- ============================================================

CREATE TABLE IF NOT EXISTS cpo_connections (
  id text PRIMARY KEY,
  profile text NOT NULL DEFAULT 'cpo',
  status text NOT NULL,
  config jsonb NOT NULL,
  credentials jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_cpo_connections_profile ON cpo_connections(profile);
CREATE TABLE IF NOT EXISTS cpo_secrets (
  connection_id text NOT NULL REFERENCES cpo_connections(id) ON DELETE CASCADE,
  key text NOT NULL,
  ciphertext text NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (connection_id, key)
);
CREATE TABLE IF NOT EXISTS cpo_endpoints (
  id bigserial PRIMARY KEY,
  connection_id text NOT NULL REFERENCES cpo_connections(id) ON DELETE CASCADE,
  identifier text NOT NULL,
  role text NOT NULL,
  url text NOT NULL,
  UNIQUE (connection_id, identifier, role, url)
);
CREATE TABLE IF NOT EXISTS cpo_objects (
  connection_id text NOT NULL REFERENCES cpo_connections(id) ON DELETE CASCADE,
  module text NOT NULL,
  object_id text NOT NULL,
  body jsonb NOT NULL,
  snapshot jsonb NOT NULL DEFAULT '{}'::jsonb,
  delivery text NOT NULL,
  findings jsonb NOT NULL DEFAULT '[]'::jsonb,
  cdr_location text NOT NULL DEFAULT '',
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (connection_id, module, object_id)
);
CREATE TABLE IF NOT EXISTS cpo_commands (
  connection_id text NOT NULL REFERENCES cpo_connections(id) ON DELETE CASCADE,
  id text NOT NULL,
  type text NOT NULL,
  status text NOT NULL,
  request jsonb NOT NULL,
  response_url text NOT NULL,
  result jsonb,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  PRIMARY KEY (connection_id, id)
);
CREATE TABLE IF NOT EXISTS cpo_exchanges (
  connection_id text NOT NULL REFERENCES cpo_connections(id) ON DELETE CASCADE,
  id text NOT NULL,
  payload jsonb NOT NULL,
  created_at timestamptz NOT NULL,
  PRIMARY KEY (connection_id, id)
);
CREATE INDEX IF NOT EXISTS idx_cpo_exchanges_created ON cpo_exchanges(connection_id, created_at DESC);

-- ============================================================
-- Saved requests (Requests panel on /emsp, /cpo, /dual)
-- ============================================================

CREATE TABLE IF NOT EXISTS request_fixtures (
  id text PRIMARY KEY,
  lane text NOT NULL,
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
CREATE TABLE IF NOT EXISTS request_exchanges (
  id bigserial PRIMARY KEY,
  fixture_id text NOT NULL REFERENCES request_fixtures(id) ON DELETE CASCADE,
  request jsonb NOT NULL,
  response jsonb NOT NULL,
  status text NOT NULL,
  duration_ms bigint NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

COMMIT;
