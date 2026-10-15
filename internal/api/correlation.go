package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/correlation"
)

type CorrelationRepository interface {
	CorrelationSummary(context.Context) (map[string]any, error)
	CorrelatedIncidents(context.Context, correlation.Filter) ([]correlation.Incident, error)
	CorrelatedIncident(context.Context, string) (correlation.Case, error)
	RebuildCorrelation(context.Context) (int, error)
	CorrelationServices(context.Context, int, int) ([]correlation.Service, error)
	CorrelationService(context.Context, string) (correlation.Service, error)
	SaveCorrelationService(context.Context, string, correlation.ServiceInput, string) (correlation.Service, bool, error)
}

func WithCorrelation(repo CorrelationRepository) Option { return func(a *API) { a.correlation = repo } }

func correlationError(w http.ResponseWriter, err error) bool {
	if errors.Is(err, correlation.ErrInvalid) {
		problem(w, 400, "invalid_correlation_request", "valid filters, a closed service dependency set, current revision, confirmation, reason and one Idempotency-Key are required")
		return true
	}
	if errors.Is(err, correlation.ErrConflict) {
		problem(w, 409, "correlation_conflict", "service revision, Asset state or bounded rebuild precondition changed")
		return true
	}
	return false
}

func (a *API) correlationIncidents(w http.ResponseWriter, r *http.Request) {
	if a.correlation == nil {
		problem(w, 503, "correlation_unavailable", "correlation service is not configured")
		return
	}
	if r.URL.Path == "/api/correlation/summary" {
		v, err := a.correlation.CorrelationSummary(r.Context())
		if err != nil {
			a.fail(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		write(w, 200, v)
		return
	}
	id := r.PathValue("id")
	if id != "" {
		v, err := a.correlation.CorrelatedIncident(r.Context(), id)
		if err != nil {
			a.fail(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		write(w, 200, v)
		return
	}
	limit, e1 := number(r, "limit", 100, 1, 200)
	offset, e2 := number(r, "offset", 0, 0, 100000)
	state, severity := r.URL.Query().Get("state"), r.URL.Query().Get("severity")
	if e1 != nil || e2 != nil || state != "" && state != "open" && state != "resolved" || severity != "" && severity != "warning" && severity != "critical" {
		problem(w, 400, "invalid_query", "invalid state, severity or pagination")
		return
	}
	f := correlation.Filter{State: state, Severity: severity, AssetID: r.URL.Query().Get("asset_id"), ServiceID: r.URL.Query().Get("service_id"), Limit: limit, Offset: offset}
	items, err := a.correlation.CorrelatedIncidents(r.Context(), f)
	if correlationError(w, err) {
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	write(w, 200, map[string]any{"items": items, "limit": limit, "offset": offset})
}

func (a *API) correlationServices(w http.ResponseWriter, r *http.Request) {
	if a.correlation == nil {
		problem(w, 503, "correlation_unavailable", "correlation service is not configured")
		return
	}
	id := r.PathValue("id")
	if r.Method == http.MethodGet {
		if id != "" {
			v, err := a.correlation.CorrelationService(r.Context(), id)
			if err != nil {
				a.fail(w, err)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			write(w, 200, v)
			return
		}
		limit, e1 := number(r, "limit", 100, 1, 200)
		offset, e2 := number(r, "offset", 0, 0, 100000)
		if e1 != nil || e2 != nil {
			problem(w, 400, "invalid_query", "invalid pagination")
			return
		}
		items, err := a.correlation.CorrelationServices(r.Context(), limit, offset)
		if correlationError(w, err) {
			return
		}
		if err != nil {
			a.fail(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		write(w, 200, map[string]any{"items": items, "limit": limit, "offset": offset})
		return
	}
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 || commands.ValidateKey(keys[0]) != nil {
		problem(w, 400, "invalid_correlation_request", "one valid Idempotency-Key is required")
		return
	}
	var in correlation.ServiceInput
	if !accessJSON(w, r, &in) {
		return
	}
	if err := in.Validate(id == ""); correlationError(w, err) {
		return
	}
	v, replayed, err := a.correlation.SaveCorrelationService(r.Context(), id, in, keys[0])
	if correlationError(w, err) {
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	w.Header().Set("Idempotent-Replay", map[bool]string{true: "true", false: "false"}[replayed])
	status := 200
	if id == "" && !replayed {
		status = 201
	}
	write(w, status, v)
}

func (a *API) rebuildCorrelation(w http.ResponseWriter, r *http.Request) {
	if a.correlation == nil {
		problem(w, 503, "correlation_unavailable", "correlation service is not configured")
		return
	}
	var body struct {
		Confirmed bool `json:"confirmed"`
	}
	if !accessJSON(w, r, &body) {
		return
	}
	if !body.Confirmed {
		problem(w, 400, "confirmation_required", "confirmed=true required")
		return
	}
	count, err := a.correlation.RebuildCorrelation(r.Context())
	if correlationError(w, err) {
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	write(w, 202, map[string]any{"queued_incidents": count})
}
