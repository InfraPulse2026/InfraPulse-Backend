CREATE TABLE telephony_targets (
    agent_id text NOT NULL REFERENCES agents(agent_id),
    target_id text NOT NULL,
    first_received_at timestamptz NOT NULL,
    last_received_at timestamptz NOT NULL,
    latest_sample_id bigint,
    clock_issue_sample_id bigint,
    PRIMARY KEY(agent_id,target_id)
);
CREATE TABLE telephony_batches (
    agent_id text NOT NULL,
    message_id text NOT NULL,
    observed_at timestamptz NOT NULL,
    source text NOT NULL,
    received_at timestamptz NOT NULL,
    target_count integer NOT NULL CHECK(target_count BETWEEN 0 AND 256),
    PRIMARY KEY(agent_id,message_id),
    FOREIGN KEY(agent_id,message_id) REFERENCES agent_messages(agent_id,message_id)
);
CREATE TABLE telephony_samples (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    agent_id text NOT NULL,
    target_id text NOT NULL,
    message_id text NOT NULL,
    address text NOT NULL,
    transport text NOT NULL CHECK(transport IN ('udp','tcp','tls')),
    source text NOT NULL,
    reachable boolean NOT NULL,
    response_code bigint CHECK(response_code BETWEEN 100 AND 699),
    latency_ms double precision CHECK(latency_ms >= 0 AND latency_ms < 'Infinity'::double precision),
    observed_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL,
    consecutive_failures bigint NOT NULL CHECK(consecutive_failures BETWEEN 0 AND 4294967295),
    state text NOT NULL CHECK(state IN ('unknown','up','healthy','degraded','down')),
    error_category text NOT NULL,
    clock_skew boolean NOT NULL,
    UNIQUE(agent_id,message_id,target_id),
    UNIQUE(id,agent_id,target_id),
    FOREIGN KEY(agent_id,target_id) REFERENCES telephony_targets(agent_id,target_id),
    FOREIGN KEY(agent_id,message_id) REFERENCES agent_messages(agent_id,message_id)
);
ALTER TABLE telephony_targets ADD FOREIGN KEY(latest_sample_id,agent_id,target_id) REFERENCES telephony_samples(id,agent_id,target_id);
ALTER TABLE telephony_targets ADD FOREIGN KEY(clock_issue_sample_id,agent_id,target_id) REFERENCES telephony_samples(id,agent_id,target_id);
CREATE INDEX telephony_samples_target_cursor_idx ON telephony_samples(agent_id,target_id,id DESC);
CREATE INDEX telephony_samples_target_time_idx ON telephony_samples(agent_id,target_id,observed_at DESC,id DESC);
CREATE TABLE telephony_policies (
    agent_id text NOT NULL,
    target_id text NOT NULL,
    expected_interval_seconds integer NOT NULL CHECK(expected_interval_seconds BETWEEN 3 AND 86400),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(agent_id,target_id),
    FOREIGN KEY(agent_id,target_id) REFERENCES telephony_targets(agent_id,target_id)
);
