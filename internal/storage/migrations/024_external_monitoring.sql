CREATE TABLE external_metric_samples (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 source_id text NOT NULL REFERENCES data_sources(id), item_id text NOT NULL, host_id text NOT NULL,
 source_revision bigint NOT NULL, fingerprint text NOT NULL, payload jsonb NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(), UNIQUE(source_id,item_id,fingerprint)
);
CREATE INDEX external_metric_history_idx ON external_metric_samples(source_id,host_id,item_id,id DESC);
CREATE TABLE external_metrics (
 source_id text NOT NULL REFERENCES data_sources(id), item_id text NOT NULL, host_id text NOT NULL,
 source_revision bigint NOT NULL, latest_sample_id bigint NOT NULL REFERENCES external_metric_samples(id),
 last_seen timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(source_id,item_id)
);
CREATE TABLE external_signals (
 source_id text NOT NULL REFERENCES data_sources(id), event_id text NOT NULL, trigger_id text NOT NULL,
 source_revision bigint NOT NULL, state text NOT NULL CHECK(state IN ('active','resolved')),
 payload jsonb NOT NULL, fingerprint text NOT NULL, incident_id text REFERENCES incidents(id),
 first_seen timestamptz NOT NULL DEFAULT now(), last_seen timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(source_id,event_id)
);
CREATE INDEX external_signals_active_idx ON external_signals(source_id,event_id) WHERE state='active';
CREATE TABLE external_signal_hosts (
 source_id text NOT NULL,event_id text NOT NULL,host_id text NOT NULL,
 PRIMARY KEY(source_id,event_id,host_id),
 FOREIGN KEY(source_id,event_id) REFERENCES external_signals(source_id,event_id)
);
CREATE TABLE external_signal_versions (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,source_id text NOT NULL,event_id text NOT NULL,
 source_revision bigint NOT NULL,fingerprint text NOT NULL,payload jsonb NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(source_id,event_id) REFERENCES external_signals(source_id,event_id)
);
CREATE INDEX external_signal_versions_idx ON external_signal_versions(source_id,event_id,id DESC);
ALTER TABLE incidents ALTER COLUMN agent_id DROP NOT NULL;
ALTER TABLE incidents ADD COLUMN source_instance_id text REFERENCES data_sources(id);
ALTER TABLE incidents ADD COLUMN external_event_id text;
ALTER TABLE incidents ADD FOREIGN KEY(source_instance_id,external_event_id) REFERENCES external_signals(source_id,event_id);
ALTER TABLE incidents DROP CONSTRAINT incident_resource_kind;
ALTER TABLE incidents DROP CONSTRAINT incident_resource_identity;
ALTER TABLE incidents ADD CONSTRAINT incident_resource_kind CHECK(resource_kind IN ('device','interface','sip','agent','external'));
ALTER TABLE incidents ADD CONSTRAINT incident_resource_identity CHECK(
 (resource_kind<>'external' AND agent_id IS NOT NULL AND source_instance_id IS NULL AND external_event_id IS NULL AND (
  (resource_kind='device' AND device_id IS NOT NULL AND resource_id='' AND target_id IS NULL) OR
  (resource_kind='interface' AND device_id IS NOT NULL AND resource_id<>'' AND target_id IS NULL) OR
  (resource_kind='sip' AND device_id IS NULL AND target_id IS NOT NULL AND target_id=resource_id) OR
  (resource_kind='agent' AND device_id IS NULL AND target_id IS NULL AND resource_id=agent_id))) OR
 (resource_kind='external' AND agent_id IS NULL AND device_id IS NULL AND target_id IS NULL AND source_instance_id IS NOT NULL AND external_event_id IS NOT NULL AND resource_id<>'' AND resource_context=source_instance_id)
);
CREATE UNIQUE INDEX external_incident_event_idx ON incidents(source_instance_id,external_event_id) WHERE resource_kind='external';
