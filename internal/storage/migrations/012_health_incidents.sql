CREATE INDEX telemetry_health_idx ON telemetry(agent_id,device_id,source,observed_at DESC,id DESC);
CREATE TABLE health_observations (
 agent_id text NOT NULL, device_id text NOT NULL, source text NOT NULL, scope text NOT NULL, rule_id text NOT NULL,
 observed_at timestamptz NOT NULL, evaluated_at timestamptz NOT NULL DEFAULT now(), assessment jsonb NOT NULL,
 PRIMARY KEY(agent_id,device_id,source,scope,rule_id),
 FOREIGN KEY(agent_id,device_id) REFERENCES devices(agent_id,device_id)
);
CREATE TABLE incidents (
 id text PRIMARY KEY, fingerprint text NOT NULL,
 agent_id text NOT NULL, device_id text NOT NULL, source text NOT NULL, scope text NOT NULL, rule_id text NOT NULL,
 occurrence integer NOT NULL, state text NOT NULL CHECK(state IN ('open','acknowledged','resolved')),
 severity text NOT NULL CHECK(severity IN ('warning','critical')),
 opened_at timestamptz NOT NULL DEFAULT now(), last_observed_at timestamptz NOT NULL, updated_at timestamptz NOT NULL DEFAULT now(),
 acknowledged_at timestamptz, acknowledged_by text REFERENCES access_users(id), resolved_at timestamptz, signal jsonb NOT NULL,
 FOREIGN KEY(agent_id,device_id) REFERENCES devices(agent_id,device_id), UNIQUE(fingerprint,occurrence)
);
CREATE UNIQUE INDEX incidents_active_idx ON incidents(fingerprint) WHERE state<>'resolved';
CREATE INDEX incidents_list_idx ON incidents(agent_id,device_id,opened_at DESC,id);
CREATE TABLE incident_transitions (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, incident_id text NOT NULL REFERENCES incidents(id),
 state text NOT NULL, reason text NOT NULL, actor_id text REFERENCES access_users(id), at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX incident_transitions_idx ON incident_transitions(incident_id,id);
CREATE TABLE incident_evidence (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, incident_id text NOT NULL REFERENCES incidents(id),
 kind text NOT NULL, observed_at timestamptz NOT NULL, at timestamptz NOT NULL DEFAULT now(), assessment jsonb NOT NULL
);
CREATE INDEX incident_evidence_idx ON incident_evidence(incident_id,id DESC);
