ALTER TABLE configuration_drift_results ADD CONSTRAINT configuration_drift_asset_identity UNIQUE(asset_id,id);
CREATE TABLE configuration_remediation_policies (
 asset_id text PRIMARY KEY REFERENCES assets(id), revision bigint NOT NULL CHECK(revision>0),
 enabled boolean NOT NULL, allowed_vlans jsonb NOT NULL, protected_interfaces jsonb NOT NULL,
 max_evidence_age_seconds integer NOT NULL CHECK(max_evidence_age_seconds BETWEEN 60 AND 86400),
 actor_id text NOT NULL REFERENCES access_users(id), reason text NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE configuration_remediation_policy_history (
 asset_id text NOT NULL REFERENCES assets(id), revision bigint NOT NULL CHECK(revision>0),
 enabled boolean NOT NULL, allowed_vlans jsonb NOT NULL, protected_interfaces jsonb NOT NULL,
 max_evidence_age_seconds integer NOT NULL CHECK(max_evidence_age_seconds BETWEEN 60 AND 86400),
 actor_id text NOT NULL REFERENCES access_users(id), reason text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(asset_id,revision)
);
CREATE TABLE configuration_remediations (
 id text PRIMARY KEY, asset_id text NOT NULL REFERENCES assets(id), revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 drift_result_id bigint NOT NULL,
 baseline_revision bigint NOT NULL, actual_snapshot_id text NOT NULL, policy_revision bigint NOT NULL,
 action text NOT NULL CHECK(action='restore_access_vlan'), interface_name text NOT NULL,
 expected_vlan integer NOT NULL CHECK(expected_vlan BETWEEN 1 AND 4094),
 actual_vlan integer NOT NULL CHECK(actual_vlan BETWEEN 1 AND 4094),
 state text NOT NULL CHECK(state IN ('pending_approval','queued','running','cancel_requested','verified','failed','cancelled')),
 error_code text NOT NULL DEFAULT '', requester_id text NOT NULL REFERENCES access_users(id),
 approver_id text REFERENCES access_users(id), reason text NOT NULL, approval_reason text NOT NULL DEFAULT '',
 automation_job_id text UNIQUE REFERENCES automation_jobs(id), verification_drift_result_id bigint REFERENCES configuration_drift_results(id),
 request_idempotency_key text NOT NULL, request_hash text NOT NULL,
 approval_idempotency_key text, approval_hash text,
 created_at timestamptz NOT NULL DEFAULT now(), approved_at timestamptz, finished_at timestamptz,
 UNIQUE(requester_id,request_idempotency_key),
 UNIQUE(approver_id,approval_idempotency_key),
 FOREIGN KEY(asset_id,drift_result_id) REFERENCES configuration_drift_results(asset_id,id),
 FOREIGN KEY(asset_id,baseline_revision) REFERENCES configuration_baseline_versions(asset_id,revision),
 FOREIGN KEY(asset_id,actual_snapshot_id) REFERENCES automation_snapshots(asset_id,id)
);
CREATE INDEX configuration_remediation_asset_history ON configuration_remediations(asset_id,created_at DESC,id DESC);
CREATE UNIQUE INDEX configuration_remediation_active_asset ON configuration_remediations(asset_id) WHERE state IN ('pending_approval','queued','running','cancel_requested');
