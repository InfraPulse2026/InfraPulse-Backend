package storage

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/locations"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const locationColumns = `id,kind,parent_id,name,rack_units,revision,created_at,updated_at`

func locationError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var p *pgconn.PgError
	if errors.As(err, &p) && (p.Code == "23505" || p.Code == "23514" || p.Code == "23503") {
		return locations.ErrConflict
	}
	return err
}
func readLocation(row interface{ Scan(...any) error }) (locations.Location, error) {
	var v locations.Location
	err := row.Scan(&v.ID, &v.Kind, &v.ParentID, &v.Name, &v.RackUnits, &v.Revision, &v.CreatedAt, &v.UpdatedAt)
	return v, locationError(err)
}
func (s *Store) Locations(ctx context.Context, kind, parent string, limit, offset int) ([]locations.Location, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+locationColumns+` FROM physical_locations WHERE ($1='' OR kind=$1) AND ($2='' OR parent_id=$2) ORDER BY lower(name),id LIMIT $3 OFFSET $4`, kind, parent, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []locations.Location{}
	for rows.Next() {
		v, e := readLocation(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) Location(ctx context.Context, id string) (locations.Location, error) {
	return readLocation(s.pool.QueryRow(ctx, `SELECT `+locationColumns+` FROM physical_locations WHERE id=$1`, id))
}

func priorLocationOperation(ctx context.Context, tx pgx.Tx, actor, key, hash string, out any) (bool, error) {
	var oldHash string
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT request_hash,response FROM location_operations WHERE actor_id=$1 AND idempotency_key=$2`, actor, key).Scan(&oldHash, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if oldHash != hash {
		return false, locations.ErrConflict
	}
	return true, json.Unmarshal(raw, out)
}
func saveLocationOperation(ctx context.Context, tx pgx.Tx, actor access.Principal, key, hash, action, resource string, out any) error {
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO location_operations(actor_id,idempotency_key,request_hash,response) VALUES($1,$2,$3,$4)`, actor.User.ID, key, hash, raw); err != nil {
		return err
	}
	meta := access.CurrentMeta(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO access_audit(actor_id,action,outcome,resource,request_id,peer) VALUES($1,$2,'success',$3,$4,$5)`, actor.User.ID, action, resource, meta.RequestID, meta.Peer)
	return err
}
func (s *Store) SaveLocation(ctx context.Context, id string, in locations.LocationInput, key string) (locations.Location, bool, error) {
	var out locations.Location
	if !locations.ValidLocation(in) || commands.ValidateKey(key) != nil || (id == "" && in.ExpectedRevision != 0) || (id != "" && in.ExpectedRevision < 1) {
		return out, false, locations.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback(context.Background())
	actor, err := assetDecisionLock(ctx, tx)
	if err != nil {
		return out, false, err
	}
	hash := decisionRequestHash("location:"+id, in)
	if exists, e := priorLocationOperation(ctx, tx, actor.User.ID, key, hash, &out); e != nil || exists {
		return out, exists, e
	}
	var before *locations.Location
	if id == "" {
		id, err = assetRegistryID("loc_")
		if err != nil {
			return out, false, err
		}
	} else {
		old, e := readLocation(tx.QueryRow(ctx, `SELECT `+locationColumns+` FROM physical_locations WHERE id=$1 FOR UPDATE`, id))
		if e != nil {
			return out, false, e
		}
		if old.Revision != in.ExpectedRevision || old.Kind != in.Kind || !sameOptional(old.ParentID, in.ParentID) {
			return out, false, locations.ErrConflict
		}
		before = &old
	}
	if in.ParentID != nil {
		p, e := readLocation(tx.QueryRow(ctx, `SELECT `+locationColumns+` FROM physical_locations WHERE id=$1`, *in.ParentID))
		if e != nil {
			return out, false, e
		}
		expected := map[string]string{"building": "site", "room": "building", "rack": "room"}[in.Kind]
		if p.Kind != expected {
			return out, false, locations.ErrConflict
		}
	}
	if in.Kind == "rack" {
		var max int
		if err = tx.QueryRow(ctx, `SELECT COALESCE(max(unit),0) FROM rack_unit_occupancy WHERE rack_id=$1`, id).Scan(&max); err != nil {
			return out, false, err
		}
		if max > in.RackUnits {
			return out, false, locations.ErrConflict
		}
	}
	if in.ExpectedRevision == 0 {
		out, err = readLocation(tx.QueryRow(ctx, `INSERT INTO physical_locations(id,kind,parent_id,name,rack_units) VALUES($1,$2,$3,$4,$5) RETURNING `+locationColumns, id, in.Kind, in.ParentID, in.Name, in.RackUnits))
	} else {
		out, err = readLocation(tx.QueryRow(ctx, `UPDATE physical_locations SET name=$2,rack_units=$3,revision=revision+1,updated_at=now() WHERE id=$1 RETURNING `+locationColumns, id, in.Name, in.RackUnits))
	}
	if err != nil {
		return out, false, err
	}
	b, _ := json.Marshal(before)
	v, _ := json.Marshal(out)
	if _, err = tx.Exec(ctx, `INSERT INTO physical_location_events(location_id,actor_id,before_value,after_value,reason) VALUES($1,$2,$3,$4,$5)`, id, actor.User.ID, b, v, logbook.SanitizeText(in.Reason)); err != nil {
		return out, false, err
	}
	if err = saveLocationOperation(ctx, tx, actor, key, hash, "location_saved", id, out); err != nil {
		return out, false, err
	}
	return out, false, tx.Commit(ctx)
}
func sameOptional(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
func placementTx(ctx context.Context, tx pgx.Tx, id string) (locations.Placement, error) {
	out := locations.Placement{AssetID: id, Mode: "unplaced", Path: []locations.Location{}}
	err := tx.QueryRow(ctx, `SELECT mode,location_id,start_unit,height_units,position,revision,updated_at FROM asset_placements WHERE asset_id=$1`, id).Scan(&out.Mode, &out.LocationID, &out.StartUnit, &out.HeightUnits, &out.Position, &out.Revision, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if out.LocationID != nil {
		next := out.LocationID
		for next != nil && len(out.Path) < 4 {
			v, e := readLocation(tx.QueryRow(ctx, `SELECT `+locationColumns+` FROM physical_locations WHERE id=$1`, *next))
			if e != nil {
				return out, e
			}
			out.Path = append(out.Path, v)
			next = v.ParentID
		}
		if next != nil {
			return out, locations.ErrConflict
		}
		for i, j := 0, len(out.Path)-1; i < j; i, j = i+1, j-1 {
			out.Path[i], out.Path[j] = out.Path[j], out.Path[i]
		}
	}
	return out, nil
}
func (s *Store) AssetPlacement(ctx context.Context, id string) (locations.Placement, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return locations.Placement{}, err
	}
	defer tx.Rollback(context.Background())
	if _, err = readAsset(tx.QueryRow(ctx, `SELECT `+assetColumns+` FROM assets a WHERE id=$1`, id)); err != nil {
		return locations.Placement{}, err
	}
	out, err := placementTx(ctx, tx, id)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
func (s *Store) PlaceAssets(ctx context.Context, in locations.BatchInput, key string) ([]locations.Placement, bool, error) {
	out := []locations.Placement{}
	if !locations.ValidBatch(in) || commands.ValidateKey(key) != nil {
		return out, false, locations.ErrInvalid
	}
	// Sorting gives stable idempotency and a deterministic lock order for imports.
	in.Placements = append([]locations.PlacementInput{}, in.Placements...)
	sort.Slice(in.Placements, func(i, j int) bool { return in.Placements[i].AssetID < in.Placements[j].AssetID })
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback(context.Background())
	actor, err := assetDecisionLock(ctx, tx)
	if err != nil {
		return out, false, err
	}
	hash := decisionRequestHash("placements", in)
	if exists, e := priorLocationOperation(ctx, tx, actor.User.ID, key, hash, &out); e != nil || exists {
		return out, exists, e
	}
	before := map[string]locations.Placement{}
	ids := []string{}
	for _, p := range in.Placements {
		a, e := readAsset(tx.QueryRow(ctx, `SELECT `+assetColumns+` FROM assets a WHERE id=$1 FOR UPDATE`, p.AssetID))
		if e != nil {
			return out, false, e
		}
		if a.LifecycleStatus != "observed" {
			return out, false, locations.ErrConflict
		}
		old, e := placementTx(ctx, tx, p.AssetID)
		if e != nil {
			return out, false, e
		}
		if old.Revision != p.ExpectedRevision {
			return out, false, locations.ErrConflict
		}
		before[p.AssetID] = old
		ids = append(ids, p.AssetID)
		if p.LocationID != nil {
			loc, e := readLocation(tx.QueryRow(ctx, `SELECT `+locationColumns+` FROM physical_locations WHERE id=$1`, *p.LocationID))
			if e != nil {
				return out, false, e
			}
			if p.Mode == "rack" && (loc.Kind != "rack" || p.StartUnit+p.HeightUnits-1 > loc.RackUnits) || p.Mode == "outside_rack" && loc.Kind != "room" {
				return out, false, locations.ErrConflict
			}
		}
	}
	// Release all reviewed slots first, allowing an atomic two-device rack swap.
	if _, err = tx.Exec(ctx, `DELETE FROM rack_unit_occupancy WHERE asset_id=ANY($1::text[])`, ids); err != nil {
		return out, false, err
	}
	for _, p := range in.Placements {
		var at time.Time
		var revision int64
		err = tx.QueryRow(ctx, `INSERT INTO asset_placements(asset_id,mode,location_id,start_unit,height_units,position) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(asset_id) DO UPDATE SET mode=EXCLUDED.mode,location_id=EXCLUDED.location_id,start_unit=EXCLUDED.start_unit,height_units=EXCLUDED.height_units,position=EXCLUDED.position,revision=asset_placements.revision+1,updated_at=now() RETURNING revision,updated_at`, p.AssetID, p.Mode, p.LocationID, p.StartUnit, p.HeightUnits, p.Position).Scan(&revision, &at)
		if err != nil {
			return out, false, locationError(err)
		}
		if p.Mode == "rack" {
			_, err = tx.Exec(ctx, `INSERT INTO rack_unit_occupancy(rack_id,unit,asset_id) SELECT $1,generate_series($2::int,$3::int),$4`, *p.LocationID, p.StartUnit, p.StartUnit+p.HeightUnits-1, p.AssetID)
			if err != nil {
				return out, false, locationError(err)
			}
		}
		after, e := placementTx(ctx, tx, p.AssetID)
		if e != nil {
			return out, false, e
		}
		out = append(out, after)
		b, _ := json.Marshal(before[p.AssetID])
		v, _ := json.Marshal(after)
		if _, err = tx.Exec(ctx, `INSERT INTO placement_events(asset_id,actor_id,before_value,after_value,reason) VALUES($1,$2,$3,$4,$5)`, p.AssetID, actor.User.ID, b, v, logbook.SanitizeText(in.Reason)); err != nil {
			return out, false, err
		}
	}
	if err = saveLocationOperation(ctx, tx, actor, key, hash, "placements_saved", key, out); err != nil {
		return out, false, err
	}
	return out, false, tx.Commit(ctx)
}
func (s *Store) PlacementEvents(ctx context.Context, id string, limit, offset int) ([]locations.Event, error) {
	if _, err := s.Asset(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id,asset_id,actor_id,before_value,after_value,reason,created_at FROM placement_events WHERE asset_id=$1 ORDER BY id DESC LIMIT $2 OFFSET $3`, id, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []locations.Event{}
	for rows.Next() {
		var v locations.Event
		var b, a []byte
		if err = rows.Scan(&v.ID, &v.AssetID, &v.ActorID, &b, &a, &v.Reason, &v.CreatedAt); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &v.Before); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(a, &v.After); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) RackLayout(ctx context.Context, id string) (locations.Rack, error) {
	out := locations.Rack{Placements: []locations.Placement{}}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(context.Background())
	out.Location, err = readLocation(tx.QueryRow(ctx, `SELECT `+locationColumns+` FROM physical_locations WHERE id=$1`, id))
	if err != nil {
		return out, err
	}
	if out.Location.Kind != "rack" {
		return out, locations.ErrInvalid
	}
	rows, err := tx.Query(ctx, `SELECT asset_id FROM asset_placements WHERE location_id=$1 AND mode='rack' ORDER BY start_unit,asset_id LIMIT 100`, id)
	if err != nil {
		return out, err
	}
	ids := []string{}
	for rows.Next() {
		var asset string
		if err = rows.Scan(&asset); err != nil {
			rows.Close()
			return out, err
		}
		ids = append(ids, asset)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	for _, asset := range ids {
		p, e := placementTx(ctx, tx, asset)
		if e != nil {
			return out, e
		}
		out.Placements = append(out.Placements, p)
		out.OccupiedUnits += p.HeightUnits
	}
	out.FreeUnits = out.Location.RackUnits - out.OccupiedUnits
	return out, tx.Commit(ctx)
}
