CREATE TABLE monitoring_policies (
    agent_id text NOT NULL,
    device_id text NOT NULL,
    telemetry_interval_seconds integer NOT NULL CHECK (telemetry_interval_seconds BETWEEN 3 AND 36000),
    inventory_interval_seconds integer NOT NULL CHECK (inventory_interval_seconds BETWEEN 3 AND 36000),
    topology_interval_seconds integer NOT NULL CHECK (topology_interval_seconds BETWEEN 3 AND 36000),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, device_id),
    FOREIGN KEY (agent_id, device_id) REFERENCES devices(agent_id, device_id)
);
