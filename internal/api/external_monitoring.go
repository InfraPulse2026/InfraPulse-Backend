package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/externalmonitor"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
)

type ExternalMonitoringRepository interface {
	ExternalMetrics(context.Context, externalmonitor.Filter, bool) ([]externalmonitor.MetricRecord, error)
	ExternalSignals(context.Context, externalmonitor.Filter) ([]externalmonitor.SignalRecord, error)
	ExternalSignalHistory(context.Context, string, string, int, int) ([]externalmonitor.SignalVersion, error)
}

func (a *API) externalMonitoring(w http.ResponseWriter, r *http.Request) {
	if a.external == nil {
		problem(w, 503, "external_monitoring_unavailable", "external monitoring is not configured")
		return
	}
	limit, e1 := number(r, "limit", 50, 1, 200)
	offset, e2 := number(r, "offset", 0, 0, 100000)
	if e1 != nil || e2 != nil {
		problem(w, 400, "invalid_query", "invalid pagination")
		return
	}
	f := externalmonitor.Filter{SourceID: r.URL.Query().Get("source_instance_id"), AssetID: r.URL.Query().Get("asset_id"), ItemID: r.URL.Query().Get("item_id"), State: r.URL.Query().Get("state"), Limit: limit, Offset: offset}
	if strings.HasPrefix(r.URL.Path, "/api/assets/") {
		f.AssetID = r.PathValue("id")
	}
	if strings.HasPrefix(r.URL.Path, "/api/data-sources/") {
		f.SourceID = r.PathValue("id")
	}
	if f.ItemID != "" && !externalmonitor.ValidID(f.ItemID) && !prommodel.MetricIDValid(f.ItemID) || f.State != "" && f.State != "active" && f.State != "resolved" || r.PathValue("event_id") != "" && !externalmonitor.ValidID(r.PathValue("event_id")) {
		problem(w, 400, "invalid_query", "invalid state or external ID")
		return
	}
	var result any
	var err error
	switch {
	case r.PathValue("event_id") != "":
		result, err = a.external.ExternalSignalHistory(r.Context(), f.SourceID, r.PathValue("event_id"), limit, offset)
	case strings.Contains(r.URL.Path, "metrics"):
		result, err = a.external.ExternalMetrics(r.Context(), f, strings.HasSuffix(r.URL.Path, "/history"))
	default:
		result, err = a.external.ExternalSignals(r.Context(), f)
	}
	if err != nil {
		if errors.Is(err, prommodel.ErrHistory) {
			problem(w, 409, "history_on_demand", "use the Asset Prometheus range endpoint")
		} else if errors.Is(err, externalmonitor.ErrInvalid) {
			problem(w, 400, "invalid_query", "use valid IDs, state and pagination")
		} else {
			a.fail(w, err)
		}
		return
	}
	write(w, 200, map[string]any{"items": result, "limit": limit, "offset": offset})
}
