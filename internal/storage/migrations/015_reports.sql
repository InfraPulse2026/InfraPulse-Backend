CREATE INDEX incidents_report_period_idx ON incidents(opened_at,resolved_at);
CREATE INDEX telemetry_report_received_idx ON telemetry(received_at,agent_id,source);
CREATE INDEX telephony_report_received_idx ON telephony_samples(received_at,agent_id,source);
CREATE TABLE report_snapshots (
 id text PRIMARY KEY,
 created_at timestamptz NOT NULL DEFAULT now(),
 created_by text NOT NULL REFERENCES access_users(id),
 idempotency_key text NOT NULL,
 request_fingerprint text NOT NULL,
 filter jsonb NOT NULL,
 report jsonb NOT NULL,
 UNIQUE(created_by,idempotency_key)
);
CREATE INDEX report_snapshots_created_idx ON report_snapshots(created_at DESC,id);
