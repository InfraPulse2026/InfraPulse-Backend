CREATE TABLE agents (
    agent_id text PRIMARY KEY,
    session_id text NOT NULL,
    name text NOT NULL,
    version text NOT NULL,
    local_device_id text NOT NULL,
    protocol_version integer NOT NULL CHECK (protocol_version = 1),
    connected boolean NOT NULL DEFAULT true,
    last_seen timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    hello jsonb NOT NULL,
    heartbeat jsonb
);

CREATE TABLE agent_sessions (
    agent_id text NOT NULL REFERENCES agents(agent_id),
    session_id text NOT NULL,
    started_at timestamptz NOT NULL,
    connected_at timestamptz NOT NULL DEFAULT now(),
    last_seen timestamptz NOT NULL DEFAULT now(),
    disconnected_at timestamptz,
    PRIMARY KEY (agent_id, session_id)
);

-- Device identity belongs to an agent. Equal IDs from two collectors do not overwrite each other.
CREATE TABLE devices (
    agent_id text NOT NULL REFERENCES agents(agent_id),
    device_id text NOT NULL,
    name text NOT NULL,
    hostname text NOT NULL DEFAULT '',
    vendor text NOT NULL DEFAULT '',
    model text NOT NULL DEFAULT '',
    type text NOT NULL DEFAULT 'DEVICE_TYPE_UNSPECIFIED',
    management_address text NOT NULL DEFAULT '',
    source text NOT NULL DEFAULT '',
    online boolean NOT NULL DEFAULT false,
    state_at timestamptz NOT NULL,
    inventory_at timestamptz,
    last_seen timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    inventory jsonb NOT NULL DEFAULT '{}',
    PRIMARY KEY (agent_id, device_id)
);
CREATE INDEX devices_device_id_idx ON devices(device_id);

CREATE TABLE agent_messages (
    agent_id text NOT NULL REFERENCES agents(agent_id),
    message_id text NOT NULL,
    session_id text NOT NULL,
    sequence numeric(20,0) NOT NULL CHECK (sequence > 0),
    sent_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    kind text NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (agent_id, message_id)
);

CREATE TABLE telemetry (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    agent_id text NOT NULL,
    device_id text NOT NULL,
    message_id text NOT NULL,
    sample_index integer NOT NULL,
    observed_at timestamptz NOT NULL,
    collected_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    source text NOT NULL,
    cpu_usage_percent double precision,
    memory_total_bytes numeric(20,0),
    memory_used_bytes numeric(20,0),
    payload jsonb NOT NULL,
    FOREIGN KEY (agent_id, device_id) REFERENCES devices(agent_id, device_id),
    FOREIGN KEY (agent_id, message_id) REFERENCES agent_messages(agent_id, message_id),
    UNIQUE (agent_id, message_id, sample_index)
);
CREATE INDEX telemetry_latest_idx ON telemetry(agent_id, device_id, observed_at DESC, id DESC);
