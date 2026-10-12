package api

import (
	"context"
	"errors"
	cc "github.com/InfraPulse2026/InfraPulse-Backend/internal/collectorconfig"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"net/http"
	"strings"
)

type CollectorConfigRepository interface {
	CollectorConfig(context.Context, string) (cc.State, error)
	PutCollectorConfig(context.Context, string, cc.Input, string) (cc.Revision, bool, error)
	CollectorConfigHistory(context.Context, string, int, int) ([]cc.Revision, error)
	CollectorConfigEvents(context.Context, string, int, int) ([]cc.Event, error)
}

func WithCollectorConfig(repo CollectorConfigRepository) Option {
	return func(a *API) { a.collectorConfig = repo }
}
func (a *API) collectorConfigFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, cc.ErrInvalid):
		problem(w, 400, "invalid_collector_config", "a closed full target snapshot, current revision, confirmation, reason and one Idempotency-Key are required; credentials must be local references")
	case errors.Is(err, cc.ErrConflict):
		problem(w, 409, "collector_config_conflict", "reload the desired revision and check Collector managed_config_v1, remote opt-in and revision range")
	case errors.Is(err, cc.ErrDisabled):
		problem(w, 503, "collector_config_disabled", "managed Collector configuration is disabled on backend")
	default:
		a.fail(w, err)
	}
}
func (a *API) collectorConfigRoute(w http.ResponseWriter, r *http.Request) {
	if a.collectorConfig == nil {
		a.collectorConfigFail(w, cc.ErrDisabled)
		return
	}
	allowed := map[string]bool{}
	if strings.HasSuffix(r.URL.Path, "/history") || strings.HasSuffix(r.URL.Path, "/events") {
		allowed = map[string]bool{"limit": true, "offset": true}
	}
	if !strictB8Query(w, r, allowed) {
		return
	}
	id := r.PathValue("id")
	if !cc.Identifier(id) {
		a.collectorConfigFail(w, cc.ErrInvalid)
		return
	}
	if r.Method == "PUT" {
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 || commands.ValidateKey(keys[0]) != nil {
			a.collectorConfigFail(w, cc.ErrInvalid)
			return
		}
		var in cc.Input
		if !boundedJSON(w, r, &in, cc.MaxBytes) {
			return
		}
		out, replay, err := a.collectorConfig.PutCollectorConfig(r.Context(), id, in, keys[0])
		if err != nil {
			a.collectorConfigFail(w, err)
			return
		}
		if replay {
			w.Header().Set("Idempotent-Replay", "true")
		}
		write(w, 202, out)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/history") || strings.HasSuffix(r.URL.Path, "/events") {
		limit, err := number(r, "limit", 50, 1, 200)
		if err != nil {
			a.collectorConfigFail(w, cc.ErrInvalid)
			return
		}
		offset, err := number(r, "offset", 0, 0, 100000)
		if err != nil {
			a.collectorConfigFail(w, cc.ErrInvalid)
			return
		}
		var out any
		if strings.HasSuffix(r.URL.Path, "/events") {
			out, err = a.collectorConfig.CollectorConfigEvents(r.Context(), id, limit, offset)
		} else {
			out, err = a.collectorConfig.CollectorConfigHistory(r.Context(), id, limit, offset)
		}
		if err != nil {
			a.collectorConfigFail(w, err)
			return
		}
		write(w, 200, map[string]any{"items": out, "limit": limit, "offset": offset})
		return
	}
	out, err := a.collectorConfig.CollectorConfig(r.Context(), id)
	if err != nil {
		a.collectorConfigFail(w, err)
		return
	}
	write(w, 200, out)
}
