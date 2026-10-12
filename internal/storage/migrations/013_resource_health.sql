ALTER TABLE incidents ALTER COLUMN device_id DROP NOT NULL;
ALTER TABLE incidents ADD COLUMN resource_kind text NOT NULL DEFAULT 'device';
ALTER TABLE incidents ADD COLUMN resource_id text NOT NULL DEFAULT '';
ALTER TABLE incidents ADD COLUMN resource_context text NOT NULL DEFAULT '';
ALTER TABLE incidents ADD COLUMN target_id text;
ALTER TABLE incidents ADD CONSTRAINT incident_resource_kind CHECK(resource_kind IN ('device','interface','sip'));
ALTER TABLE incidents ADD CONSTRAINT incident_resource_identity CHECK(
 (resource_kind='device' AND device_id IS NOT NULL AND resource_id='' AND target_id IS NULL) OR
 (resource_kind='interface' AND device_id IS NOT NULL AND resource_id<>'' AND target_id IS NULL) OR
 (resource_kind='sip' AND device_id IS NULL AND resource_id<>'' AND target_id IS NOT NULL AND target_id=resource_id));
ALTER TABLE incidents ADD FOREIGN KEY(agent_id,target_id) REFERENCES telephony_targets(agent_id,target_id);
CREATE INDEX incidents_resource_idx ON incidents(agent_id,resource_kind,resource_id,opened_at DESC);
CREATE TABLE resource_health_observations (
 agent_id text NOT NULL REFERENCES agents(agent_id), device_id text, target_id text,
 resource_kind text NOT NULL CHECK(resource_kind IN ('interface','sip')), resource_id text NOT NULL, resource_context text NOT NULL,
 source text NOT NULL, scope text NOT NULL, rule_id text NOT NULL,
 observed_at timestamptz NOT NULL, evaluated_at timestamptz NOT NULL DEFAULT now(), assessment jsonb NOT NULL,
 PRIMARY KEY(agent_id,resource_kind,resource_id,resource_context,source,scope,rule_id,device_key),
 device_key text GENERATED ALWAYS AS (COALESCE(device_id,'')) STORED,
 FOREIGN KEY(agent_id,device_id) REFERENCES devices(agent_id,device_id),
 FOREIGN KEY(agent_id,target_id) REFERENCES telephony_targets(agent_id,target_id),
 CHECK((resource_kind='interface' AND device_id IS NOT NULL AND target_id IS NULL) OR
       (resource_kind='sip' AND device_id IS NULL AND target_id IS NOT NULL AND target_id=resource_id))
);
CREATE INDEX resource_health_device_idx ON resource_health_observations(agent_id,device_id,evaluated_at DESC);
CREATE INDEX resource_health_target_idx ON resource_health_observations(agent_id,target_id,evaluated_at DESC);
CREATE INDEX telephony_health_source_idx ON telephony_samples(agent_id,target_id,source,observed_at DESC,id DESC);
