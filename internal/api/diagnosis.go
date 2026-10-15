package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/diagnosis"
)

type diagnosticRepository interface {
	IncidentContext(context.Context, string, int, int) (diagnosis.Context, error)
}

func (a *API) incidentContext(w http.ResponseWriter, r *http.Request) {
	repo, ok := a.incidents.(diagnosticRepository)
	if !ok {
		problem(w, 503, "diagnosis_unavailable", "diagnostic context is not configured")
		return
	}
	window, e1 := number(r, "window_seconds", 300, 60, 3600)
	limit, e2 := number(r, "limit", 20, 1, 50)
	if e1 != nil || e2 != nil {
		problem(w, 400, "invalid_query", "window_seconds=60..3600, limit=1..50 required")
		return
	}
	result, err := repo.IncidentContext(r.Context(), r.PathValue("id"), window, limit)
	if errors.Is(err, diagnosis.ErrInvalid) {
		problem(w, 400, "invalid_query", err.Error())
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, result)
}
