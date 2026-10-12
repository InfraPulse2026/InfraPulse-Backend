package storage

import (
	"context"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/locations"
	"sync"
	"testing"
	"time"
)

func physicalFixture(t *testing.T, s *Store, ctx context.Context) (locations.Location, locations.Location) {
	t.Helper()
	var parent *string
	var room, rack locations.Location
	for i, kind := range []string{"site", "building", "room", "rack"} {
		in := locations.LocationInput{Kind: kind, Name: kind, ParentID: parent, Reason: "verified labels"}
		if kind == "rack" {
			in.RackUnits = 42
		}
		v, replay, err := s.SaveLocation(ctx, "", in, "fixture-location-"+kind)
		if err != nil || replay {
			t.Fatal("location", i, err)
		}
		again, replay, err := s.SaveLocation(ctx, "", in, "fixture-location-"+kind)
		if err != nil || !replay || v.ID != again.ID {
			t.Fatal("catalog idempotency", err)
		}
		parent = &v.ID
		if kind == "room" {
			room = v
		}
		if kind == "rack" {
			rack = v
		}
	}
	return room, rack
}
func placeRequest(id, rack string, start, height int, revision int64) locations.PlacementInput {
	return locations.PlacementInput{AssetID: id, Mode: "rack", LocationID: &rack, StartUnit: start, HeightUnits: height, ExpectedRevision: revision}
}

