package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/locations"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
)

type PhysicalRepository interface {
	ResolvedInventory(context.Context, string) (storage.ResolvedInventory, error)
	Locations(context.Context, string, string, int, int) ([]locations.Location, error)
	Location(context.Context, string) (locations.Location, error)
	SaveLocation(context.Context, string, locations.LocationInput, string) (locations.Location, bool, error)
	AssetPlacement(context.Context, string) (locations.Placement, error)
	PlaceAssets(context.Context, locations.BatchInput, string) ([]locations.Placement, bool, error)
	PlacementEvents(context.Context, string, int, int) ([]locations.Event, error)
	RackLayout(context.Context, string) (locations.Rack, error)
}

func (a *API) physicalFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, locations.ErrInvalid):
		problem(w, 400, "invalid_location", "check location hierarchy, placement mode, unit bounds, revision and reason")
	case errors.Is(err, locations.ErrConflict):
		problem(w, 409, "location_conflict", "reload revisions and check rack capacity, occupied units, hierarchy and asset lifecycle")
	case errors.Is(err, assets.ErrConflict):
		problem(w, 409, "inventory_limit", "inventory exceeds the supported source count or response size")
	default:
		a.fail(w, err)
	}
}
func (a *API) physicalRoutes(w http.ResponseWriter, r *http.Request) {
	if a.physical == nil {
		problem(w, 503, "inventory_unavailable", "inventory and locations are not configured")
		return
	}
	allowed := map[string]bool{}
	if r.Method == "GET" && r.Pattern == "GET /api/locations" {
		allowed = map[string]bool{"kind": true, "parent_id": true, "limit": true, "offset": true}
	}
	if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/history") {
		allowed = map[string]bool{"limit": true, "offset": true}
	}
	for k, vs := range r.URL.Query() {
		if !allowed[k] || len(vs) != 1 {
			a.physicalFail(w, locations.ErrInvalid)
			return
		}
	}
	id := r.PathValue("id")
	if r.Method == "GET" {
		var result any
		var err error
		switch r.Pattern {
		case "GET /api/assets/{id}/resolved-inventory":
			result, err = a.physical.ResolvedInventory(r.Context(), id)
		case "GET /api/assets/{id}/placement":
			result, err = a.physical.AssetPlacement(r.Context(), id)
		case "GET /api/locations/{id}":
			result, err = a.physical.Location(r.Context(), id)
		case "GET /api/locations/{id}/rack-layout":
			result, err = a.physical.RackLayout(r.Context(), id)
		default:
			limit, e := number(r, "limit", 50, 1, 200)
			if e != nil {
				a.physicalFail(w, locations.ErrInvalid)
				return
			}
			offset, e := number(r, "offset", 0, 0, 100000)
			if e != nil {
				a.physicalFail(w, locations.ErrInvalid)
				return
			}
			var items any
			if strings.HasSuffix(r.URL.Path, "/history") {
				items, err = a.physical.PlacementEvents(r.Context(), id, limit, offset)
			} else {
				kind := r.URL.Query().Get("kind")
				switch kind {
				case "", "site", "building", "room", "rack":
				default:
					a.physicalFail(w, locations.ErrInvalid)
					return
				}
				items, err = a.physical.Locations(r.Context(), kind, r.URL.Query().Get("parent_id"), limit, offset)
			}
			result = map[string]any{"items": items, "limit": limit, "offset": offset}
		}
		if err != nil {
			a.physicalFail(w, err)
			return
		}
		write(w, 200, result)
		return
	}
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 || commands.ValidateKey(keys[0]) != nil {
		a.physicalFail(w, locations.ErrInvalid)
		return
	}
	var result any
	var replay bool
	var err error
	if strings.HasPrefix(r.URL.Path, "/api/locations") {
		var in locations.LocationInput
		if !accessJSON(w, r, &in) {
			return
		}
		if !locations.ValidLocation(in) || id == "" && in.ExpectedRevision != 0 || id != "" && in.ExpectedRevision < 1 {
			a.physicalFail(w, locations.ErrInvalid)
			return
		}
		result, replay, err = a.physical.SaveLocation(r.Context(), id, in, keys[0])
	} else {
		var in locations.BatchInput
		if r.Pattern == "PUT /api/assets/{id}/placement" {
			var single struct {
				locations.PlacementInput
				Reason string `json:"reason"`
			}
			if !accessJSON(w, r, &single) {
				return
			}
			if single.AssetID != "" && single.AssetID != id {
				a.physicalFail(w, locations.ErrInvalid)
				return
			}
			single.AssetID = id
			in = locations.BatchInput{Placements: []locations.PlacementInput{single.PlacementInput}, Reason: single.Reason}
		} else {
			if !boundedJSON(w, r, &in, 65536) {
				return
			}
		}
		if !locations.ValidBatch(in) {
			a.physicalFail(w, locations.ErrInvalid)
			return
		}
		var placements []locations.Placement
		placements, replay, err = a.physical.PlaceAssets(r.Context(), in, keys[0])
		if r.Pattern == "PUT /api/assets/{id}/placement" && err == nil {
			if len(placements) != 1 {
				problem(w, 503, "storage_unavailable", "invalid placement response")
				return
			}
			result = placements[0]
		} else {
			result = map[string]any{"items": placements}
		}
	}
	if err != nil {
		a.physicalFail(w, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
	}
	write(w, 200, result)
}
