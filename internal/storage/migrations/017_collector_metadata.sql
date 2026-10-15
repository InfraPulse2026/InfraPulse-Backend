-- Legacy logs retain NULL metadata. Metadata and inbox share one transaction.
ALTER TABLE log_records ADD COLUMN syslog_metadata jsonb;
ALTER TABLE log_records ADD COLUMN observation_metadata jsonb;
ALTER TABLE log_records ADD COLUMN raw_payload_omitted boolean NOT NULL DEFAULT false;
ALTER TABLE log_records ADD CONSTRAINT log_syslog_metadata_object CHECK (syslog_metadata IS NULL OR jsonb_typeof(syslog_metadata)='object');
ALTER TABLE log_records ADD CONSTRAINT log_observation_metadata_object CHECK (observation_metadata IS NULL OR jsonb_typeof(observation_metadata)='object');
