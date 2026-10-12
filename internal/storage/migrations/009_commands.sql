CREATE TABLE controller_commands (
 id text PRIMARY KEY,
 agent_id text NOT NULL,
 device_id text NOT NULL,
 interface_id text NOT NULL,
 operation text NOT NULL CHECK(operation IN ('enable','disable')),
 expected_admin_up boolean NOT NULL,
 timeout_seconds integer NOT NULL CHECK(timeout_seconds BETWEEN 1 AND 60),
 idempotency_key text NOT NULL,
 actor_id text NOT NULL REFERENCES access_users(id),
 actor_session_hash bytea NOT NULL CHECK(octet_length(actor_session_hash)=32),
 fingerprint bytea NOT NULL,
 state text NOT NULL CHECK(state IN ('queued','dispatching','sent','accepted','running','success','failed','rejected','unsupported','timeout','expired','uncertain')),
 reason text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL,
 deadline_at timestamptz NOT NULL,
 session_id text NOT NULL,
 dispatched_at timestamptz,
 request jsonb NOT NULL,
 wire_payload jsonb NOT NULL,
 result jsonb,
 result_fingerprint bytea,
 UNIQUE(actor_id,idempotency_key),
 FOREIGN KEY(agent_id,device_id) REFERENCES devices(agent_id,device_id)
);
-- One command in-flight per Agent matches its single worker. Uncertainty blocks
-- new mutations until a definitive late result, never an automatic resend.
CREATE UNIQUE INDEX controller_commands_active_agent_idx ON controller_commands(agent_id)
 WHERE state IN ('queued','dispatching','sent','accepted','running','uncertain');
CREATE INDEX controller_commands_page_idx ON controller_commands(created_at DESC,id);
CREATE INDEX controller_commands_queue_idx ON controller_commands(created_at) WHERE state='queued';
CREATE INDEX controller_commands_deadline_idx ON controller_commands(deadline_at) WHERE state IN ('queued','dispatching','sent','accepted','running');
CREATE TABLE command_events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 command_id text NOT NULL REFERENCES controller_commands(id),
 state text NOT NULL,
 reason text NOT NULL,
 at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX command_events_command_idx ON command_events(command_id,id);
CREATE TABLE command_results (
 agent_id text NOT NULL,
 message_id text NOT NULL,
 reported_command_id text NOT NULL,
 command_id text REFERENCES controller_commands(id),
 disposition text NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(),
 payload jsonb NOT NULL,
 PRIMARY KEY(agent_id,message_id),
 FOREIGN KEY(agent_id,message_id) REFERENCES agent_messages(agent_id,message_id)
);
CREATE INDEX command_results_command_idx ON command_results(command_id,received_at);
