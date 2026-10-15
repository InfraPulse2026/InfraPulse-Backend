ALTER TABLE assets ADD COLUMN revision bigint NOT NULL DEFAULT 1 CHECK(revision>0);
ALTER TABLE assets ADD COLUMN merged_into_id text REFERENCES assets(id);
CREATE TABLE asset_decisions (
 id text PRIMARY KEY,
 operation text NOT NULL CHECK(operation IN ('merge','split')),
 actor_id text NOT NULL REFERENCES access_users(id),
 idempotency_key text NOT NULL,
 request_hash text NOT NULL,
 target_id text NOT NULL REFERENCES assets(id),
 source_id text NOT NULL REFERENCES assets(id),
 binding_ids jsonb NOT NULL CHECK(jsonb_typeof(binding_ids)='array'),
 canonical_binding_id text NOT NULL,
 reason text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(actor_id,idempotency_key)
);
CREATE INDEX asset_decision_history ON asset_decisions(target_id,created_at DESC);
-- A split is an explicit do-not-merge instruction until a reviewed manual merge.
CREATE TABLE asset_separations (
 asset_a text NOT NULL REFERENCES assets(id),
 asset_b text NOT NULL REFERENCES assets(id),
 decision_id text NOT NULL REFERENCES asset_decisions(id),
 PRIMARY KEY(asset_a,asset_b),
 CHECK(asset_a<asset_b)
);
