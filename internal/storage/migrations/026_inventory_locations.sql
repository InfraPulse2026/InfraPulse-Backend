-- Physical placement is a reviewed Backend fact, never inferred from topology.
CREATE TABLE physical_locations (
 id text PRIMARY KEY,
 kind text NOT NULL CHECK(kind IN ('site','building','room','rack')),
 parent_id text REFERENCES physical_locations(id),
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 128),
 rack_units integer NOT NULL DEFAULT 0,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK((kind='site' AND parent_id IS NULL) OR (kind<>'site' AND parent_id IS NOT NULL)),
 CHECK((kind='rack' AND rack_units BETWEEN 1 AND 100) OR (kind<>'rack' AND rack_units=0))
);
CREATE UNIQUE INDEX physical_location_name ON physical_locations(COALESCE(parent_id,''),lower(name));
CREATE INDEX physical_location_children ON physical_locations(parent_id,id);
CREATE TABLE asset_placements (
 asset_id text PRIMARY KEY REFERENCES assets(id),
 mode text NOT NULL CHECK(mode IN ('unplaced','rack','outside_rack')),
 location_id text REFERENCES physical_locations(id),
 start_unit integer NOT NULL DEFAULT 0, height_units integer NOT NULL DEFAULT 0,
 position text NOT NULL DEFAULT '',
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0), updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK((mode='unplaced' AND location_id IS NULL AND start_unit=0 AND height_units=0 AND position='') OR (mode='outside_rack' AND location_id IS NOT NULL AND start_unit=0 AND height_units=0) OR (mode='rack' AND location_id IS NOT NULL AND start_unit>=1 AND height_units>=1 AND start_unit+height_units-1<=100))
);
CREATE INDEX asset_placements_location ON asset_placements(location_id,asset_id);
-- One row for every occupied U. No extension or application-only overlap check.
CREATE TABLE rack_unit_occupancy (
 rack_id text NOT NULL REFERENCES physical_locations(id),
 unit integer NOT NULL CHECK(unit BETWEEN 1 AND 100),
 asset_id text NOT NULL REFERENCES asset_placements(asset_id),
 PRIMARY KEY(rack_id,unit), UNIQUE(asset_id,unit)
);
CREATE TABLE placement_events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 asset_id text NOT NULL REFERENCES assets(id), actor_id text NOT NULL REFERENCES access_users(id),
 before_value jsonb NOT NULL, after_value jsonb NOT NULL, reason text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX placement_event_history ON placement_events(asset_id,id DESC);
CREATE TABLE location_operations (
 actor_id text NOT NULL REFERENCES access_users(id), idempotency_key text NOT NULL,
 request_hash text NOT NULL, response jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(actor_id,idempotency_key)
);
CREATE TABLE physical_location_events (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 location_id text NOT NULL REFERENCES physical_locations(id), actor_id text NOT NULL REFERENCES access_users(id),
 before_value jsonb NOT NULL, after_value jsonb NOT NULL, reason text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX physical_location_event_history ON physical_location_events(location_id,id DESC);
CREATE FUNCTION validate_physical_location() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE parent_kind text;
BEGIN
 IF TG_OP='UPDATE' AND (NEW.kind<>OLD.kind OR NEW.parent_id IS DISTINCT FROM OLD.parent_id) THEN
  RAISE EXCEPTION 'immutable physical location hierarchy' USING ERRCODE='23514';
 END IF;
 IF NEW.kind<>'site' THEN
  SELECT kind INTO parent_kind FROM physical_locations WHERE id=NEW.parent_id;
  IF parent_kind IS DISTINCT FROM (CASE NEW.kind WHEN 'building' THEN 'site' WHEN 'room' THEN 'building' WHEN 'rack' THEN 'room' END) THEN
   RAISE EXCEPTION 'invalid physical location parent' USING ERRCODE='23514';
  END IF;
 END IF;
 IF NEW.kind='rack' AND EXISTS(SELECT 1 FROM rack_unit_occupancy WHERE rack_id=NEW.id AND unit>NEW.rack_units) THEN
  RAISE EXCEPTION 'rack resize would discard occupancy' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER physical_location_guard BEFORE INSERT OR UPDATE ON physical_locations FOR EACH ROW EXECUTE FUNCTION validate_physical_location();
CREATE FUNCTION validate_asset_placement() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE location_kind text; capacity integer;
BEGIN
 IF NOT EXISTS(SELECT 1 FROM assets WHERE id=NEW.asset_id AND lifecycle_status='observed') THEN
  RAISE EXCEPTION 'inactive asset placement' USING ERRCODE='23514';
 END IF;
 IF NEW.mode<>'unplaced' THEN
  SELECT kind,rack_units INTO location_kind,capacity FROM physical_locations WHERE id=NEW.location_id;
  IF (NEW.mode='rack' AND (location_kind IS DISTINCT FROM 'rack' OR NEW.start_unit+NEW.height_units-1>capacity)) OR (NEW.mode='outside_rack' AND location_kind IS DISTINCT FROM 'room') THEN
   RAISE EXCEPTION 'invalid placement destination' USING ERRCODE='23514';
  END IF;
 END IF;
 IF EXISTS(SELECT 1 FROM rack_unit_occupancy WHERE asset_id=NEW.asset_id) THEN
  RAISE EXCEPTION 'release old slots before moving' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER asset_placement_guard BEFORE INSERT OR UPDATE ON asset_placements FOR EACH ROW EXECUTE FUNCTION validate_asset_placement();
CREATE FUNCTION validate_rack_unit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM asset_placements p JOIN physical_locations l ON l.id=p.location_id WHERE p.asset_id=NEW.asset_id AND p.mode='rack' AND p.location_id=NEW.rack_id AND NEW.unit BETWEEN p.start_unit AND p.start_unit+p.height_units-1 AND NEW.unit<=l.rack_units) THEN
  RAISE EXCEPTION 'unit does not belong to reviewed placement' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END; $$;
CREATE TRIGGER rack_unit_guard BEFORE INSERT OR UPDATE ON rack_unit_occupancy FOR EACH ROW EXECUTE FUNCTION validate_rack_unit();
