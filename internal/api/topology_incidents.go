package api

import (
	"context"
	"errors"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
	"net/http"
)

type TopologyIncidentRepository interface {
	ReviewTopologyIncident(context.Context, string, health.LogReview) (health.Incident, error)
	TopologyIncidentRecords(context.Context, string, int, int) ([]health.TopologyIncidentRecord, error)
}

func (a *API) reviewTopologyEvent(w http.ResponseWriter, r *http.Request) {
	repo, ok := a.incidents.(TopologyIncidentRepository)
	if !ok {
		problem(w, 503, "topology_incidents_unavailable", "topology incident service unavailable")
		return
	}
	var input health.LogReview
	if !accessJSON(w, r, &input) {
		return
	}
	if err := input.Validate(); err != nil {
		problem(w, 400, "invalid_topology_review", err.Error())
		return
	}
	v, err := repo.ReviewTopologyIncident(r.Context(), r.PathValue("id"), input)
	if errors.Is(err, health.ErrConflict) {
		problem(w, 409, "topology_review_conflict", "only a topology incident with matching event count can be reviewed")
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, v)
}
func (a *API) topologyIncidentRecords(w http.ResponseWriter, r *http.Request) {
	repo, ok := a.incidents.(TopologyIncidentRepository)
	if !ok {
		problem(w, 503, "topology_incidents_unavailable", "topology incident service unavailable")
		return
	}
	limit, e1 := number(r, "limit", 100, 1, 200)
	offset, e2 := number(r, "offset", 0, 0, 100000)
	if e1 != nil || e2 != nil {
		problem(w, 400, "invalid_query", "invalid pagination")
		return
	}
	v, err := repo.TopologyIncidentRecords(r.Context(), r.PathValue("id"), limit, offset)
	if errors.Is(err, health.ErrConflict) {
		problem(w, 409, "not_topology_incident", "topology incident required")
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, map[string]any{"items": v, "limit": limit, "offset": offset})
}
