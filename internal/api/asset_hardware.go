package api

import (
	"context"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assethardware"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assets"
	"net/http"
	"net/url"
	"strconv"
)

type AssetHardwareRepository interface {
	AssetHardware(context.Context, string, assethardware.Filter) (assethardware.Page, error)
	AssetHardwareHistory(context.Context, string, assethardware.HistoryFilter) (assethardware.HistoryPage, error)
}

func strictB8Query(w http.ResponseWriter, r *http.Request, allowed map[string]bool) bool {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		problem(w, 400, "invalid_query", "malformed query encoding")
		return false
	}
	for key, values := range query {
		if !allowed[key] || len(values) != 1 {
			problem(w, 400, "invalid_query", "unknown or repeated query parameter")
			return false
		}
	}
	return true
}
func b8Bool(r *http.Request, key string, fallback bool) (bool, error) {
	switch r.URL.Query().Get(key) {
	case "":
		return fallback, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, errors.New("invalid boolean")
}
func b8Cursor(r *http.Request, key string) (int64, error) {
	value := r.URL.Query().Get(key)
	if value == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("invalid cursor")
	}
	return n, nil
}
func (a *API) assetHardwareRoutes(w http.ResponseWriter, r *http.Request) {
	if a.hardware == nil {
		problem(w, 503, "hardware_unavailable", "asset hardware is not configured")
		return
	}
	history := r.Pattern == "GET /api/assets/{id}/hardware/history"
	allowed := map[string]bool{"source_instance_id": true, "include_related": true, "limit": true, "max_age_seconds": true}
	if history {
		allowed["after_id"] = true
		allowed["observation_key"] = true
	} else {
		allowed["kind"] = true
		allowed["offset"] = true
	}
	if !strictB8Query(w, r, allowed) {
		return
	}
	includeRelated, err := b8Bool(r, "include_related", true)
	if err != nil {
		problem(w, 400, "invalid_query", "include_related must be true or false")
		return
	}
	maxAge, err := number(r, "max_age_seconds", 300, 3, 86400)
	if err != nil {
		problem(w, 400, "invalid_query", "max_age_seconds must be 3..86400")
		return
	}
	limit, err := number(r, "limit", 50, 1, 200)
	if err != nil {
		problem(w, 400, "invalid_query", "limit must be 1..200")
		return
	}
	source := r.URL.Query().Get("source_instance_id")
	if len(source) > 128 {
		problem(w, 400, "invalid_query", "source_instance_id exceeds 128 bytes")
		return
	}
	if !r.URL.Query().Has("max_age_seconds") {
		maxAge = 0
	}
	var out any
	if history {
		after, e := b8Cursor(r, "after_id")
		if e != nil {
			problem(w, 400, "invalid_query", "after_id must be nonnegative int64")
			return
		}
		out, err = a.hardware.AssetHardwareHistory(r.Context(), r.PathValue("id"), assethardware.HistoryFilter{SourceID: source, ObservationKey: r.URL.Query().Get("observation_key"), IncludeRelated: includeRelated, AfterID: after, Limit: limit, MaxAgeSeconds: maxAge})
	} else {
		offset, e := number(r, "offset", 0, 0, 100000)
		if e != nil {
			problem(w, 400, "invalid_query", "invalid offset")
			return
		}
		out, err = a.hardware.AssetHardware(r.Context(), r.PathValue("id"), assethardware.Filter{SourceID: source, Kind: r.URL.Query().Get("kind"), IncludeRelated: includeRelated, Limit: limit, Offset: offset, MaxAgeSeconds: maxAge})
	}
	if errors.Is(err, assethardware.ErrInvalid) {
		problem(w, 400, "invalid_query", "invalid hardware kind, identity or query bounds")
		return
	}
	if errors.Is(err, assethardware.ErrLimit) {
		problem(w, 409, "hardware_limit", "hardware context exceeds bounded source/response size")
		return
	}
	if errors.Is(err, assets.ErrConflict) {
		problem(w, 409, "asset_inactive", "use the active asset ID from merged_into_id")
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, out)
}