func TestPhysicalPlacementAtomicSwapHistoryBoundsAndMerge(t *testing.T) {
	s, ctx, items, bindings, d := assetDecisionFixture(t)
	room, rack := physicalFixture(t, s, ctx)
	in := locations.BatchInput{Placements: []locations.PlacementInput{placeRequest(items[0].ID, rack.ID, 1, 2, 0), placeRequest(items[1].ID, rack.ID, 4, 1, 0)}, Reason: "installed hardware"}
	out, replay, err := s.PlaceAssets(ctx, in, "initial-placement")
	if err != nil || replay || len(out) != 2 {
		t.Fatal("place", out, err)
	}
	if _, replay, err = s.PlaceAssets(ctx, in, "initial-placement"); err != nil || !replay {
		t.Fatal("placement idempotency", err)
	}
	layout, err := s.RackLayout(ctx, rack.ID)
	if err != nil || layout.OccupiedUnits != 3 || layout.FreeUnits != 39 {
		t.Fatal("rack occupancy", layout, err)
	}
	p, err := s.AssetPlacement(ctx, items[0].ID)
	if err != nil || len(p.Path) != 4 || p.Path[0].Kind != "site" || p.Path[3].Kind != "rack" {
		t.Fatal("location path", p, err)
	}
	swap := locations.BatchInput{Placements: []locations.PlacementInput{placeRequest(items[0].ID, rack.ID, 4, 2, 1), placeRequest(items[1].ID, rack.ID, 1, 1, 1)}, Reason: "move both atomically"}
	if _, _, err = s.PlaceAssets(ctx, swap, "swap"); err != nil {
		t.Fatal("atomic swap", err)
	}
	bad := locations.BatchInput{Placements: []locations.PlacementInput{placeRequest(items[0].ID, rack.ID, 2, 2, 2), placeRequest(items[1].ID, rack.ID, 3, 2, 2)}, Reason: "overlap should roll back"}
	if _, _, err = s.PlaceAssets(ctx, bad, "overlap"); !errors.Is(err, locations.ErrConflict) {
		t.Fatal("overlap accepted", err)
	}
	p, _ = s.AssetPlacement(ctx, items[0].ID)
	if p.StartUnit != 4 || p.Revision != 2 {
		t.Fatal("partial batch committed", p)
	}
	events, err := s.PlacementEvents(ctx, items[0].ID, 50, 0)
	if err != nil || len(events) != 2 || events[0].Before.StartUnit != 1 || events[0].After.StartUnit != 4 {
		t.Fatal("history/replay/rollback", events, err)
	}
	resize := locations.LocationInput{Kind: "rack", ParentID: rack.ParentID, Name: rack.Name, RackUnits: 4, ExpectedRevision: rack.Revision, Reason: "resize test"}
	if _, _, err = s.SaveLocation(ctx, rack.ID, resize, "resize"); !errors.Is(err, locations.ErrConflict) {
		t.Fatal("occupied top units discarded", err)
	}
	if _, _, err = s.PlaceAssets(ctx, locations.BatchInput{Placements: []locations.PlacementInput{placeRequest(items[0].ID, rack.ID, 42, 2, 2)}, Reason: "exceeds rack capacity"}, "bounds"); !errors.Is(err, locations.ErrConflict) {
		t.Fatal("out of bounds", err)
	}
	if _, _, err = s.PlaceAssets(ctx, swap, "stale"); !errors.Is(err, locations.ErrConflict) {
		t.Fatal("stale revision accepted", err)
	}
	// Source inventory refreshes never overwrite reviewed physical placement.
	if err = s.Ingest(ctx, assetInventory(*bindings[0].LegacyAgentID, "session-1", "after-placement", time.Now(), d)); err != nil {
		t.Fatal(err)
	}
	inv, err := s.ResolvedInventory(ctx, items[0].ID)
	if err != nil || inv.Placement.StartUnit != 4 || inv.Attributes["serial"].Selected == nil {
		t.Fatal("inventory card", inv, err)
	}
	for i := range items {
		items[i], err = s.Asset(ctx, items[i].ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err = s.MergeAssets(ctx, assetMergeInput(items, bindings), "placed-merge"); !errors.Is(err, assets.ErrConflict) {
		t.Fatal("placed alias silently merged", err)
	}
	clear := locations.BatchInput{Placements: []locations.PlacementInput{{AssetID: items[1].ID, Mode: "unplaced", ExpectedRevision: 2}}, Reason: "remove duplicate placement before merge"}
	if _, _, err = s.PlaceAssets(ctx, clear, "clear-source"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.MergeAssets(ctx, assetMergeInput(items, bindings), "reviewed-merge"); err != nil {
		t.Fatal("merge after clear", err)
	}
	p, _ = s.AssetPlacement(ctx, items[0].ID)
	if p.StartUnit != 4 {
		t.Fatal("target placement lost", p)
	}
	merged, _ := s.Asset(ctx, items[0].ID)
	split := assets.SplitInput{AssetID: merged.ID, Revision: merged.Revision, BindingIDs: []string{bindings[1].ID}, CanonicalBindingID: bindings[1].ID, Confirmed: true, Reason: "verify distinct hardware"}
	decision, _, err := s.SplitAsset(ctx, split, "split-placed")
	if err != nil {
		t.Fatal(err)
	}
	p, _ = s.AssetPlacement(ctx, decision.TargetID)
	if p.Mode != "unplaced" || p.Revision != 0 {
		t.Fatal("split copied occupied units", p)
	}
	outside := locations.BatchInput{Placements: []locations.PlacementInput{{AssetID: decision.TargetID, Mode: "outside_rack", LocationID: &room.ID, Position: "table 2"}}, Reason: "outside rack"}
	if _, _, err = s.PlaceAssets(ctx, outside, "outside"); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("repeat migration", err)
	}
	layout, err = s.RackLayout(ctx, rack.ID)
	if err != nil || layout.OccupiedUnits != 2 {
		t.Fatal("upgrade lost rack", layout, err)
	}
}
func TestPhysicalPlacementConcurrentOverlapAndLiveAuthorization(t *testing.T) {
	s, ctx, items, _, _ := assetDecisionFixture(t)
	_, rack := physicalFixture(t, s, ctx)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, a := range items {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, _, err := s.PlaceAssets(ctx, locations.BatchInput{Placements: []locations.PlacementInput{placeRequest(id, rack.ID, 10, 2, 0)}, Reason: "concurrent install"}, "concurrent-"+id)
			results <- err
		}(a.ID)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, locations.ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("overlap race", success, conflict)
	}
	var units int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM rack_unit_occupancy`).Scan(&units); err != nil || units != 2 {
		t.Fatal("slot allocation", units, err)
	}
	actor, _ := access.Current(ctx)
	if _, err := s.pool.Exec(ctx, `UPDATE access_users SET role='viewer' WHERE id=$1`, actor.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PlaceAssets(ctx, locations.BatchInput{Placements: []locations.PlacementInput{{AssetID: items[0].ID, Mode: "unplaced"}}, Reason: "revoked role"}, "revoked"); !errors.Is(err, access.ErrForbidden) {
		t.Fatal("stale admin principal wrote", err)
	}
}
func TestPhysicalHierarchyDatabaseGuardsAndUpgrade(t *testing.T) {
	s := integrationStore(t)
	_, admin := adminFixture(t, s)
	ctx := access.WithPrincipal(t.Context(), admin)
	room, rack := physicalFixture(t, s, ctx)
	if _, err := s.pool.Exec(ctx, `INSERT INTO physical_locations(id,kind,parent_id,name,rack_units) VALUES('bad','rack',$1,'bad',42)`, *room.ParentID); err == nil {
		t.Fatal("SQL accepted rack under building")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE physical_locations SET parent_id=$2 WHERE id=$1`, rack.ID, *room.ParentID); err == nil {
		t.Fatal("SQL reparented hierarchy")
	}
	// Recreate a populated B5 schema only inside this test's disposable schema.
	if _, err := s.pool.Exec(ctx, `DROP TABLE physical_location_events,placement_events,rack_unit_occupancy,asset_placements,location_operations,physical_locations; DROP FUNCTION validate_rack_unit(),validate_asset_placement(),validate_physical_location(); DELETE FROM schema_migrations WHERE version=26`); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, registration("upgrade-agent", time.Now())); err != nil {
		t.Fatal(err)
	}
	before, err := s.Assets(ctx, "", "all", 50, 0)
	if err != nil || len(before) == 0 {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("B5 upgrade", err)
	}
	after, err := s.Assets(ctx, "", "all", 50, 0)
	if err != nil || len(after) != len(before) {
		t.Fatal("upgrade changed existing assets", err)
	}
	p, err := s.AssetPlacement(ctx, after[0].ID)
	if err != nil || p.Mode != "unplaced" {
		t.Fatal("upgrade invented location", p, err)
	}
}
