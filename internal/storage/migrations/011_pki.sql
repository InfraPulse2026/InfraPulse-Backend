CREATE TABLE pki_bootstrap_tokens (
 id text PRIMARY KEY,
 agent_id text NOT NULL,
 token_hash bytea NOT NULL UNIQUE CHECK(octet_length(token_hash)=32),
 created_by text NOT NULL REFERENCES access_users(id),
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL,
 used_at timestamptz,
 revoked_at timestamptz
);
CREATE INDEX pki_tokens_agent_idx ON pki_bootstrap_tokens(agent_id,created_at DESC);
CREATE TABLE pki_certificates (
 fingerprint text PRIMARY KEY CHECK(length(fingerprint)=64),
 agent_id text NOT NULL,
 serial text NOT NULL,
 not_before timestamptz NOT NULL,
 not_after timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 renewed_from text REFERENCES pki_certificates(fingerprint),
 revoked_at timestamptz,
 CHECK(not_after>not_before)
);
CREATE INDEX pki_certificates_agent_idx ON pki_certificates(agent_id,created_at DESC);
CREATE TABLE pki_audit (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 agent_id text NOT NULL,
 actor_id text REFERENCES access_users(id),
 action text NOT NULL,
 resource text NOT NULL,
 at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX pki_audit_agent_idx ON pki_audit(agent_id,id DESC);
