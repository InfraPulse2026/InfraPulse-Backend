package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
)

type DataSourceRepository interface {
	DataSources(context.Context, int, int) ([]datasources.Source, error)
	DataSource(context.Context, string) (datasources.Source, error)
	SaveDataSource(context.Context, string, datasources.Input) (datasources.Source, error)
}

func WithDataSources(repo DataSourceRepository) Option {
	return func(a *API) { a.dataSources = repo; a.sourceJobs, _ = repo.(DataSourceJobRepository) }
}

func (a *API) dataSourceFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, datasources.ErrUnsupported):
		problem(w, 409, "operation_unsupported", "this adapter has no implementation for the requested operation")
	case errors.Is(err, datasources.ErrBusy):
		problem(w, 409, "source_busy", "wait for the active source job to finish")
	case errors.Is(err, datasources.ErrDisabled):
		problem(w, 409, "source_disabled", "enable the source before starting a new operation")
	case errors.Is(err, datasources.ErrInvalid):
		problem(w, 400, "invalid_data_source", "use a supported source type, HTTPS endpoint without credentials/query, interval 3..86400, timeout 1..120 and current revision")
	case errors.Is(err, datasources.ErrConflict):
		problem(w, 409, "source_conflict", "reload current revision; Collector connection references must be unique")
	case errors.Is(err, datasources.ErrNotFound):
		problem(w, 404, "not_found", "data source not found")
	case errors.Is(err, datasources.ErrKeyUnavailable):
		problem(w, 503, "credential_storage_unavailable", "credential encryption key is not configured")
	default:
		a.fail(w, err)
	}
}
func (a *API) dataSourceRoutes(w http.ResponseWriter, r *http.Request) {
	if a.dataSources == nil {
		problem(w, 503, "data_sources_unavailable", "data sources are not configured")
		return
	}
	id := r.PathValue("id")
	if r.Method == "POST" || r.Method == "PUT" {
		var in datasources.Input
		if !accessJSON(w, r, &in) {
			return
		}
		v, err := a.dataSources.SaveDataSource(r.Context(), id, in)
		if err != nil {
			a.dataSourceFail(w, err)
			return
		}
		status := 200
		if r.Method == "POST" {
			status = 201
		}
		write(w, status, v)
		return
	}
	if id != "" {
		v, err := a.dataSources.DataSource(r.Context(), id)
		if err != nil {
			a.dataSourceFail(w, err)
			return
		}
		write(w, 200, v)
		return
	}
	limit, err := number(r, "limit", 50, 1, 200)
	if err != nil {
		a.dataSourceFail(w, datasources.ErrInvalid)
		return
	}
	offset, err := number(r, "offset", 0, 0, 100000)
	if err != nil {
		a.dataSourceFail(w, datasources.ErrInvalid)
		return
	}
	items, err := a.dataSources.DataSources(r.Context(), limit, offset)
	if err != nil {
		a.dataSourceFail(w, err)
		return
	}
	write(w, 200, map[string]any{"items": items, "limit": limit, "offset": offset})
}
