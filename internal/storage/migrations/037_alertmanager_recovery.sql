ALTER TABLE prometheus_alert_events ADD COLUMN label_fingerprint text;
ALTER TABLE prometheus_alert_events ADD COLUMN target_id text;
ALTER TABLE prometheus_alert_events ADD COLUMN source_revision bigint;
ALTER TABLE prometheus_alert_events ADD CONSTRAINT prometheus_alert_event_identity UNIQUE(source_id,id);
CREATE INDEX prometheus_alert_family ON prometheus_alert_events(source_id,label_fingerprint,active_at);
CREATE TABLE alertmanager_receivers(
 source_id text PRIMARY KEY REFERENCES data_sources(id),revision bigint NOT NULL CHECK(revision>0),source_revision bigint NOT NULL,
 enabled boolean NOT NULL,token_hash bytea NOT NULL CHECK(octet_length(token_hash)=32),expires_at timestamptz NOT NULL,
 updated_by text NOT NULL REFERENCES access_users(id),updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE alertmanager_receipts(
 source_id text NOT NULL REFERENCES data_sources(id),event_id bigint NOT NULL,
 starts_at timestamptz NOT NULL,ends_at timestamptz NOT NULL,received_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(source_id,event_id,starts_at,ends_at),
 FOREIGN KEY(source_id,event_id) REFERENCES prometheus_alert_events(source_id,id)
);
