-- Independent Ansible queue: never changes Agent envelopes or controller commands.
CREATE TABLE automation_targets (
 asset_id text PRIMARY KEY REFERENCES assets(id), revision bigint NOT NULL CHECK(revision>0),
 definition jsonb NOT NULL, updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE automation_workers (
 id text PRIMARY KEY, available boolean NOT NULL, versions jsonb NOT NULL, templates jsonb NOT NULL,
 error_code text NOT NULL DEFAULT '', last_seen timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE automation_jobs (
 id text PRIMARY KEY, asset_id text NOT NULL REFERENCES assets(id), target_revision bigint NOT NULL,
 template_id text NOT NULL, target jsonb NOT NULL,
 actor_id text NOT NULL REFERENCES access_users(id), session_hash bytea NOT NULL,
 idempotency_key text NOT NULL, request_hash text NOT NULL,
 state text NOT NULL CHECK(state IN ('queued','running','succeeded','failed','cancelled','timed_out')),
 error_code text NOT NULL DEFAULT '', timeout_seconds integer NOT NULL CHECK(timeout_seconds BETWEEN 30 AND 600),
 reason text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), started_at timestamptz, finished_at timestamptz,
 worker_id text REFERENCES automation_workers(id), claim_token text, deadline timestamptz, cancel_requested boolean NOT NULL DEFAULT false,
 snapshot_id text, UNIQUE(actor_id,idempotency_key), CHECK((state='running')=(claim_token IS NOT NULL))
);
CREATE INDEX automation_queue ON automation_jobs(created_at,id) WHERE state='queued';
CREATE UNIQUE INDEX automation_active_asset ON automation_jobs(asset_id) WHERE state IN ('queued','running');
CREATE INDEX automation_job_history ON automation_jobs(asset_id,created_at DESC,id DESC);
CREATE INDEX automation_expiry ON automation_jobs(deadline) WHERE state='running';
CREATE TABLE automation_events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, job_id text NOT NULL REFERENCES automation_jobs(id),
 code text NOT NULL, at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX automation_event_history ON automation_events(job_id,id);
CREATE TABLE automation_snapshots (
 id text PRIMARY KEY, asset_id text NOT NULL REFERENCES assets(id), job_id text NOT NULL UNIQUE REFERENCES automation_jobs(id),
 template_id text NOT NULL, digest text NOT NULL, bytes integer NOT NULL CHECK(bytes BETWEEN 1 AND 4194304),
 ciphertext bytea NOT NULL, observed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX automation_snapshot_history ON automation_snapshots(asset_id,observed_at DESC,id DESC);
ALTER TABLE automation_jobs ADD CONSTRAINT automation_job_snapshot FOREIGN KEY(snapshot_id) REFERENCES automation_snapshots(id);
