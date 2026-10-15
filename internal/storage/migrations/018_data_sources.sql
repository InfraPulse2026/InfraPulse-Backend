CREATE TABLE data_sources (
 id text PRIMARY KEY,
 type text NOT NULL CHECK(type IN ('COLLECTOR','ZABBIX','PROMETHEUS','ANSIBLE')),
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 128),
 endpoint text NOT NULL DEFAULT '',
 connection_ref text NOT NULL DEFAULT '',
 enabled boolean NOT NULL,
 sync_interval_seconds integer NOT NULL CHECK(sync_interval_seconds BETWEEN 3 AND 86400),
 configuration jsonb NOT NULL CHECK(jsonb_typeof(configuration)='object'),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision > 0),
 credential_ciphertext bytea,
 connection_status text NOT NULL DEFAULT 'unverified' CHECK(connection_status IN ('unverified','disabled','connected','disconnected','adapter_unavailable','error')),
 last_successful_sync timestamptz,
 last_error text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK ((type='COLLECTOR' AND endpoint='' AND connection_ref<>'' AND credential_ciphertext IS NULL)
     OR (type<>'COLLECTOR' AND endpoint<>'' AND connection_ref=''))
);
-- Several independent instances of one adapter are intentional. Collector
-- aliases are unique so one stream cannot pretend to be two independent sources.
CREATE UNIQUE INDEX data_sources_collector_ref ON data_sources(connection_ref) WHERE type='COLLECTOR';
CREATE INDEX data_sources_list ON data_sources(created_at,id);
CREATE TABLE data_source_audit (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 source_id text NOT NULL REFERENCES data_sources(id),
 actor_id text NOT NULL REFERENCES access_users(id),
 action text NOT NULL CHECK(action IN ('created','updated')),
 revision bigint NOT NULL,
 at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX data_source_audit_history ON data_source_audit(source_id,id DESC);
