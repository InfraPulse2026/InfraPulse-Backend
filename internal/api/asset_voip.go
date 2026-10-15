package api

import (
	"context"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assetvoip"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"net/http"
	"time"
)

type AssetVoIPRepository interface {
	AssetVoIP(context.Context, string, int, int) (assetvoip.Card, error)
	SaveAssetVoIP(context.Context, string, string, assetvoip.Input, string) (assetvoip.Association, bool, error)
	AssetVoIPHistory(context.Context, string, string, assetvoip.HistoryFilter) (assetvoip.HistoryPage, error)
	AssetVoIPEvents(context.Context, string, string, int, int) ([]assetvoip.Event, error)
}

func (a *API) assetVoIPRoutes(w http.ResponseWriter, r *http.Request) {
	if a.voip == nil {
		problem(w, 503, "asset_voip_unavailable", "asset VoIP projection is not configured")
		return
	}
	asset, id := r.PathValue("id"), r.PathValue("association_id")
	var out any
	var err error
	if r.Method != "GET" {
		if !strictB8Query(w, r, map[string]bool{}) {
			return
		}
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 || commands.ValidateKey(keys[0]) != nil {
			problem(w, 400, "invalid_voip", "one valid Idempotency-Key is required")
			return
		}
		var in assetvoip.Input
		if !accessJSON(w, r, &in) {
			return
		}
		if !assetvoip.ValidInput(in) || id == "" && in.ExpectedRevision != 0 || id != "" && in.ExpectedRevision < 1 {
			problem(w, 400, "invalid_voip", "confirmed source-owned mapping, reason and current revisions are required")
			return
		}
		var replay bool
		out, replay, err = a.voip.SaveAssetVoIP(r.Context(), asset, id, in, keys[0])
		if replay {
			w.Header().Set("Idempotent-Replay", "true")
		}
	} else {
		allowed := map[string]bool{"limit": true, "offset": true}
		history := r.Pattern == "GET /api/assets/{id}/voip/{association_id}/history"
		if history {
			allowed = map[string]bool{"limit": true, "before_id": true, "from": true, "to": true}
		}
		if !strictB8Query(w, r, allowed) {
			return
		}
		maxLimit := 100
		if history {
			maxLimit = 200
		}
		limit, e := number(r, "limit", 50, 1, maxLimit)
		if e != nil {
			problem(w, 400, "invalid_query", "invalid limit")
			return
		}
		if history {
			before, e := b8Cursor(r, "before_id")
			if e != nil {
				problem(w, 400, "invalid_query", "before_id must be nonnegative int64")
				return
			}
			f := assetvoip.HistoryFilter{BeforeID: before, Limit: limit}
			for key, p := range map[string]**time.Time{"from": &f.From, "to": &f.To} {
				if value := r.URL.Query().Get(key); value != "" {
					v, e := time.Parse(time.RFC3339Nano, value)
					if e != nil {
						problem(w, 400, "invalid_query", "time bounds require RFC3339 timezone")
						return
					}
					v = v.UTC()
					*p = &v
				}
			}
			if !assetvoip.ValidHistory(f) {
				problem(w, 400, "invalid_query", "invalid VoIP history range")
				return
			}
			out, err = a.voip.AssetVoIPHistory(r.Context(), asset, id, f)
		} else {
			offset, e := number(r, "offset", 0, 0, 100000)
			if e != nil {
				problem(w, 400, "invalid_query", "invalid offset")
				return
			}
			if id == "" {
				out, err = a.voip.AssetVoIP(r.Context(), asset, limit, offset)
			} else {
				var items []assetvoip.Event
				items, err = a.voip.AssetVoIPEvents(r.Context(), asset, id, limit, offset)
				out = map[string]any{"items": items, "limit": limit, "offset": offset}
			}
		}
	}
	if errors.Is(err, assetvoip.ErrInvalid) {
		problem(w, 400, "invalid_voip", "invalid VoIP source identity or bounds")
		return
	}
	if errors.Is(err, assetvoip.ErrConflict) {
		problem(w, 409, "voip_conflict", "reload revisions and review binding ownership, scope and endpoint identity")
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, out)
}
