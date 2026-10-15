ALTER TABLE controller_commands DROP CONSTRAINT controller_commands_operation_check;
ALTER TABLE controller_commands ADD CONSTRAINT controller_commands_operation_check CHECK(operation IN ('enable','disable','rollback'));
ALTER TABLE controller_commands ADD COLUMN rollback_of_command_id text REFERENCES controller_commands(id);
ALTER TABLE controller_commands ADD COLUMN rollback_snapshot_id text;
CREATE TABLE command_snapshots (
 agent_id text NOT NULL REFERENCES agents(agent_id),
 snapshot_id text NOT NULL,
 origin_command_id text NOT NULL UNIQUE REFERENCES controller_commands(id),
 device_id text NOT NULL,
 interface_id text NOT NULL,
 status text NOT NULL CHECK(status IN ('candidate','reserved','consumed','superseded','uncertain','unverified','unavailable')),
 reason text NOT NULL DEFAULT '',
 initial_observed_admin_up boolean NOT NULL,
 after_admin_up boolean,
 recorded_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz,
 expiry_basis text NOT NULL DEFAULT 'not_transmitted_by_agent',
 previous_state_basis text NOT NULL DEFAULT 'backend_initial_observation_not_agent_snapshot',
 rollback_command_id text REFERENCES controller_commands(id),
 PRIMARY KEY(agent_id,snapshot_id),
 FOREIGN KEY(agent_id,device_id) REFERENCES devices(agent_id,device_id)
);
CREATE INDEX command_snapshots_resource_idx ON command_snapshots(agent_id,device_id,interface_id,recorded_at DESC);
-- Backfill only observed references, never infer an Agent snapshot's TTL/content.
INSERT INTO command_snapshots(agent_id,snapshot_id,origin_command_id,device_id,interface_id,status,initial_observed_admin_up,after_admin_up,recorded_at)
SELECT agent_id,result->>'snapshot_id',id,device_id,interface_id,
 CASE WHEN state='success' AND result->>'uncertain'='false' AND result->'verification'->>'verified'='true' THEN 'candidate' ELSE 'unverified' END,
 expected_admin_up,(result->'verification'->>'observed_admin_up')::boolean,updated_at
FROM controller_commands WHERE result->>'snapshot_id' IS NOT NULL AND result->>'snapshot_id'<>''
ON CONFLICT DO NOTHING;
UPDATE command_snapshots s SET status='superseded',reason='newer_backend_command_reference'
WHERE EXISTS(SELECT 1 FROM command_snapshots n WHERE n.agent_id=s.agent_id AND n.device_id=s.device_id AND n.interface_id=s.interface_id AND (n.recorded_at,n.origin_command_id)>(s.recorded_at,s.origin_command_id));
