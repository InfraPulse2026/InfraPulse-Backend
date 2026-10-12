-- Both legacy source=simulated and Phase8 typed provenance are authoritative.
-- This pure function operates on the immutable original envelope, not on the
-- mutable Collector hello or target's global latest pointer.
CREATE FUNCTION asset_voip_scope(payload jsonb, session_id text, target_id text, source text) RETURNS text LANGUAGE sql IMMUTABLE AS $$
 SELECT CASE WHEN source='simulated' OR COALESCE(payload->'telephony'->>'source','')='simulated'
 OR COALESCE(payload->'telephony'->'observation'->'source'->>'provenance','')='simulated'
 OR EXISTS(SELECT 1 FROM jsonb_array_elements(COALESCE(payload->'telephony'->'targets','[]'::jsonb)) item
           WHERE item->>'target_id'=target_id AND item->'observation'->'source'->>'provenance'='simulated')
 THEN session_id ELSE '' END;
$$;
-- A SIP target stays source-owned. Its physical identity is an explicit,
-- reviewed association anchored to a binding, so merges/splits follow it.
CREATE TABLE asset_voip_associations (
 id text PRIMARY KEY,
 binding_id text NOT NULL REFERENCES asset_source_bindings(id),
 source_instance_id text NOT NULL REFERENCES data_sources(id),
 agent_id text NOT NULL,
 target_id text NOT NULL,
 address text NOT NULL CHECK(length(address) BETWEEN 1 AND 1024),
 transport text NOT NULL CHECK(transport IN ('udp','tcp','tls')),
 source text NOT NULL CHECK(length(source) BETWEEN 1 AND 1024),
 scope text NOT NULL,
 service_kind text NOT NULL CHECK(service_kind IN ('pbx','voice_gateway','sip_service')),
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 128),
 enabled boolean NOT NULL,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 reviewed_sample_id bigint NOT NULL,
 reviewed_at timestamptz NOT NULL DEFAULT now(),
 reviewed_by text NOT NULL REFERENCES access_users(id),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(agent_id,target_id,scope),
 FOREIGN KEY(agent_id,target_id) REFERENCES telephony_targets(agent_id,target_id),
 FOREIGN KEY(reviewed_sample_id,agent_id,target_id) REFERENCES telephony_samples(id,agent_id,target_id),
 CHECK(source<>'simulated' OR scope<>'')
);
CREATE INDEX asset_voip_bindings ON asset_voip_associations(binding_id,id);
CREATE TABLE asset_voip_events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 association_id text NOT NULL REFERENCES asset_voip_associations(id),
 actor_id text NOT NULL REFERENCES access_users(id),
 before_value jsonb NOT NULL,
 after_value jsonb NOT NULL,
 reason text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX asset_voip_event_history ON asset_voip_events(association_id,id DESC);
CREATE TABLE asset_voip_operations (
 actor_id text NOT NULL REFERENCES access_users(id),
 idempotency_key text NOT NULL,
 request_hash text NOT NULL,
 response jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(actor_id,idempotency_key)
);
CREATE FUNCTION validate_asset_voip_association() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM asset_source_bindings b JOIN data_sources s ON s.id=NEW.source_instance_id JOIN assets a ON a.id=b.asset_id
  WHERE b.id=NEW.binding_id AND s.type='COLLECTOR' AND s.connection_ref=NEW.agent_id AND b.scope=NEW.scope AND a.lifecycle_status='observed'
  AND ((NEW.scope='' AND a.scope='') OR (NEW.scope<>'' AND b.source_instance_id=NEW.source_instance_id AND b.legacy_agent_id=NEW.agent_id))) THEN
  RAISE EXCEPTION 'VoIP binding ownership mismatch' USING ERRCODE='23514';
 END IF;
 IF NOT EXISTS(SELECT 1 FROM telephony_samples t JOIN agent_messages m ON m.agent_id=t.agent_id AND m.message_id=t.message_id
  WHERE t.id=NEW.reviewed_sample_id AND t.agent_id=NEW.agent_id AND t.target_id=NEW.target_id
  AND t.address=NEW.address AND t.transport=NEW.transport AND t.source=NEW.source AND NOT t.clock_skew
  AND asset_voip_scope(m.payload,m.session_id,t.target_id,t.source)=NEW.scope) THEN
  RAISE EXCEPTION 'VoIP reviewed endpoint mismatch' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER asset_voip_association_guard BEFORE INSERT OR UPDATE ON asset_voip_associations FOR EACH ROW EXECUTE FUNCTION validate_asset_voip_association();
