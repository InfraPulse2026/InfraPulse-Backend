-- Small current projections; raw observations remain in the existing inbox.
CREATE TABLE availability_sources (
    agent_id text NOT NULL,
    device_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('hello','heartbeat','inventory','telemetry','event_online','event_offline','legacy')),
    clock_skew boolean NOT NULL,
    online boolean NOT NULL,
    observed_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    source text NOT NULL,
    message_id text,
    event_id text NOT NULL DEFAULT '',
    time_basis text NOT NULL CHECK (time_basis IN ('agent_observed','server_receipt')),
    priority integer NOT NULL,
    PRIMARY KEY (agent_id,device_id,kind,clock_skew),
    FOREIGN KEY (agent_id,device_id) REFERENCES devices(agent_id,device_id),
    FOREIGN KEY (agent_id,message_id) REFERENCES agent_messages(agent_id,message_id)
);

CREATE TABLE device_availability_current (
    agent_id text NOT NULL,
    device_id text NOT NULL,
    online boolean NOT NULL,
    observed_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    kind text NOT NULL,
    source text NOT NULL,
    message_id text,
    event_id text NOT NULL DEFAULT '',
    time_basis text NOT NULL,
    priority integer NOT NULL,
    PRIMARY KEY (agent_id,device_id),
    FOREIGN KEY (agent_id,device_id) REFERENCES devices(agent_id,device_id),
    FOREIGN KEY (agent_id,message_id) REFERENCES agent_messages(agent_id,message_id)
);

CREATE TABLE device_availability_changes (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    agent_id text NOT NULL,
    device_id text NOT NULL,
    from_online boolean,
    to_online boolean NOT NULL,
    observed_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    kind text NOT NULL,
    source text NOT NULL,
    message_id text,
    event_id text NOT NULL DEFAULT '',
    time_basis text NOT NULL,
    FOREIGN KEY (agent_id,device_id) REFERENCES devices(agent_id,device_id),
    FOREIGN KEY (agent_id,message_id) REFERENCES agent_messages(agent_id,message_id)
);
CREATE INDEX device_availability_changes_device_idx ON device_availability_changes(agent_id,device_id,id DESC);

CREATE TABLE availability_event_ids (
    agent_id text NOT NULL REFERENCES agents(agent_id),
    event_scope text NOT NULL,
    event_id text NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY(agent_id,event_scope,event_id)
);

-- Preserve known legacy reports. A false protobuf default without last_seen
-- and topology-only placeholders are not negative availability observations.
INSERT INTO availability_sources(agent_id,device_id,kind,clock_skew,online,observed_at,received_at,source,time_basis,priority)
SELECT d.agent_id,d.device_id,'legacy',
       (CASE WHEN d.online THEN d.last_seen ELSE d.state_at END)>now()+interval '30 seconds',
       d.online,CASE WHEN d.online THEN d.last_seen ELSE d.state_at END,
       d.received_at,d.source,'agent_observed',CASE WHEN d.online THEN 0 ELSE 25 END
FROM devices d
WHERE (d.inventory_at IS NOT NULL OR EXISTS(SELECT 1 FROM telemetry t WHERE t.agent_id=d.agent_id AND t.device_id=d.device_id))
  AND (d.online OR d.inventory ? 'last_seen');

INSERT INTO device_availability_current(agent_id,device_id,online,observed_at,received_at,kind,source,time_basis,priority)
SELECT agent_id,device_id,online,observed_at,received_at,kind,source,time_basis,priority
FROM availability_sources WHERE NOT clock_skew;

INSERT INTO device_availability_changes(agent_id,device_id,to_online,observed_at,received_at,kind,source,time_basis)
SELECT agent_id,device_id,online,observed_at,received_at,kind,source,time_basis
FROM device_availability_current;
