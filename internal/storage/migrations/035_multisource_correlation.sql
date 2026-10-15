-- B12 keeps legacy incidents immutable and adds a global, Asset-aware layer.
CREATE TABLE correlation_pending (
 incident_id text PRIMARY KEY REFERENCES incidents(id),
 requested_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO correlation_pending(incident_id) SELECT id FROM incidents;
CREATE TABLE correlation_services (
 id text PRIMARY KEY,
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 128),
 kind text NOT NULL CHECK(kind IN ('network','voice','infrastructure','business')),
 enabled boolean NOT NULL DEFAULT true,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 created_by text NOT NULL REFERENCES access_users(id),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX correlation_service_name ON correlation_services(lower(name));
CREATE TABLE correlation_service_assets (
 service_id text NOT NULL REFERENCES correlation_services(id),
 asset_id text NOT NULL REFERENCES assets(id),
 role text NOT NULL CHECK(role IN ('provider','dependency','consumer')),
 PRIMARY KEY(service_id,asset_id)
);
CREATE INDEX correlation_service_assets_asset ON correlation_service_assets(asset_id,service_id);
CREATE TABLE correlation_service_events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 service_id text NOT NULL REFERENCES correlation_services(id),
 actor_id text NOT NULL REFERENCES access_users(id),
 before_value jsonb NOT NULL,
 after_value jsonb NOT NULL,
 reason text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX correlation_service_event_history ON correlation_service_events(service_id,id DESC);
CREATE TABLE correlation_operations (
 actor_id text NOT NULL REFERENCES access_users(id),
 idempotency_key text NOT NULL,
 request_hash text NOT NULL,
 response jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(actor_id,idempotency_key)
);

CREATE TABLE normalized_signals (
 id text PRIMARY KEY,
 incident_id text NOT NULL UNIQUE REFERENCES incidents(id),
 category text NOT NULL CHECK(category IN ('availability','performance','network','telephony','collector','external','hardware','configuration','topology','log','unknown')),
 status text NOT NULL CHECK(status IN ('active','resolved')),
 severity text NOT NULL CHECK(severity IN ('warning','critical')),
 source_kind text NOT NULL,
 source_instance_id text,
 scope text NOT NULL DEFAULT '',
 observed_at timestamptz NOT NULL,
 received_at timestamptz NOT NULL,
 time_quality text NOT NULL CHECK(time_quality IN ('exact','estimated','received_only','skewed')),
 payload jsonb NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX normalized_signals_active_time ON normalized_signals(observed_at DESC,id) WHERE status='active';
CREATE INDEX normalized_signals_source ON normalized_signals(source_kind,source_instance_id,observed_at DESC);
CREATE TABLE normalized_signal_assets (
 signal_id text NOT NULL REFERENCES normalized_signals(id),
 asset_id text NOT NULL REFERENCES assets(id),
 role text NOT NULL CHECK(role IN ('primary','affected','context')),
 evidence jsonb NOT NULL,
 PRIMARY KEY(signal_id,asset_id)
);
CREATE INDEX normalized_signal_assets_asset ON normalized_signal_assets(asset_id,signal_id);

CREATE TABLE correlated_incidents (
 id text PRIMARY KEY,
 state text NOT NULL CHECK(state IN ('open','resolved')),
 severity text NOT NULL CHECK(severity IN ('warning','critical')),
 title text NOT NULL CHECK(length(title) BETWEEN 1 AND 256),
 primary_asset_id text REFERENCES assets(id),
 confidence text NOT NULL CHECK(confidence IN ('low','medium','high')),
 confidence_score integer NOT NULL CHECK(confidence_score BETWEEN 0 AND 100),
 time_confidence text NOT NULL CHECK(time_confidence IN ('low','medium','high')),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 opened_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now(),
 resolved_at timestamptz
);
CREATE INDEX correlated_incidents_list ON correlated_incidents(state,updated_at DESC,id);
CREATE INDEX correlated_incidents_asset ON correlated_incidents(primary_asset_id,state,updated_at DESC);
CREATE TABLE correlated_incident_signals (
 correlated_incident_id text NOT NULL REFERENCES correlated_incidents(id),
 signal_id text NOT NULL UNIQUE REFERENCES normalized_signals(id),
 relation text NOT NULL,
 score integer NOT NULL CHECK(score BETWEEN 0 AND 100),
 evidence jsonb NOT NULL,
 added_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(correlated_incident_id,signal_id)
);
CREATE INDEX correlated_incident_members ON correlated_incident_signals(correlated_incident_id,added_at,signal_id);
CREATE TABLE correlation_evidence (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 correlated_incident_id text NOT NULL REFERENCES correlated_incidents(id),
 left_signal_id text NOT NULL REFERENCES normalized_signals(id),
 right_signal_id text NOT NULL REFERENCES normalized_signals(id),
 kind text NOT NULL CHECK(kind IN ('same_asset','same_service','topology_neighbor','same_site','temporal_proximity','configuration_precedes','recurring_pattern','source_diversity','existing_incident')),
 score integer NOT NULL CHECK(score BETWEEN 0 AND 100),
 details jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(correlated_incident_id,left_signal_id,right_signal_id,kind)
);
CREATE INDEX correlation_evidence_incident ON correlation_evidence(correlated_incident_id,id);
CREATE TABLE correlation_timeline (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 correlated_incident_id text NOT NULL REFERENCES correlated_incidents(id),
 signal_id text REFERENCES normalized_signals(id),
 event_kind text NOT NULL CHECK(event_kind IN ('incident_opened','signal_added','signal_updated','signal_resolved','incident_resolved','incident_reopened')),
 occurred_at timestamptz NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT now(),
 time_quality text NOT NULL CHECK(time_quality IN ('exact','estimated','received_only','skewed')),
 summary text NOT NULL,
 fingerprint text NOT NULL UNIQUE
);
CREATE INDEX correlation_timeline_incident ON correlation_timeline(correlated_incident_id,occurred_at,id);
