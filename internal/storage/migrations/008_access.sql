CREATE TABLE access_users (
 id text PRIMARY KEY,
 username text NOT NULL UNIQUE,
 role text NOT NULL CHECK(role IN ('viewer','operator','admin')),
 password_hash text NOT NULL,
 disabled boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 last_login_at timestamptz
);
CREATE TABLE access_sessions (
 token_hash bytea PRIMARY KEY CHECK(octet_length(token_hash)=32),
 user_id text NOT NULL REFERENCES access_users(id),
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL
);
CREATE INDEX access_sessions_user_idx ON access_sessions(user_id,created_at DESC);
CREATE INDEX access_sessions_expiry_idx ON access_sessions(expires_at);
CREATE TABLE access_audit (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 actor_id text REFERENCES access_users(id),
 target_id text REFERENCES access_users(id),
 action text NOT NULL,
 outcome text NOT NULL,
 resource text NOT NULL DEFAULT '',
 request_id text NOT NULL,
 peer text NOT NULL,
 at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX access_audit_time_idx ON access_audit(id DESC);
