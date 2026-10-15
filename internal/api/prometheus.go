package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/prommodel"
)

type PrometheusRepository interface {
	PrometheusQuery(context.Context, prommodel.Query) (prommodel.QueryResult, error)
}

func (a *API) prometheusQuery(w http.ResponseWriter, r *http.Request) {
	if a.prometheus == nil {
		problem(w, 503, "prometheus_unavailable", "Prometheus query service is not configured")
		return
	}
	q := prommodel.Query{AssetID: r.PathValue("id"), SourceID: r.URL.Query().Get("source_instance_id"), TargetID: r.URL.Query().Get("target_id"), MappingID: r.URL.Query().Get("mapping_id"), Kind: "instant"}
	if strings.HasSuffix(r.URL.Path, "/metadata") {
		q.Kind = "metadata"
	}
	if strings.HasSuffix(r.URL.Path, "/range") {
		q.Kind = "range"
		var err error
		q.From, err = time.Parse(time.RFC3339Nano, r.URL.Query().Get("from"))
		if err != nil {
			problem(w, 400, "invalid_query", "from/to must be RFC3339 timestamps")
			return
		}
		q.To, err = time.Parse(time.RFC3339Nano, r.URL.Query().Get("to"))
		if err != nil {
			problem(w, 400, "invalid_query", "from/to must be RFC3339 timestamps")
			return
		}
		q.StepSeconds, err = number(r, "step_seconds", 60, 3, 86400)
		if err != nil {
			problem(w, 400, "invalid_query", "invalid step_seconds")
			return
		}
	}
	// No viewer-supplied raw PromQL/URL/token can replace persisted mappings.
	allowed := map[string]bool{"source_instance_id": true, "target_id": true, "mapping_id": true, "from": true, "to": true, "step_seconds": true}
	for key, values := range r.URL.Query() {
		if !allowed[key] || len(values) != 1 {
			problem(w, 400, "invalid_query", "use one value per documented parameter")
			return
		}
	}
	if q.Validate(time.Now()) != nil {
		problem(w, 400, "invalid_query", "use source/target/mapping IDs; range <=14d and <=3000 points")
		return
	}
	result, err := a.prometheus.PrometheusQuery(r.Context(), q)
	if err != nil {
		switch {
		case errors.Is(err, prommodel.ErrInvalid):
			problem(w, 400, "invalid_query", "invalid Prometheus query mapping or target")
		case errors.Is(err, datasources.ErrConflict), errors.Is(err, datasources.ErrDisabled), errors.Is(err, datasources.ErrUnsupported), errors.Is(err, datasources.ErrKeyUnavailable):
			a.dataSourceFail(w, err)
		default:
			code, _ := datasources.Outcome(err)
			if code != "adapter_error" {
				status := 502
				if code == "timeout" {
					status = 504
				}
				problem(w, status, code, "Prometheus request did not produce a complete valid response")
			} else {
				a.fail(w, err)
			}
		}
		return
	}
	write(w, 200, result)
}
