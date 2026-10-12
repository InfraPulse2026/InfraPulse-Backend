CREATE UNIQUE INDEX asset_binding_source_owner_idx ON asset_source_bindings(id,source_instance_id);
CREATE TABLE asset_hardware_samples (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 binding_id text NOT NULL REFERENCES asset_source_bindings(id),
 source_instance_id text NOT NULL REFERENCES data_sources(id),
 observation_key text NOT NULL CHECK(length(observation_key)=64),
 agent_id text NOT NULL,
 message_id text NOT NULL,
 sample_index integer NOT NULL CHECK(sample_index>=0),
 kind text NOT NULL CHECK(kind IN ('inventory','component','health','metric')),
 observed_at timestamptz NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(),
 payload jsonb NOT NULL CHECK(jsonb_typeof(payload)='object'),
 FOREIGN KEY(agent_id,message_id) REFERENCES agent_messages(agent_id,message_id),
	FOREIGN KEY(binding_id,source_instance_id) REFERENCES asset_source_bindings(id,source_instance_id),
	UNIQUE(id,binding_id,source_instance_id,observation_key,kind,observed_at),
 UNIQUE(agent_id,message_id,sample_index)
);
CREATE INDEX asset_hardware_binding_history_idx ON asset_hardware_samples(binding_id,id);
CREATE INDEX asset_hardware_key_history_idx ON asset_hardware_samples(observation_key,id);
CREATE INDEX asset_hardware_source_history_idx ON asset_hardware_samples(source_instance_id,id);
CREATE TABLE asset_hardware_latest (
 observation_key text PRIMARY KEY,
 sample_id bigint NOT NULL UNIQUE REFERENCES asset_hardware_samples(id),
 binding_id text NOT NULL REFERENCES asset_source_bindings(id),
 source_instance_id text NOT NULL REFERENCES data_sources(id),
 kind text NOT NULL,
 observed_at timestamptz NOT NULL,
	FOREIGN KEY(sample_id,binding_id,source_instance_id,observation_key,kind,observed_at) REFERENCES asset_hardware_samples(id,binding_id,source_instance_id,observation_key,kind,observed_at),
 CHECK(kind IN ('inventory','component','health','metric'))
);
CREATE INDEX asset_hardware_latest_binding_idx ON asset_hardware_latest(binding_id,kind,observation_key);
