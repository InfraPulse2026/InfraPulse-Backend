package locations

import (
	"math"
	"testing"
)

func TestPlacementRejectsOverflowDuplicatesAndInconsistentModes(t *testing.T) {
	id := "rack"
	good := PlacementInput{AssetID: "asset", Mode: "rack", LocationID: &id, StartUnit: 41, HeightUnits: 2}
	if !ValidBatch(BatchInput{Placements: []PlacementInput{good}, Reason: "reviewed"}) {
		t.Fatal("2U rejected")
	}
	for _, p := range []PlacementInput{{AssetID: "asset", Mode: "rack", LocationID: &id, StartUnit: math.MaxInt, HeightUnits: 2}, {AssetID: "asset", Mode: "rack", LocationID: &id, StartUnit: 2, HeightUnits: math.MaxInt}, {AssetID: "asset", Mode: "unplaced", StartUnit: 1}, {AssetID: "asset", Mode: "outside_rack", LocationID: &id, HeightUnits: 1}, {AssetID: "asset", Mode: "rack", LocationID: &id, StartUnit: 100, HeightUnits: 2}} {
		if ValidBatch(BatchInput{Placements: []PlacementInput{p}, Reason: "reviewed"}) {
			t.Fatal("unsafe placement accepted", p)
		}
	}
	if ValidBatch(BatchInput{Placements: []PlacementInput{good, good}, Reason: "reviewed"}) {
		t.Fatal("duplicate asset accepted")
	}
}
