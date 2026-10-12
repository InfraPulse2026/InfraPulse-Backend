package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
)

type AssetDecisionRepository interface {
	MergeAssets(context.Context, assets.MergeInput, string) (assets.Decision, bool, error)
	SplitAsset(context.Context, assets.SplitInput, string) (assets.Decision, bool, error)
	AssetCandidates(context.Context, string, int) ([]assets.Candidate, error)
	AssetDecisions(context.Context, string, int, int) ([]assets.Decision, error)
	AssetResolutionEvents(context.Context, string, int, int) ([]assets.ResolutionEvent, error)
}

func (a *API) assetDecisionFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, assets.ErrInvalid):
		problem(w, 400, "invalid_asset_decision", "confirmation, current revisions, owned binding IDs, a reason and one Idempotency-Key are required")
	case errors.Is(err, assets.ErrConflict):
		problem(w, 409, "asset_conflict", "reload revisions, review identity and binding ownership, and clear placement of the source asset before merge")
	default:
		a.fail(w, err)
	}
}
func (a *API) assetDecisionRoutes(w http.ResponseWriter, r *http.Request) {
	if a.assetDecisions == nil {
		problem(w, 503, "asset_decisions_unavailable", "asset decisions are not configured")
		return
	}
	if r.Method == "POST" {
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 || commands.ValidateKey(keys[0]) != nil {
			a.assetDecisionFail(w, assets.ErrInvalid)
			return
		}
		var decision assets.Decision
		var replay bool
		var err error
		if strings.HasSuffix(r.URL.Path, "/merge") {
			var in assets.MergeInput
			if !accessJSON(w, r, &in) {
				return
			}
			decision, replay, err = a.assetDecisions.MergeAssets(r.Context(), in, keys[0])
		} else {
			var in assets.SplitInput
			if !accessJSON(w, r, &in) {
				return
			}
			decision, replay, err = a.assetDecisions.SplitAsset(r.Context(), in, keys[0])
		}
		if err != nil {
			a.assetDecisionFail(w, err)
			return
		}
		if replay {
			w.Header().Set("Idempotent-Replay", "true")
		}
		write(w, 200, decision)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/candidates") {
		limit, err := number(r, "limit", 20, 1, 50)
		if err != nil {
			a.assetDecisionFail(w, assets.ErrInvalid)
			return
		}
		items, err := a.assetDecisions.AssetCandidates(r.Context(), r.PathValue("id"), limit)
		if err != nil {
			a.assetDecisionFail(w, err)
			return
		}
		write(w, 200, map[string]any{"items": items, "limit": limit})
		return
	}
	limit, err := number(r, "limit", 50, 1, 200)
	if err != nil {
		a.assetDecisionFail(w, assets.ErrInvalid)
		return
	}
	offset, err := number(r, "offset", 0, 0, 100000)
	if err != nil {
		a.assetDecisionFail(w, assets.ErrInvalid)
		return
	}
	var items any
	if strings.HasSuffix(r.URL.Path, "/resolution-events") {
		items, err = a.assetDecisions.AssetResolutionEvents(r.Context(), r.PathValue("id"), limit, offset)
	} else {
		items, err = a.assetDecisions.AssetDecisions(r.Context(), r.PathValue("id"), limit, offset)
	}
	if err != nil {
		a.assetDecisionFail(w, err)
		return
	}
	write(w, 200, map[string]any{"items": items, "limit": limit, "offset": offset})
}
