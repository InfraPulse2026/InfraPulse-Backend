ALTER TABLE automation_snapshots ADD CONSTRAINT automation_snapshot_asset_identity UNIQUE(asset_id,id);
CREATE TABLE configuration_baseline_versions (
 asset_id text NOT NULL REFERENCES assets(id), revision bigint NOT NULL CHECK(revision>0),
 asset_revision bigint NOT NULL CHECK(asset_revision>0), snapshot_id text NOT NULL,
 read_template_id text NOT NULL, normalizer text NOT NULL,
 raw_digest text NOT NULL, normalized_digest text NOT NULL,
 actor_id text NOT NULL REFERENCES access_users(id), reason text NOT NULL,
 idempotency_key text NOT NULL, request_hash text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(asset_id,revision),
 UNIQUE(actor_id,idempotency_key), FOREIGN KEY(asset_id,snapshot_id) REFERENCES automation_snapshots(asset_id,id)
);
CREATE TABLE configuration_baseline_state (
 asset_id text PRIMARY KEY REFERENCES assets(id), revision bigint NOT NULL,
 FOREIGN KEY(asset_id,revision) REFERENCES configuration_baseline_versions(asset_id,revision)
);
CREATE TABLE configuration_drift_results (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 asset_id text NOT NULL, baseline_revision bigint NOT NULL, actual_snapshot_id text NOT NULL,
 actual_target_revision bigint NOT NULL, normalizer text NOT NULL,
 status text NOT NULL CHECK(status IN ('compliant','drift','unknown')), error_code text NOT NULL,
 comparison jsonb NOT NULL, related_services jsonb NOT NULL,
 actual_observed_at timestamptz NOT NULL, compared_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(asset_id,baseline_revision) REFERENCES configuration_baseline_versions(asset_id,revision),
 FOREIGN KEY(asset_id,actual_snapshot_id) REFERENCES automation_snapshots(asset_id,id),
 UNIQUE(asset_id,baseline_revision,actual_snapshot_id)
);
CREATE INDEX configuration_drift_current ON configuration_drift_results(asset_id,baseline_revision,actual_observed_at DESC,id DESC);
CREATE INDEX configuration_drift_history ON configuration_drift_results(asset_id,id DESC);
