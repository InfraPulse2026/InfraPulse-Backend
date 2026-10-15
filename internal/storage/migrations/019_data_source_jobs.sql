ALTER TABLE data_sources ADD COLUMN next_sync_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE data_sources ADD COLUMN sync_cursor jsonb NOT NULL DEFAULT '{}' CHECK(jsonb_typeof(sync_cursor)='object');
ALTER TABLE data_sources ADD COLUMN sync_failures integer NOT NULL DEFAULT 0 CHECK(sync_failures>=0);
CREATE TABLE data_source_jobs (
 id text PRIMARY KEY,
 source_id text NOT NULL REFERENCES data_sources(id),
 operation text NOT NULL CHECK(operation IN ('test','sync')),
 trigger text NOT NULL CHECK(trigger IN ('manual','schedule')),
 state text NOT NULL CHECK(state IN ('queued','running','retry_wait','succeeded','failed','cancelled')),
 source_revision bigint NOT NULL,
 idempotency_key text,
 actor_id text REFERENCES access_users(id),
 actor_session_hash bytea,
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 3),
 code text NOT NULL DEFAULT '',
 lease_token text,
 lease_until timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 started_at timestamptz,
 finished_at timestamptz,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 CHECK((trigger='manual' AND actor_id IS NOT NULL AND actor_session_hash IS NOT NULL AND octet_length(actor_session_hash)=32 AND idempotency_key IS NOT NULL)
    OR (trigger='schedule' AND actor_id IS NULL AND actor_session_hash IS NULL AND idempotency_key IS NULL)),
 CHECK((state='running' AND lease_token IS NOT NULL AND lease_until IS NOT NULL)
    OR (state<>'running' AND lease_token IS NULL AND lease_until IS NULL))
);
CREATE UNIQUE INDEX data_source_one_active_job ON data_source_jobs(source_id) WHERE state IN ('queued','running','retry_wait');
CREATE UNIQUE INDEX data_source_job_idempotency ON data_source_jobs(source_id,operation,idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX data_source_job_history ON data_source_jobs(source_id,created_at DESC,id);
CREATE TABLE data_source_job_events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 job_id text NOT NULL REFERENCES data_source_jobs(id),
 attempt integer NOT NULL,
 state text NOT NULL,
 code text NOT NULL,
 at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX data_source_job_event_history ON data_source_job_events(job_id,id);
