package api

import (
	"context"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/inventory"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/locations"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type physicalStub struct {
	assetStub
	reads, writes int
	batch         locations.BatchInput
	fail          error
}

func (s *physicalStub) ResolvedInventory(context.Context, string) (storage.ResolvedInventory, error) {
	s.reads++
	return storage.ResolvedInventory{Summary: inventory.Summary{AssetID: "asset"}}, s.fail
}
func (s *physicalStub) Locations(context.Context, string, string, int, int) ([]locations.Location, error) {
	s.reads++
	return []locations.Location{}, s.fail
}
func (s *physicalStub) Location(context.Context, string) (locations.Location, error) {
	s.reads++
	return locations.Location{ID: "loc"}, s.fail
}
func (s *physicalStub) SaveLocation(context.Context, string, locations.LocationInput, string) (locations.Location, bool, error) {
	s.writes++
	return locations.Location{ID: "loc"}, true, s.fail
}
func (s *physicalStub) AssetPlacement(context.Context, string) (locations.Placement, error) {
	s.reads++
	return locations.Placement{AssetID: "asset", Mode: "unplaced"}, s.fail
}
func (s *physicalStub) PlaceAssets(_ context.Context, in locations.BatchInput, _ string) ([]locations.Placement, bool, error) {
	s.writes++
	s.batch = in
	return []locations.Placement{{AssetID: in.Placements[0].AssetID, Mode: "rack"}}, true, s.fail
}
func (s *physicalStub) PlacementEvents(context.Context, string, int, int) ([]locations.Event, error) {
	s.reads++
	return []locations.Event{}, s.fail
}
func (s *physicalStub) RackLayout(context.Context, string) (locations.Rack, error) {
	s.reads++
	return locations.Rack{Placements: []locations.Placement{}}, s.fail
}

func TestPhysicalAPIPermissionsValidationAndIdempotency(t *testing.T) {
	for _, role := range []string{"viewer", "operator", "admin"} {
		t.Run(role, func(t *testing.T) {
			token, _ := access.NewToken()
			hash, _ := access.TokenHash(token)
			auth := accessStub{principal: access.Principal{User: access.User{ID: "admin", Role: role}, SessionHash: hash, ExpiresAt: time.Now().Add(time.Hour)}}
			repo := &physicalStub{}
			h := New(fakeRepository{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithAccess(access.New(auth, access.Config{Mode: "required", SessionTTL: time.Hour})), WithAssets(repo))
			for _, path := range []string{"/api/assets/asset/resolved-inventory", "/api/assets/asset/placement", "/api/assets/asset/placement/history", "/api/locations", "/api/locations/loc", "/api/locations/loc/rack-layout"} {
				w := httptest.NewRecorder()
				r := httptest.NewRequest("GET", path, nil)
				h.ServeHTTP(w, r)
				if w.Code != 401 {
					t.Fatal("read without auth", path, w.Code)
				}
				w = httptest.NewRecorder()
				r.Header.Set("Authorization", "Bearer "+token)
				h.ServeHTTP(w, r)
				if w.Code != 200 {
					t.Fatal("authorized read", path, w.Code)
				}
			}
			for _, request := range []struct{ method, path, body string }{{"POST", "/api/locations", `{"kind":"site","name":"Lab","reason":"reviewed labels"}`}, {"PUT", "/api/locations/loc", `{"kind":"site","name":"Lab","expected_revision":1,"reason":"reviewed labels"}`}, {"PUT", "/api/assets/asset/placement", `{"mode":"rack","location_id":"rack","start_unit":1,"height_units":2,"expected_revision":0,"reason":"reviewed labels"}`}, {"POST", "/api/placements/import", `{"placements":[{"asset_id":"asset","mode":"rack","location_id":"rack","start_unit":1,"height_units":2}],"reason":"reviewed import"}`}} {
				r := httptest.NewRequest(request.method, request.path, strings.NewReader(request.body))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer "+token)
				r.Header.Set("Idempotency-Key", "one-operation")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				expected := 403
				if role == "admin" {
					expected = 200
				}
				if w.Code != expected {
					t.Fatal("write permissions", request.path, w.Code, w.Body.String())
				}
				if expected == 200 && w.Header().Get("Idempotent-Replay") != "true" {
					t.Fatal("replay not indicated")
				}
			}
			if role != "admin" && repo.writes != 0 {
				t.Fatal("forbidden write reached storage")
			}
			if role == "admin" {
				before := repo.writes
				for _, body := range []string{`{"mode":"rack","location_id":"rack","start_unit":1,"height_units":2,"asset_id":"other","reason":"wrong ownership"}`, `{"mode":"rack","location_id":"rack","start_unit":1,"height_units":101,"reason":"bounds"}`, `{"mode":"unplaced","reason":"valid reason","unexpected":true}`} {
					r := httptest.NewRequest("PUT", "/api/assets/asset/placement", strings.NewReader(body))
					r.Header.Set("Authorization", "Bearer "+token)
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("Idempotency-Key", "bad-body")
					w := httptest.NewRecorder()
					h.ServeHTTP(w, r)
					if w.Code != 400 || repo.writes != before {
						t.Fatal("invalid body reached storage", w.Code)
					}
				}
				r := httptest.NewRequest("PUT", "/api/assets/asset/placement", strings.NewReader(`{"mode":"unplaced","reason":"reviewed"}`))
				r.Header.Set("Authorization", "Bearer "+token)
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 400 || repo.writes != before {
					t.Fatal("missing idempotency key")
				}
			}
			before := repo.reads
			for _, path := range []string{"/api/locations?kind=datacenter", "/api/locations?limit=201", "/api/locations?kind=rack&kind=room", "/api/assets/asset/resolved-inventory?source_id=someone-else"} {
				r := httptest.NewRequest("GET", path, nil)
				r.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 400 || repo.reads != before {
					t.Fatal("invalid query reached storage", path, w.Code)
				}
			}
		})
	}
}
