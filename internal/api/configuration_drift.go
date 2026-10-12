package api

import (
	"context"
	"errors"
	au "github.com/InfraPulse2026/InfraPulse-Backend/internal/automation"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	cd "github.com/InfraPulse2026/InfraPulse-Backend/internal/configdrift"
	"net/http"
	"strings"
)

type ConfigurationDriftRepository interface {
	ConfigurationBaseline(context.Context, string) (cd.Baseline, error)
	PutConfigurationBaseline(context.Context, string, cd.BaselineInput, string) (cd.Baseline, bool, error)
	ConfigurationBaselineHistory(context.Context, string, int, int) ([]cd.Baseline, error)
	ConfigurationDrift(context.Context, string) (cd.State, error)
	ConfigurationDriftHistory(context.Context, string, int, int) ([]cd.Record, error)
	CompareConfiguration(context.Context, string, cd.CompareInput) (cd.Record, error)
}

func WithConfigurationDrift(repo ConfigurationDriftRepository) Option {
	return func(a *API) { a.configurationDrift = repo }
}
func (a *API) configurationDriftFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, cd.ErrInvalid) || errors.Is(err, cd.ErrUnsupported):
		problem(w, 400, "invalid_configuration_drift_request", "use a supported successful same-Asset snapshot, current Asset/baseline revisions and a reviewed reason")
	case errors.Is(err, cd.ErrConflict):
		problem(w, 409, "configuration_baseline_conflict", "reload the baseline, Asset revision and snapshot origin; do not reuse an idempotency key for different content")
	case errors.Is(err, au.ErrUnavailable):
		a.automationFail(w, err)
	default:
		a.fail(w, err)
	}
}
func (a *API) configurationDriftRoute(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.configurationDrift == nil {
		a.configurationDriftFail(w, au.ErrUnavailable)
		return
	}
	id := r.PathValue("id")
	if !au.Identifier(id) {
		a.configurationDriftFail(w, cd.ErrInvalid)
		return
	}
	history := strings.HasSuffix(r.URL.Path, "/history")
	allowed := map[string]bool{}
	if history {
		allowed = map[string]bool{"limit": true, "offset": true}
	}
	if !strictB8Query(w, r, allowed) {
		return
	}
	var out any
	var err error
	switch {
	case r.Method == "PUT":
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 || commands.ValidateKey(keys[0]) != nil {
			a.configurationDriftFail(w, cd.ErrInvalid)
			return
		}
		var in cd.BaselineInput
		if !boundedJSON(w, r, &in, 2048) {
			return
		}
		if in.Validate() != nil {
			a.configurationDriftFail(w, cd.ErrInvalid)
			return
		}
		var replay bool
		out, replay, err = a.configurationDrift.PutConfigurationBaseline(r.Context(), id, in, keys[0])
		if replay {
			w.Header().Set("Idempotent-Replay", "true")
		}
	case r.Method == "POST":
		var in cd.CompareInput
		if !boundedJSON(w, r, &in, 2048) {
			return
		}
		if in.Validate() != nil {
			a.configurationDriftFail(w, cd.ErrInvalid)
			return
		}
		out, err = a.configurationDrift.CompareConfiguration(r.Context(), id, in)
	case history:
		limit, e := number(r, "limit", 50, 1, 200)
		if e != nil {
			a.configurationDriftFail(w, cd.ErrInvalid)
			return
		}
		offset, e := number(r, "offset", 0, 0, 100000)
		if e != nil {
			a.configurationDriftFail(w, cd.ErrInvalid)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/baseline/history") {
			out, err = a.configurationDrift.ConfigurationBaselineHistory(r.Context(), id, limit, offset)
		} else {
			out, err = a.configurationDrift.ConfigurationDriftHistory(r.Context(), id, limit, offset)
		}
		if err == nil {
			out = map[string]any{"items": out, "limit": limit, "offset": offset}
		}
	case strings.HasSuffix(r.URL.Path, "/baseline"):
		out, err = a.configurationDrift.ConfigurationBaseline(r.Context(), id)
	default:
		out, err = a.configurationDrift.ConfigurationDrift(r.Context(), id)
	}
	if err != nil {
		a.configurationDriftFail(w, err)
		return
	}
	write(w, 200, out)
}
