CREATE TABLE asset_resolution_events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 binding_id text NOT NULL REFERENCES asset_source_bindings(id),
 source_asset_id text NOT NULL REFERENCES assets(id),
 target_asset_id text REFERENCES assets(id),
 fact_id bigint NOT NULL REFERENCES asset_fact_versions(id),
 target_fact_ids jsonb NOT NULL DEFAULT '[]',
 source_observed_at timestamptz NOT NULL,
 target_observed_ats jsonb NOT NULL DEFAULT '[]',
 code text NOT NULL CHECK(code IN ('matched','ambiguous','separated','conflict','stale','insufficient_evidence')),
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(binding_id,fact_id) REFERENCES asset_fact_versions(binding_id,id)
);
CREATE INDEX asset_resolution_history ON asset_resolution_events(source_asset_id,id DESC);
CREATE TABLE asset_projection_marks (
 agent_id text NOT NULL,
 message_id text NOT NULL,
 status text NOT NULL CHECK(status IN ('projected','skipped')),
 processed_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(agent_id,message_id),
 FOREIGN KEY(agent_id,message_id) REFERENCES agent_messages(agent_id,message_id)
);
