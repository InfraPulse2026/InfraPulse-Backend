CREATE TABLE log_batches (
    agent_id text NOT NULL,
    message_id text NOT NULL,
    observed_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL,
    source text NOT NULL,
    record_count integer NOT NULL CHECK(record_count BETWEEN 0 AND 1024),
    PRIMARY KEY(agent_id,message_id),
    FOREIGN KEY(agent_id,message_id) REFERENCES agent_messages(agent_id,message_id)
);

CREATE TABLE log_records (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    agent_id text NOT NULL,
    device_id text NOT NULL,
    record_scope text NOT NULL,
    record_id text NOT NULL,
    fingerprint bytea NOT NULL,
    message_id text NOT NULL,
    source text NOT NULL,
    source_id text NOT NULL,
    event_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    original_timestamp timestamptz,
    received_at timestamptz NOT NULL,
    time_basis text NOT NULL,
    severity bigint CHECK(severity BETWEEN 0 AND 4294967295),
    facility bigint CHECK(facility BETWEEN 0 AND 4294967295),
    host text NOT NULL,
    process text NOT NULL,
    process_id text NOT NULL,
    message text NOT NULL,
    raw text NOT NULL,
    truncated boolean NOT NULL,
    redacted boolean NOT NULL,
    file_identity text NOT NULL,
    file_offset bigint NOT NULL,
    search_text text NOT NULL,
    search_vector tsvector GENERATED ALWAYS AS (to_tsvector('simple'::regconfig,search_text)) STORED,
    FOREIGN KEY(agent_id,device_id) REFERENCES devices(agent_id,device_id),
    FOREIGN KEY(agent_id,message_id) REFERENCES agent_messages(agent_id,message_id),
    UNIQUE(agent_id,device_id,record_scope,source,source_id,record_id)
);
CREATE INDEX log_records_device_cursor_idx ON log_records(agent_id,device_id,id DESC);
CREATE INDEX log_records_time_idx ON log_records(event_at DESC,id DESC);
CREATE INDEX log_records_source_idx ON log_records(source,source_id,id DESC);
CREATE INDEX log_records_severity_idx ON log_records(severity,id DESC);
CREATE INDEX log_records_search_idx ON log_records USING gin(search_vector);

CREATE TABLE audit_records (
    log_id bigint PRIMARY KEY REFERENCES log_records(id),
    event_key text NOT NULL,
    event_id text NOT NULL,
    record_type text NOT NULL,
    uid bigint CHECK(uid BETWEEN 0 AND 4294967295),
    auid bigint CHECK(auid BETWEEN 0 AND 4294967295),
    pid bigint CHECK(pid BETWEEN 0 AND 4294967295),
    executable text NOT NULL,
    syscall text NOT NULL,
    success boolean,
    result text NOT NULL,
    host text NOT NULL
);
CREATE INDEX audit_records_event_idx ON audit_records(event_key,log_id DESC);
CREATE INDEX audit_records_event_id_idx ON audit_records(event_id);
CREATE INDEX audit_records_uid_idx ON audit_records(uid,log_id DESC);
CREATE INDEX audit_records_success_idx ON audit_records(success,log_id DESC);
