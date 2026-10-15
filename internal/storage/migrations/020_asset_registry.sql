-- Additive inventory layer. Legacy device/telemetry primary keys remain intact.
ALTER TABLE data_source_audit ALTER COLUMN actor_id DROP NOT NULL;
CREATE TABLE assets (
 id text PRIMARY KEY,
 kind text NOT NULL CHECK(kind IN ('host','network_device','system','bmc','chassis','component','unknown')),
 canonical_name text NOT NULL,
 vendor text NOT NULL DEFAULT '',
 model text NOT NULL DEFAULT '',
 serial text NOT NULL DEFAULT '',
 hardware_revision text NOT NULL DEFAULT '',
 software_version text NOT NULL DEFAULT '',
 firmware_version text NOT NULL DEFAULT '',
 lifecycle_status text NOT NULL DEFAULT 'observed' CHECK(lifecycle_status IN ('observed','retired','merged')),
 resolution_status text NOT NULL DEFAULT 'unresolved' CHECK(resolution_status IN ('unresolved','resolved','conflict')),
 scope text NOT NULL DEFAULT '',
 canonical_binding_id text,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX assets_page ON assets(created_at,id);
CREATE TABLE asset_source_bindings (
 id text PRIMARY KEY,
 asset_id text NOT NULL REFERENCES assets(id),
 source_instance_id text NOT NULL REFERENCES data_sources(id),
 external_type text NOT NULL,
 external_id text NOT NULL,
 confidence text NOT NULL DEFAULT 'source_owned',
 provenance text NOT NULL CHECK(provenance IN ('real','simulated','manual','external')),
 scope text NOT NULL DEFAULT '',
 legacy_agent_id text,
 legacy_device_id text,
 first_seen timestamptz NOT NULL DEFAULT now(),
 last_seen timestamptz NOT NULL DEFAULT now(),
 inventory_at timestamptz NOT NULL,
 inventory jsonb NOT NULL,
 latest_fingerprint text NOT NULL,
 latest_fact_id bigint,
 FOREIGN KEY(legacy_agent_id,legacy_device_id) REFERENCES devices(agent_id,device_id),
 CHECK((legacy_agent_id IS NULL AND legacy_device_id IS NULL) OR (legacy_agent_id IS NOT NULL AND legacy_device_id IS NOT NULL)),
 CHECK((provenance='simulated' AND scope<>'') OR (provenance<>'simulated' AND scope='')),
 UNIQUE(source_instance_id,external_type,external_id,scope),
 UNIQUE(asset_id,id)
);
CREATE INDEX asset_bindings_asset ON asset_source_bindings(asset_id,id);
CREATE TABLE asset_fact_versions (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 binding_id text NOT NULL REFERENCES asset_source_bindings(id),
 observed_at timestamptz NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(),
 origin text NOT NULL,
 origin_ref text NOT NULL,
 fingerprint text NOT NULL,
 inventory jsonb NOT NULL,
 UNIQUE(binding_id,id)
);
CREATE INDEX asset_fact_history ON asset_fact_versions(binding_id,id DESC);
ALTER TABLE asset_source_bindings ADD CONSTRAINT asset_latest_fact FOREIGN KEY(id,latest_fact_id) REFERENCES asset_fact_versions(binding_id,id);
ALTER TABLE assets ADD CONSTRAINT asset_canonical_binding FOREIGN KEY(id,canonical_binding_id) REFERENCES asset_source_bindings(asset_id,id);
CREATE TABLE asset_identity_evidence (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 binding_id text NOT NULL REFERENCES asset_source_bindings(id),
 fact_id bigint NOT NULL REFERENCES asset_fact_versions(id),
 kind text NOT NULL,
 value text NOT NULL,
 normalized_value text NOT NULL,
 confidence text NOT NULL,
 subject_id text NOT NULL DEFAULT '',
 usable boolean NOT NULL,
 evidence jsonb,
 FOREIGN KEY(binding_id,fact_id) REFERENCES asset_fact_versions(binding_id,id)
);
CREATE INDEX asset_identity_lookup ON asset_identity_evidence(kind,normalized_value) WHERE usable AND subject_id='';
CREATE INDEX asset_identity_history ON asset_identity_evidence(binding_id,fact_id,id);
