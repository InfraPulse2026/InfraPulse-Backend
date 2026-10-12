CREATE TABLE global_topology_versions (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 source_instance_id text NOT NULL REFERENCES data_sources(id), scope text NOT NULL DEFAULT '',
 observation_id text NOT NULL, agent_id text NOT NULL, device_id text NOT NULL, message_id text NOT NULL,
 source text NOT NULL, protocol text NOT NULL, observed_at timestamptz NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL,
 fingerprint text NOT NULL, payload jsonb NOT NULL,
 FOREIGN KEY(agent_id,message_id) REFERENCES agent_messages(agent_id,message_id),
 UNIQUE(source_instance_id,scope,observation_id,id)
);
CREATE INDEX global_topology_version_history ON global_topology_versions(source_instance_id,scope,observation_id,id DESC);
CREATE TABLE global_topology_latest (
 source_instance_id text NOT NULL REFERENCES data_sources(id), scope text NOT NULL DEFAULT '',
 observation_id text NOT NULL, version_id bigint NOT NULL,
 agent_id text NOT NULL, device_id text NOT NULL, message_id text NOT NULL,
 source text NOT NULL, protocol text NOT NULL, observed_at timestamptz NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL,
 fingerprint text NOT NULL, payload jsonb NOT NULL, expiry_logged boolean NOT NULL DEFAULT false,
 PRIMARY KEY(source_instance_id,scope,observation_id),
 FOREIGN KEY(source_instance_id,scope,observation_id,version_id) REFERENCES global_topology_versions(source_instance_id,scope,observation_id,id),
 FOREIGN KEY(agent_id,message_id) REFERENCES agent_messages(agent_id,message_id)
);
CREATE INDEX global_topology_expiry ON global_topology_latest(expires_at) WHERE NOT expiry_logged;
CREATE INDEX global_topology_local ON global_topology_latest(source_instance_id,scope,device_id);
CREATE TABLE global_topology_events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 version_id bigint NOT NULL REFERENCES global_topology_versions(id),
 kind text NOT NULL CHECK(kind IN ('observed','changed','revived','expired','imported')),
 occurred_at timestamptz NOT NULL, recorded_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX global_topology_event_version ON global_topology_events(version_id,id);
