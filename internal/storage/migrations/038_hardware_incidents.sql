ALTER TABLE incidents DROP CONSTRAINT incident_resource_kind;
ALTER TABLE incidents ADD CONSTRAINT incident_resource_kind CHECK(resource_kind IN ('device','interface','sip','agent','external','hardware'));
ALTER TABLE incidents DROP CONSTRAINT incident_resource_identity;
ALTER TABLE incidents ADD CONSTRAINT incident_resource_identity CHECK(
 (resource_kind<>'external' AND agent_id IS NOT NULL AND source_instance_id IS NULL AND external_event_id IS NULL AND (
  (resource_kind='device' AND device_id IS NOT NULL AND resource_id='' AND target_id IS NULL) OR
  (resource_kind IN ('interface','hardware') AND device_id IS NOT NULL AND resource_id<>'' AND target_id IS NULL) OR
  (resource_kind='sip' AND device_id IS NULL AND target_id IS NOT NULL AND target_id=resource_id) OR
  (resource_kind='agent' AND device_id IS NULL AND target_id IS NULL AND resource_id=agent_id))) OR
 (resource_kind='external' AND agent_id IS NULL AND device_id IS NULL AND target_id IS NULL AND source_instance_id IS NOT NULL AND external_event_id IS NOT NULL AND resource_id<>'' AND resource_context=source_instance_id));
CREATE TABLE hardware_signal_pending(sample_id bigint PRIMARY KEY REFERENCES asset_hardware_samples(id) ON DELETE CASCADE);
CREATE TABLE hardware_signal_state(
 observation_key text PRIMARY KEY CHECK(length(observation_key)=64),
 last_observed_at timestamptz NOT NULL,
 last_source_at timestamptz,
 last_session_id text NOT NULL,
 recovery_count integer NOT NULL DEFAULT 0 CHECK(recovery_count BETWEEN 0 AND 3),
 recovery_evidence jsonb NOT NULL DEFAULT '[]',
 updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO hardware_signal_pending SELECT l.sample_id FROM asset_hardware_latest l WHERE l.kind<>'metric';
