CREATE TABLE collector_config_state (
 collector_id text PRIMARY KEY REFERENCES agents(agent_id) ON DELETE CASCADE,
 desired_revision bigint NOT NULL DEFAULT 0 CHECK(desired_revision>=0),
 highest_reported_revision numeric(20,0) NOT NULL DEFAULT 0 CHECK(highest_reported_revision>=0),
 applied_revision bigint NOT NULL DEFAULT 0,
 applied_digest text NOT NULL DEFAULT '',
 applied_at timestamptz,
 verified_session_id text NOT NULL DEFAULT '',
 status text NOT NULL DEFAULT 'none',
 monitoring_revision bigint NOT NULL DEFAULT 0,
 status_revision bigint NOT NULL DEFAULT 0,
 error_code text NOT NULL DEFAULT ''
);
CREATE TABLE collector_config_revisions (
 collector_id text NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
 revision bigint NOT NULL CHECK(revision>0),
 digest text NOT NULL CHECK(length(digest)=64),
 snapshot jsonb NOT NULL,
 site text NOT NULL DEFAULT '',
 actor_id text NOT NULL REFERENCES access_users(id),
 reason text NOT NULL,
 idempotency_key text NOT NULL,
 request_hash text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(collector_id,revision),
 UNIQUE(actor_id,idempotency_key)
);
CREATE TABLE collector_config_deliveries (
 collector_id text NOT NULL,
 revision bigint NOT NULL,
 session_id text NOT NULL,
 attempts integer NOT NULL DEFAULT 1,
 status text NOT NULL DEFAULT 'dispatching',
 last_attempt_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(collector_id,revision,session_id),
 FOREIGN KEY(collector_id,revision) REFERENCES collector_config_revisions(collector_id,revision) ON DELETE CASCADE
);
CREATE TABLE collector_config_events (
 id bigserial PRIMARY KEY,
 collector_id text NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
 session_id text NOT NULL,
 revision numeric(20,0) NOT NULL,
 digest text NOT NULL,
 status text NOT NULL,
 correlated boolean NOT NULL,
 error_code text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX collector_config_events_collector_idx ON collector_config_events(collector_id,id DESC);

-- Preserve explicit operator freshness overrides. Derived policies refer only
-- to a successfully applied managed revision and source-owned remote targets.
ALTER TABLE monitoring_policies ADD COLUMN origin text NOT NULL DEFAULT 'manual' CHECK(origin IN ('manual','collector_applied'));
ALTER TABLE monitoring_policies ADD COLUMN managed_revision bigint;
ALTER TABLE telephony_policies ADD COLUMN origin text NOT NULL DEFAULT 'manual' CHECK(origin IN ('manual','collector_applied'));
ALTER TABLE telephony_policies ADD COLUMN managed_revision bigint;
