ALTER TABLE incidents DROP CONSTRAINT incident_resource_kind;
ALTER TABLE incidents ADD CONSTRAINT incident_resource_kind CHECK(resource_kind IN ('device','interface','sip','agent','external','hardware','log'));
ALTER TABLE incidents DROP CONSTRAINT incident_resource_identity;
ALTER TABLE incidents ADD CONSTRAINT incident_resource_identity CHECK(
 (resource_kind<>'external' AND agent_id IS NOT NULL AND source_instance_id IS NULL AND external_event_id IS NULL AND (
  (resource_kind='device' AND device_id IS NOT NULL AND resource_id='' AND target_id IS NULL) OR
  (resource_kind IN ('interface','hardware','log') AND device_id IS NOT NULL AND resource_id<>'' AND target_id IS NULL) OR
  (resource_kind='sip' AND device_id IS NULL AND target_id IS NOT NULL AND target_id=resource_id) OR
  (resource_kind='agent' AND device_id IS NULL AND target_id IS NULL AND resource_id=agent_id))) OR
 (resource_kind='external' AND agent_id IS NULL AND device_id IS NULL AND target_id IS NULL AND source_instance_id IS NOT NULL AND external_event_id IS NOT NULL AND resource_id<>'' AND resource_context=source_instance_id));
CREATE TABLE log_signal_pending(log_id bigint PRIMARY KEY REFERENCES log_records(id) ON DELETE CASCADE);
CREATE TABLE log_incident_records(
 log_id bigint PRIMARY KEY REFERENCES log_records(id) ON DELETE CASCADE,
 incident_id text NOT NULL REFERENCES incidents(id),
 binding_id text NOT NULL REFERENCES asset_source_bindings(id)
);
CREATE INDEX log_incident_records_incident ON log_incident_records(incident_id,log_id);
CREATE INDEX log_records_message_idx ON log_records(agent_id,message_id);
INSERT INTO log_signal_pending SELECT id FROM log_records WHERE severity BETWEEN 0 AND 3 AND event_at>now()-interval '5 minutes' AND received_at>now()-interval '5 minutes' ORDER BY id DESC LIMIT 10000;
