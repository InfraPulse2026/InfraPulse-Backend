package locations

import (
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var ErrInvalid = errors.New("invalid physical location input")
var ErrConflict = errors.New("location revision, hierarchy or rack occupancy conflict")

type Location struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	ParentID  *string   `json:"parent_id"`
	Name      string    `json:"name"`
	RackUnits int       `json:"rack_units"`
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
type LocationInput struct {
	Kind             string  `json:"kind"`
	ParentID         *string `json:"parent_id"`
	Name             string  `json:"name"`
	RackUnits        int     `json:"rack_units"`
	ExpectedRevision int64   `json:"expected_revision"`
	Reason           string  `json:"reason"`
}
type Placement struct {
	AssetID     string     `json:"asset_id"`
	Mode        string     `json:"mode"`
	LocationID  *string    `json:"location_id"`
	StartUnit   int        `json:"start_unit"`
	HeightUnits int        `json:"height_units"`
	Position    string     `json:"position"`
	Revision    int64      `json:"revision"`
	UpdatedAt   *time.Time `json:"updated_at"`
	Path        []Location `json:"path"`
}
type PlacementInput struct {
	AssetID          string  `json:"asset_id"`
	Mode             string  `json:"mode"`
	LocationID       *string `json:"location_id"`
	StartUnit        int     `json:"start_unit"`
	HeightUnits      int     `json:"height_units"`
	Position         string  `json:"position"`
	ExpectedRevision int64   `json:"expected_revision"`
}
type BatchInput struct {
	Placements []PlacementInput `json:"placements"`
	Reason     string           `json:"reason"`
}
type Event struct {
	ID        int64     `json:"id"`
	AssetID   string    `json:"asset_id"`
	ActorID   string    `json:"actor_id"`
	Before    Placement `json:"before"`
	After     Placement `json:"after"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}
type Rack struct {
	Location      Location    `json:"location"`
	Placements    []Placement `json:"placements"`
	OccupiedUnits int         `json:"occupied_units"`
	FreeUnits     int         `json:"free_units"`
}

func text(v string, min, max int) bool {
	if !utf8.ValidString(v) || len(v) < min || len(v) > max || v != strings.TrimSpace(v) {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func ValidLocation(in LocationInput) bool {
	if !text(in.Name, 1, 128) || !text(in.Reason, 3, 1000) || in.ExpectedRevision < 0 {
		return false
	}
	switch in.Kind {
	case "site":
		return in.ParentID == nil && in.RackUnits == 0
	case "building", "room":
		return in.ParentID != nil && text(*in.ParentID, 1, 128) && in.RackUnits == 0
	case "rack":
		return in.ParentID != nil && text(*in.ParentID, 1, 128) && in.RackUnits >= 1 && in.RackUnits <= 100
	}
	return false
}
func ValidBatch(in BatchInput) bool {
	if len(in.Placements) < 1 || len(in.Placements) > 50 || !text(in.Reason, 3, 1000) {
		return false
	}
	seen := map[string]bool{}
	for _, p := range in.Placements {
		if !text(p.AssetID, 1, 128) || seen[p.AssetID] || p.ExpectedRevision < 0 || !text(p.Position, 0, 256) {
			return false
		}
		seen[p.AssetID] = true
		switch p.Mode {
		case "unplaced":
			if p.LocationID != nil || p.StartUnit != 0 || p.HeightUnits != 0 || p.Position != "" {
				return false
			}
		case "outside_rack":
			if p.LocationID == nil || !text(*p.LocationID, 1, 128) || p.StartUnit != 0 || p.HeightUnits != 0 {
				return false
			}
		case "rack":
			if p.LocationID == nil || !text(*p.LocationID, 1, 128) || p.StartUnit < 1 || p.StartUnit > 100 || p.HeightUnits < 1 || p.HeightUnits > 100 || p.StartUnit+p.HeightUnits-1 > 100 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
