package api

import (
	"context"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assetlogs"
	"net/http"
	"strconv"
)

type AssetLogRepository interface {
	LogRaw(context.Context, int64, string) (assetlogs.Raw, error)
	AssetLogs(context.Context, string, assetlogs.Filter) (assetlogs.Page, error)
	AssetLog(context.Context, string, int64) (assetlogs.Record, error)
	AssetLogRaw(context.Context, string, int64) (assetlogs.Raw, error)
	AssetAuditEvents(context.Context, string, assetlogs.Filter) (assetlogs.AuditPage, error)
}

func (a *API) legacyLogRaw(w http.ResponseWriter, r *http.Request) {
	if a.assetLogs == nil {
		problem(w, 503, "asset_logs_unavailable", "controlled raw reader is not configured")
		return
	}
	if !strictB8Query(w, r, map[string]bool{"agent_id": true}) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		problem(w, 400, "invalid_query", "log id must be positive int64")
		return
	}
	agent := r.URL.Query().Get("agent_id")
	if len(agent) > 256 {
		problem(w, 400, "invalid_query", "agent_id exceeds 256 bytes")
		return
	}
	out, err := a.assetLogs.LogRaw(r.Context(), id, agent)
	if errors.Is(err, assetlogs.ErrFilter) {
		problem(w, 400, "invalid_query", "invalid raw log identity or bounded payload")
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, out)
}

func (a *API) assetLogRoutes(w http.ResponseWriter, r *http.Request) {
	if a.assetLogs == nil {
		problem(w, 503, "asset_logs_unavailable", "asset log projection is not configured")
		return
	}
	id := r.PathValue("id")
	var out any
	var err error
	if value := r.PathValue("log_id"); value != "" {
		if !strictB8Query(w, r, map[string]bool{}) {
			return
		}
		logID, e := strconv.ParseInt(value, 10, 64)
		if e != nil || logID < 1 {
			problem(w, 400, "invalid_query", "log_id must be positive int64")
			return
		}
		if r.Pattern == "GET /api/assets/{id}/logs/{log_id}/raw" {
			out, err = a.assetLogs.AssetLogRaw(r.Context(), id, logID)
		} else {
			out, err = a.assetLogs.AssetLog(r.Context(), id, logID)
		}
	} else {
		allowed := map[string]bool{"source_instance_id": true, "record_scope": true, "source": true, "source_id": true, "host": true, "process": true, "record_type": true, "event_id": true, "event_key": true, "executable": true, "q": true, "contains": true, "limit": true, "before_id": true, "severity": true, "facility": true, "uid": true, "auid": true, "pid": true, "success": true, "from": true, "to": true}
		if !strictB8Query(w, r, allowed) {
			return
		}
		filter, e := logFilter(r)
		if e != nil {
			problem(w, 400, "invalid_query", e.Error())
			return
		}
		if filter.Limit > 200 {
			problem(w, 400, "invalid_query", "limit must be 1..200")
			return
		}
		filter.EventKey = r.URL.Query().Get("event_key")
		if filter.EventKey != "" && !auditKeyPattern.MatchString(filter.EventKey) {
			problem(w, 400, "invalid_query", "event_key must be a valid audit group key")
			return
		}
		f := assetlogs.Filter{Log: filter, SourceInstanceID: r.URL.Query().Get("source_instance_id")}
		if values, ok := r.URL.Query()["record_scope"]; ok {
			v := values[0]
			f.RecordScope = &v
		}
		if assetlogs.ValidateFilter(f) != nil {
			problem(w, 400, "invalid_query", "invalid asset log filter")
			return
		}
		if r.Pattern == "GET /api/assets/{id}/audit/events" {
			out, err = a.assetLogs.AssetAuditEvents(r.Context(), id, f)
		} else {
			out, err = a.assetLogs.AssetLogs(r.Context(), id, f)
		}
	}
	if errors.Is(err, assetlogs.ErrFilter) {
		problem(w, 400, "invalid_query", "invalid asset log filter")
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, out)
}
