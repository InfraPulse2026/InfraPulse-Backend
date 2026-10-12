-- NEEDS_NUMBER_COORDINATION: not part of Store.Migrate. Do not deploy/merge
-- as an assigned migration until the team reserves a number after 033.
-- Additive B11.2; no historical drift, baseline, job or snapshot is deleted.
ALTER TABLE automation_jobs DROP CONSTRAINT automation_jobs_state_check;
ALTER TABLE automation_jobs ADD CONSTRAINT automation_jobs_state_check
 CHECK(state IN ('queued','running','verifying','succeeded','failed','cancelled','timed_out','verification_failed','unknown','stale','no_action'));
ALTER TABLE automation_jobs DROP CONSTRAINT automation_jobs_check;
ALTER TABLE automation_jobs ADD CONSTRAINT automation_job_claim_state
 CHECK((state IN ('running','verifying'))=(claim_token IS NOT NULL));
DROP INDEX automation_active_asset;
CREATE UNIQUE INDEX automation_active_asset ON automation_jobs(asset_id) WHERE state IN ('queued','running','verifying');
ALTER TABLE automation_jobs ADD CONSTRAINT automation_job_asset_identity UNIQUE(asset_id,id);
ALTER TABLE automation_snapshots DROP CONSTRAINT automation_snapshots_job_id_key;
ALTER TABLE automation_snapshots ADD COLUMN phase text NOT NULL DEFAULT 'result' CHECK(phase IN ('preflight','result'));
ALTER TABLE automation_snapshots ADD CONSTRAINT automation_snapshot_job_phase UNIQUE(job_id,phase);

CREATE TABLE remediation_proposals (
 id text PRIMARY KEY, asset_id text NOT NULL REFERENCES assets(id),
 drift_id bigint NOT NULL REFERENCES configuration_drift_results(id),
 action jsonb NOT NULL, digest text NOT NULL CHECK(digest ~ '^[0-9a-f]{64}$'),
 preview jsonb NOT NULL,
 created_by text NOT NULL REFERENCES access_users(id),
 idempotency_key text NOT NULL, request_hash text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL,
 state text NOT NULL DEFAULT 'proposed' CHECK(state IN ('proposed','queued','running','verifying','succeeded','failed','verification_failed','unknown','reconciled','stale','no_action','rejected','expired','cancelled')),
 approved_by text REFERENCES access_users(id), approved_at timestamptz,
 approved_digest text, approval_request_id text NOT NULL DEFAULT '',
 job_id text UNIQUE, before_snapshot_id text, after_snapshot_id text,
 verification jsonb, error_code text NOT NULL DEFAULT '',
 UNIQUE(created_by,idempotency_key),
 FOREIGN KEY(asset_id,job_id) REFERENCES automation_jobs(asset_id,id),
 FOREIGN KEY(asset_id,before_snapshot_id) REFERENCES automation_snapshots(asset_id,id),
 FOREIGN KEY(asset_id,after_snapshot_id) REFERENCES automation_snapshots(asset_id,id),
 CHECK(action->>'asset_id'=asset_id AND (action->>'drift_id')::bigint=drift_id),
 CHECK(expires_at>created_at),
 CHECK((approved_by IS NULL AND approved_at IS NULL AND approved_digest IS NULL AND job_id IS NULL)
   OR (approved_by IS NOT NULL AND approved_at IS NOT NULL AND approved_digest IS NOT NULL AND approved_digest=digest AND job_id IS NOT NULL))
);
CREATE INDEX remediation_asset_history ON remediation_proposals(asset_id,created_at DESC,id);
CREATE INDEX remediation_pending ON remediation_proposals(expires_at) WHERE state='proposed';
CREATE INDEX remediation_unresolved ON remediation_proposals(asset_id) WHERE state='unknown';

CREATE FUNCTION remediation_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF ROW(NEW.asset_id,NEW.drift_id,NEW.action,NEW.digest,NEW.preview,NEW.created_by,NEW.idempotency_key,NEW.request_hash,NEW.created_at,NEW.expires_at)
  IS DISTINCT FROM ROW(OLD.asset_id,OLD.drift_id,OLD.action,OLD.digest,OLD.preview,OLD.created_by,OLD.idempotency_key,OLD.request_hash,OLD.created_at,OLD.expires_at) THEN
  RAISE EXCEPTION 'immutable remediation action';
 END IF;
 IF OLD.approved_by IS NOT NULL AND ROW(NEW.approved_by,NEW.approved_at,NEW.approved_digest,NEW.approval_request_id,NEW.job_id)
  IS DISTINCT FROM ROW(OLD.approved_by,OLD.approved_at,OLD.approved_digest,OLD.approval_request_id,OLD.job_id) THEN
  RAISE EXCEPTION 'one-time remediation approval';
 END IF;
 IF NEW.state<>OLD.state AND NOT (
  (OLD.state='proposed' AND NEW.state IN ('queued','rejected','expired','stale')) OR
  (OLD.state='queued' AND NEW.state IN ('running','stale','expired','cancelled','unknown')) OR
  (OLD.state='running' AND NEW.state IN ('verifying','stale','no_action','failed','unknown','cancelled')) OR
  (OLD.state='verifying' AND NEW.state IN ('succeeded','verification_failed','unknown','failed')) OR
  (OLD.state='unknown' AND NEW.state='reconciled')
 ) THEN RAISE EXCEPTION 'invalid remediation transition'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER remediation_immutable BEFORE UPDATE ON remediation_proposals
 FOR EACH ROW EXECUTE FUNCTION remediation_immutable();
