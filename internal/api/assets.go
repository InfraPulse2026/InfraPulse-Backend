package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
)

type AssetRepository interface {
	Assets(context.Context, string, string, int, int) ([]assets.Asset, error)
	Asset(context.Context, string) (assets.Asset, error)
	AssetBindings(context.Context, string, bool, int, int) ([]assets.Binding, error)
	AssetFacts(context.Context, string, int, int) ([]assets.FactVersion, error)
	AssetIdentities(context.Context, string, int, int) ([]assets.Identity, error)
}

func WithAssets(repo AssetRepository) Option {
	return func(a *API) {
		a.assets = repo
		a.hardware, _ = repo.(AssetHardwareRepository)
		a.assetLogs, _ = repo.(AssetLogRepository)
		a.voip, _ = repo.(AssetVoIPRepository)
		a.physical, _ = repo.(PhysicalRepository)
		a.global, _ = repo.(GlobalTopologyRepository)
		a.assetDecisions, _ = repo.(AssetDecisionRepository)
		a.external, _ = repo.(ExternalMonitoringRepository)
		a.prometheus, _ = repo.(PrometheusRepository)
	}
}

func (a *API) assetRoutes(w http.ResponseWriter, r *http.Request) {
	if a.assets == nil {
		problem(w, 503, "assets_unavailable", "asset registry is not configured")
		return
	}
	limit, err := number(r, "limit", 50, 1, 200)
	if err != nil {
		problem(w, 400, "invalid_query", err.Error())
		return
	}
	offset, err := number(r, "offset", 0, 0, 100000)
	if err != nil {
		problem(w, 400, "invalid_query", err.Error())
		return
	}
	id := r.PathValue("id")
	if id == "" {
		kind := r.URL.Query().Get("kind")
		switch kind {
		case "", "host", "network_device", "system", "bmc", "chassis", "component", "unknown":
		default:
			problem(w, 400, "invalid_query", "unknown asset kind")
			return
		}
		scope := r.URL.Query().Get("scope")
		if scope == "" {
			scope = "all"
		}
		if scope != "all" && scope != "real" && scope != "simulated" {
			problem(w, 400, "invalid_query", "scope must be all, real or simulated")
			return
		}
		items, e := a.assets.Assets(r.Context(), kind, scope, limit, offset)
		if e != nil {
			a.fail(w, e)
			return
		}
		write(w, 200, map[string]any{"items": items, "limit": limit, "offset": offset})
		return
	}
	var items any
	switch {
	case strings.HasSuffix(r.URL.Path, "/sources"):
		items, err = a.assets.AssetBindings(r.Context(), id, false, limit, offset)
	case strings.HasSuffix(r.URL.Path, "/inventory"):
		items, err = a.assets.AssetBindings(r.Context(), id, true, limit, offset)
	case strings.HasSuffix(r.URL.Path, "/facts"):
		items, err = a.assets.AssetFacts(r.Context(), id, limit, offset)
	case strings.HasSuffix(r.URL.Path, "/identities"):
		items, err = a.assets.AssetIdentities(r.Context(), id, limit, offset)
	default:
		v, e := a.assets.Asset(r.Context(), id)
		if e != nil {
			a.fail(w, e)
			return
		}
		write(w, 200, v)
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, map[string]any{"items": items, "limit": limit, "offset": offset})
}
