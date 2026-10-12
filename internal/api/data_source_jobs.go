package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/datasources"
)

type DataSourceJobRepository interface {
	QueueDataSourceJob(context.Context, string, string, string) (datasources.Job, bool, error)
	DataSourceJobs(context.Context, string, int, int) ([]datasources.Job, error)
	DataSourceJob(context.Context, string, string) (datasources.Job, error)
	DataSourceJobEvents(context.Context, string, string) ([]datasources.Event, error)
}

func (a *API) dataSourceJobRoutes(w http.ResponseWriter, r *http.Request) {
	if a.sourceJobs == nil {
		problem(w, 503, "source_jobs_unavailable", "source queue is not configured")
		return
	}
	id, jobID := r.PathValue("id"), r.PathValue("job_id")
	if r.Method == "POST" {
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 || commands.ValidateKey(keys[0]) != nil {
			problem(w, 400, "invalid_request", "one valid Idempotency-Key is required")
			return
		}
		operation := "test"
		if strings.HasSuffix(r.URL.Path, "/sync") {
			operation = "sync"
		}
		j, replay, err := a.sourceJobs.QueueDataSourceJob(r.Context(), id, operation, keys[0])
		if err != nil {
			a.dataSourceFail(w, err)
			return
		}
		if replay {
			w.Header().Set("Idempotent-Replay", "true")
		}
		write(w, 202, j)
		return
	}
	if jobID != "" {
		if strings.HasSuffix(r.URL.Path, "/events") {
			items, err := a.sourceJobs.DataSourceJobEvents(r.Context(), id, jobID)
			if err != nil {
				a.dataSourceFail(w, err)
				return
			}
			write(w, 200, map[string]any{"items": items})
			return
		}
		j, err := a.sourceJobs.DataSourceJob(r.Context(), id, jobID)
		if err != nil {
			a.dataSourceFail(w, err)
			return
		}
		write(w, 200, j)
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
	items, err := a.sourceJobs.DataSourceJobs(r.Context(), id, limit, offset)
	if err != nil {
		a.dataSourceFail(w, err)
		return
	}
	write(w, 200, map[string]any{"items": items, "limit": limit, "offset": offset})
}
