ALTER TABLE incidents DROP CONSTRAINT incident_resource_kind;
ALTER TABLE incidents ADD CONSTRAINT incident_resource_kind CHECK(resource_kind IN ('device','interface','sip','agent','external','hardware','log','configuration'));
ALTER TABLE incidents DROP CONSTRAINT incident_resource_identity;
ALTER TABLE incidents ADD CONSTRAINT incident_resource_identity CHECK(
 (resource_kind NOT IN ('external','configuration') AND agent_id IS NOT NULL AND source_instance_id IS NULL AND external_event_id IS NULL AND (
  (resource_kind='device' AND device_id IS NOT NULL AND resource_id='' AND target_id IS NULL) OR
  (resource_kind IN ('interface','hardware','log') AND device_id IS NOT NULL AND resource_id<>'' AND target_id IS NULL) OR
  (resource_kind='sip' AND device_id IS NULL AND target_id IS NOT NULL AND target_id=resource_id) OR
  (resource_kind='agent' AND device_id IS NULL AND target_id IS NULL AND resource_id=agent_id))) OR
 (resource_kind='external' AND agent_id IS NULL AND device_id IS NULL AND target_id IS NULL AND source_instance_id IS NOT NULL AND external_event_id IS NOT NULL AND resource_id<>'' AND resource_context=source_instance_id) OR
 (resource_kind='configuration' AND agent_id IS NULL AND device_id IS NULL AND target_id IS NULL AND source_instance_id IS NULL AND external_event_id IS NULL AND resource_id<>'' AND resource_context<>''));
CREATE TABLE configuration_signal_pending(asset_id text PRIMARY KEY REFERENCES assets(id),requested_at timestamptz NOT NULL DEFAULT now());
CREATE TABLE configuration_signal_state(asset_id text NOT NULL REFERENCES assets(id),context_id text NOT NULL,result_id bigint NOT NULL REFERENCES configuration_drift_results(id),observed_at timestamptz NOT NULL,PRIMARY KEY(asset_id,context_id));
INSERT INTO configuration_signal_pending(asset_id) SELECT DISTINCT asset_id FROM configuration_drift_results ORDER BY asset_id LIMIT 10000;
