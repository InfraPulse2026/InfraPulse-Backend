package api

import (
	"context"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"net/http"
)

type LogIncidentRepository interface {
	ReviewLogIncident(context.Context, string, health.LogReview) (health.Incident, error)
	LogIncidentRecords(context.Context, string, int, int) ([]health.LogIncidentRecord, error)
}

func (a *API) reviewLogEvent(w http.ResponseWriter, r *http.Request) {
	repo, ok := a.incidents.(LogIncidentRepository)
	if !ok {
		problem(w, 503, "log_incidents_unavailable", "log incident service unavailable")
		return
	}
	var input health.LogReview
	if !accessJSON(w, r, &input) {
		return
	}
	if err := input.Validate(); err != nil {
		problem(w, 400, "invalid_log_review", err.Error())
		return
	}
	v, err := repo.ReviewLogIncident(r.Context(), r.PathValue("id"), input)
	if errors.Is(err, health.ErrConflict) {
		problem(w, 409, "log_review_conflict", "only a log incident with matching event count can be reviewed")
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, v)
}
func (a *API) logIncidentRecords(w http.ResponseWriter, r *http.Request) {
	repo, ok := a.incidents.(LogIncidentRepository)
	if !ok {
		problem(w, 503, "log_incidents_unavailable", "log incident service unavailable")
		return
	}
	limit, e1 := number(r, "limit", 100, 1, 200)
	offset, e2 := number(r, "offset", 0, 0, 100000)
	if e1 != nil || e2 != nil {
		problem(w, 400, "invalid_query", "invalid pagination")
		return
	}
	v, err := repo.LogIncidentRecords(r.Context(), r.PathValue("id"), limit, offset)
	if errors.Is(err, health.ErrConflict) {
		problem(w, 409, "not_log_incident", "log incident required")
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, map[string]any{"items": v, "limit": limit, "offset": offset})
}
