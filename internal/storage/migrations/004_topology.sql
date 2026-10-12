-- Projections commit in the same transaction as the durable Agent inbox.
CREATE TABLE topology_snapshots (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    agent_id text NOT NULL,
    message_id text NOT NULL,
    device_id text NOT NULL,
    source text NOT NULL,
    observed_at timestamptz NOT NULL,
    collected_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    completeness text NOT NULL,
    ttl_seconds integer NOT NULL CHECK (ttl_seconds BETWEEN 0 AND 86400),
    expiry_assumption text NOT NULL,
    observation_count integer NOT NULL CHECK (observation_count BETWEEN 0 AND 10000),
    payload jsonb NOT NULL,
    FOREIGN KEY (agent_id,device_id) REFERENCES devices(agent_id,device_id),
    FOREIGN KEY (agent_id,message_id) REFERENCES agent_messages(agent_id,message_id),
    UNIQUE (agent_id,message_id)
);
CREATE INDEX topology_snapshots_latest_idx ON topology_snapshots(agent_id,device_id,source,observed_at DESC,id DESC);

CREATE TABLE topology_observations (
    agent_id text NOT NULL,
    edge_id text NOT NULL,
    device_id text NOT NULL,
    message_id text NOT NULL,
    source text NOT NULL,
    protocol text NOT NULL,
    observed_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL CHECK (expires_at >= observed_at),
    payload jsonb NOT NULL,
    PRIMARY KEY (agent_id,edge_id),
    FOREIGN KEY (agent_id,device_id) REFERENCES devices(agent_id,device_id),
    FOREIGN KEY (agent_id,message_id) REFERENCES agent_messages(agent_id,message_id)
);
CREATE INDEX topology_observations_expiry_idx ON topology_observations(agent_id,expires_at,device_id);
