ALTER TABLE incidents DROP CONSTRAINT incident_resource_kind;
ALTER TABLE incidents DROP CONSTRAINT incident_resource_identity;
ALTER TABLE incidents ADD CONSTRAINT incident_resource_kind CHECK(resource_kind IN ('device','interface','sip','agent'));
ALTER TABLE incidents ADD CONSTRAINT incident_resource_identity CHECK(
 (resource_kind='device' AND device_id IS NOT NULL AND resource_id='' AND target_id IS NULL) OR
 (resource_kind='interface' AND device_id IS NOT NULL AND resource_id<>'' AND target_id IS NULL) OR
 (resource_kind='sip' AND device_id IS NULL AND target_id IS NOT NULL AND target_id=resource_id) OR
 (resource_kind='agent' AND device_id IS NULL AND target_id IS NULL AND resource_id=agent_id));
ALTER TABLE resource_health_observations DROP CONSTRAINT resource_health_observations_resource_kind_check;
ALTER TABLE resource_health_observations DROP CONSTRAINT resource_health_observations_check;
ALTER TABLE resource_health_observations ADD CONSTRAINT resource_health_observations_resource_kind_check CHECK(resource_kind IN ('interface','sip','agent'));
ALTER TABLE resource_health_observations ADD CONSTRAINT resource_health_observations_check CHECK(
 (resource_kind='interface' AND device_id IS NOT NULL AND target_id IS NULL) OR
 (resource_kind='sip' AND device_id IS NULL AND target_id IS NOT NULL AND target_id=resource_id) OR
 (resource_kind='agent' AND device_id IS NULL AND target_id IS NULL AND resource_id=agent_id));
CREATE TABLE interface_flap_trackers (
 agent_id text NOT NULL, device_id text NOT NULL, interface_id text NOT NULL, source text NOT NULL, scope text NOT NULL,
 epoch text NOT NULL, boot_key text NOT NULL, state text NOT NULL, observed_at timestamptz NOT NULL,
 baseline_at timestamptz NOT NULL, last_change_at timestamptz,
 PRIMARY KEY(agent_id,device_id,interface_id,source,scope),
 FOREIGN KEY(agent_id,device_id) REFERENCES devices(agent_id,device_id)
);
CREATE TABLE interface_state_changes (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 agent_id text NOT NULL, device_id text NOT NULL, interface_id text NOT NULL, source text NOT NULL, scope text NOT NULL,
 epoch text NOT NULL, telemetry_id bigint NOT NULL REFERENCES telemetry(id),
 from_state text NOT NULL, to_state text NOT NULL, observed_at timestamptz NOT NULL,
 UNIQUE(agent_id,device_id,interface_id,source,scope,telemetry_id),
 FOREIGN KEY(agent_id,device_id) REFERENCES devices(agent_id,device_id)
);
CREATE INDEX interface_flap_window_idx ON interface_state_changes(agent_id,device_id,interface_id,source,scope,epoch,observed_at DESC);
CREATE TABLE collector_health_trackers (
 agent_id text PRIMARY KEY REFERENCES agents(agent_id),
 loss_since timestamptz, recovery_since timestamptz,
 session_id text NOT NULL, observed_at timestamptz NOT NULL
);
