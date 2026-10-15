ALTER TABLE incidents DROP CONSTRAINT incident_resource_kind;
ALTER TABLE incidents ADD CONSTRAINT incident_resource_kind CHECK(resource_kind IN ('device','interface','sip','agent','external','hardware','log','configuration','topology'));
ALTER TABLE incidents DROP CONSTRAINT incident_resource_identity;
ALTER TABLE incidents ADD CONSTRAINT incident_resource_identity CHECK(
 (resource_kind NOT IN ('external','configuration') AND agent_id IS NOT NULL AND source_instance_id IS NULL AND external_event_id IS NULL AND (
  (resource_kind='device' AND device_id IS NOT NULL AND resource_id='' AND target_id IS NULL) OR
  (resource_kind IN ('interface','hardware','log','topology') AND device_id IS NOT NULL AND resource_id<>'' AND target_id IS NULL) OR
  (resource_kind='sip' AND device_id IS NULL AND target_id IS NOT NULL AND target_id=resource_id) OR
  (resource_kind='agent' AND device_id IS NULL AND target_id IS NULL AND resource_id=agent_id))) OR
 (resource_kind='external' AND agent_id IS NULL AND device_id IS NULL AND target_id IS NULL AND source_instance_id IS NOT NULL AND external_event_id IS NOT NULL AND resource_id<>'' AND resource_context=source_instance_id) OR
 (resource_kind='configuration' AND agent_id IS NULL AND device_id IS NULL AND target_id IS NULL AND source_instance_id IS NULL AND external_event_id IS NULL AND resource_id<>'' AND resource_context<>''));
CREATE TABLE topology_signal_pending(snapshot_id bigint PRIMARY KEY REFERENCES topology_snapshots(id));
CREATE TABLE topology_signal_state(agent_id text NOT NULL,device_id text NOT NULL,source text NOT NULL,scope text NOT NULL,binding_id text NOT NULL REFERENCES asset_source_bindings(id),snapshot_id bigint NOT NULL REFERENCES topology_snapshots(id),observed_at timestamptz NOT NULL,PRIMARY KEY(agent_id,device_id,source,scope));
CREATE TABLE topology_incident_records(id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,incident_id text NOT NULL REFERENCES incidents(id),binding_id text NOT NULL REFERENCES asset_source_bindings(id),before_snapshot_id bigint NOT NULL REFERENCES topology_snapshots(id),after_snapshot_id bigint NOT NULL REFERENCES topology_snapshots(id),local_port_hash text NOT NULL,observed_at timestamptz NOT NULL,UNIQUE(after_snapshot_id,local_port_hash));
CREATE INDEX topology_incident_records_incident ON topology_incident_records(incident_id,id);
INSERT INTO topology_signal_pending SELECT id FROM topology_snapshots WHERE received_at>now()-interval '5 minutes' AND observed_at>now()-interval '5 minutes' ORDER BY observed_at,id LIMIT 10000;
