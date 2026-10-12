package api

import (
	"context"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"net/http"
)

type RollbackRepository interface {
	CreateRollback(context.Context, string, commands.RollbackRequest, string) (commands.Record, bool, error)
	CommandSnapshot(context.Context, string) (commands.Snapshot, error)
	Snapshots(context.Context, string, string, int, int) ([]commands.Snapshot, error)
}

func (a *API) rollbackRepo(w http.ResponseWriter) (RollbackRepository, bool) {
	if !a.commandEnabled(w) {
		return nil, false
	}
	r, ok := a.commands.(RollbackRepository)
	if !ok {
		problem(w, 503, "rollback_unavailable", "rollback management is not configured")
	}
	return r, ok
}
func (a *API) createRollback(w http.ResponseWriter, r *http.Request) {
	if !a.authEnabled(w) {
		return
	}
	repo, ok := a.rollbackRepo(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !commandIDPattern.MatchString(id) {
		problem(w, 400, "invalid_request", "invalid origin command ID")
		return
	}
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 || commands.ValidateKey(r.Header.Get("Idempotency-Key")) != nil {
		problem(w, 400, "invalid_request", "one valid Idempotency-Key is required")
		return
	}
	var body commands.RollbackRequest
	if !accessJSON(w, r, &body) {
		return
	}
	if err := commands.ValidateRollback(body); err != nil {
		problem(w, 400, "invalid_request", err.Error())
		return
	}
	c, replay, err := repo.CreateRollback(r.Context(), id, body, keys[0])
	if err != nil {
		a.commandFail(w, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
		write(w, 200, c)
	} else {
		write(w, 202, c)
	}
}
func (a *API) commandSnapshot(w http.ResponseWriter, r *http.Request) {
	repo, ok := a.rollbackRepo(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !commandIDPattern.MatchString(id) {
		problem(w, 400, "invalid_request", "invalid command ID")
		return
	}
	snapshot, err := repo.CommandSnapshot(r.Context(), id)
	if err != nil {
		a.commandFail(w, err)
		return
	}
	write(w, 200, snapshot)
}
func (a *API) snapshotList(w http.ResponseWriter, r *http.Request) {
	repo, ok := a.rollbackRepo(w)
	if !ok {
		return
	}
	agent, device := r.URL.Query().Get("agent_id"), r.URL.Query().Get("device_id")
	if len(agent) > 256 || len(device) > 128 {
		problem(w, 400, "invalid_query", "identity filter too long")
		return
	}
	limit, err := number(r, "limit", 100, 1, 500)
	if err != nil {
		problem(w, 400, "invalid_query", "invalid limit")
		return
	}
	offset, err := number(r, "offset", 0, 0, 1000000)
	if err != nil {
		problem(w, 400, "invalid_query", "invalid offset")
		return
	}
	items, err := repo.Snapshots(r.Context(), agent, device, limit, offset)
	if err != nil {
		a.commandFail(w, err)
		return
	}
	if items == nil {
		items = []commands.Snapshot{}
	}
	write(w, 200, map[string]any{"items": items, "limit": limit, "offset": offset})
}
