-- Prometheus history stays at the source. PostgreSQL keeps only latest snapshots.
CREATE TABLE prometheus_measurements (
 source_id text NOT NULL REFERENCES data_sources(id),item_id text NOT NULL,host_id text NOT NULL,
 source_revision bigint NOT NULL,payload jsonb NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(),last_seen timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(source_id,item_id)
);
CREATE INDEX prometheus_measurements_host_idx ON prometheus_measurements(source_id,host_id);
