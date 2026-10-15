CREATE TABLE ai_previews (
 id text PRIMARY KEY, incident_id text NOT NULL REFERENCES incidents(id),
 created_by text NOT NULL REFERENCES access_users(id),created_at timestamptz NOT NULL DEFAULT now(),expires_at timestamptz NOT NULL,
 request_hash text NOT NULL, model text NOT NULL, preview jsonb NOT NULL
);
CREATE INDEX ai_previews_actor_idx ON ai_previews(created_by,created_at DESC);
CREATE TABLE ai_analyses (
 id text PRIMARY KEY, incident_id text NOT NULL REFERENCES incidents(id),preview_id text NOT NULL REFERENCES ai_previews(id),
 created_by text NOT NULL REFERENCES access_users(id),actor_session_hash bytea NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),
 state text NOT NULL CHECK(state IN ('queued','running','succeeded','failed','uncertain')),
 failure text NOT NULL DEFAULT '',model text NOT NULL,request_hash text NOT NULL,
 idempotency_key text NOT NULL, request_fingerprint text NOT NULL,result jsonb,
 UNIQUE(created_by,idempotency_key),UNIQUE(preview_id)
);
CREATE INDEX ai_analyses_incident_idx ON ai_analyses(incident_id,created_at DESC,id);
CREATE UNIQUE INDEX ai_analyses_active_actor_idx ON ai_analyses(created_by) WHERE state IN ('queued','running');
