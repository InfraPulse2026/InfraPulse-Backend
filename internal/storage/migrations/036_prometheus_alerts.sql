-- Source-scoped full fingerprint maps to a local event ID. A Prometheus API
-- disappearance is never written as explicit recovery.
CREATE TABLE prometheus_alert_events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 source_id text NOT NULL REFERENCES data_sources(id),
 fingerprint text NOT NULL CHECK(fingerprint ~ '^prom_alert_[a-f0-9]{64}$'),
 active_at timestamptz NOT NULL,
 first_seen timestamptz NOT NULL DEFAULT now(),
 UNIQUE(source_id,fingerprint)
);
