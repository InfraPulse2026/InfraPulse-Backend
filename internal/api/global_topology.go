package api

import (
	"context"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/topology"
	"net/http"
	"strconv"
	"strings"
)

type GlobalTopologyRepository interface {
	GlobalTopology(context.Context, topology.GlobalFilter) (topology.GlobalGraph, error)
	GlobalTopologyChanges(context.Context, string, string, string, int64, int) (storage.TopologyChangePage, error)
}

func (a *API) globalTopology(w http.ResponseWriter, r *http.Request) {
	if a.global == nil {
		problem(w, 503, "global_topology_unavailable", "global topology is not configured")
		return
	}
	history := strings.HasSuffix(r.URL.Path, "/changes")
	allowed := map[string]bool{"source_instance_id": true, "asset_id": true, "scope": true, "limit": true}
	if history {
		allowed["after_id"] = true
	} else {
		allowed["include_expired"] = true
		allowed["offset"] = true
		if r.Pattern == "GET /api/topology" {
			allowed["view"] = true
		}
	}
	for k, values := range r.URL.Query() {
		if !allowed[k] || len(values) != 1 {
			problem(w, 400, "invalid_query", "unknown or repeated global topology parameter")
			return
		}
	}
	f := topology.GlobalFilter{SourceID: r.URL.Query().Get("source_instance_id"), AssetID: r.URL.Query().Get("asset_id"), Scope: r.URL.Query().Get("scope")}
	if id := r.PathValue("id"); id != "" {
		if f.AssetID != "" && f.AssetID != id {
			problem(w, 400, "invalid_query", "asset_id must match path")
			return
		}
		f.AssetID = id
	}
	if len(f.SourceID) > 128 || len(f.AssetID) > 128 {
		problem(w, 400, "invalid_query", "IDs must be at most 128 bytes")
		return
	}
	if f.Scope == "" {
		f.Scope = "all"
	}
	if f.Scope != "all" && f.Scope != "real" && f.Scope != "simulated" {
		problem(w, 400, "invalid_query", "scope must be all, real or simulated")
		return
	}
	maxLimit, defaultLimit := 2000, 500
	if history {
		maxLimit, defaultLimit = 200, 50
	}
	var err error
	f.Limit, err = number(r, "limit", defaultLimit, 1, maxLimit)
	if err != nil {
		problem(w, 400, "invalid_query", "limit exceeds global topology bound")
		return
	}
	var result any
	if history {
		var after int64
		if value := r.URL.Query().Get("after_id"); value != "" {
			after, err = strconv.ParseInt(value, 10, 64)
			if err != nil || after < 0 {
				problem(w, 400, "invalid_query", "after_id must be a nonnegative int64")
				return
			}
		}
		result, err = a.global.GlobalTopologyChanges(r.Context(), f.SourceID, f.Scope, f.AssetID, after, f.Limit)
	} else {
		switch r.URL.Query().Get("include_expired") {
		case "", "false":
		case "true":
			f.IncludeExpired = true
		default:
			problem(w, 400, "invalid_query", "include_expired must be true or false")
			return
		}
		f.Offset, err = number(r, "offset", 0, 0, 1000000)
		if err != nil {
			problem(w, 400, "invalid_query", "invalid offset")
			return
		}
		result, err = a.global.GlobalTopology(r.Context(), f)
	}
	if errors.Is(err, topology.ErrGlobalInvalid) {
		problem(w, 400, "invalid_query", "invalid global topology filter")
		return
	}
	if errors.Is(err, topology.ErrGlobalLimit) {
		problem(w, 409, "topology_limit", "one history record exceeds the response bound")
		return
	}
	if errors.Is(err, assets.ErrConflict) {
		problem(w, 409, "asset_inactive", "use an active asset ID; merged aliases expose merged_into_id in the asset card")
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, result)
}
