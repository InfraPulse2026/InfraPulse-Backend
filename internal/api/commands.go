package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
)

type CommandRepository interface {
	CreateCommand(context.Context, commands.Request, string) (commands.Record, bool, error)
	Command(context.Context, string) (commands.Record, error)
	Commands(context.Context, commands.Filter) ([]commands.Record, error)
	CommandEvents(context.Context, string) ([]commands.Event, error)
	CommandPolicy() commands.Policy
}

func WithCommands(repo CommandRepository) Option { return func(a *API) { a.commands = repo } }
func (a *API) commandEnabled(w http.ResponseWriter) bool {
	if a.commands == nil {
		problem(w, 503, "commands_unavailable", "command management is not configured")
		return false
	}
	return true
}
func (a *API) commandFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, access.ErrForbidden) || errors.Is(err, access.ErrCredentials):
		a.accessFail(w, err)
	case errors.Is(err, commands.ErrDisabled):
		problem(w, 403, "commands_disabled", "enable the configured backend command policy first")
	case errors.Is(err, commands.ErrProtected):
		problem(w, 403, "protected_resource", "target interface or device is protected")
	case errors.Is(err, commands.ErrPrecondition):
		problem(w, 409, "command_precondition", "fresh observations, supported capability and expected state are required")
	case errors.Is(err, commands.ErrConflict):
		problem(w, 409, "idempotency_conflict", "key already belongs to a different request")
	case errors.Is(err, commands.ErrSnapshot):
		problem(w, 409, "snapshot_unavailable", "snapshot reference is unavailable, superseded, reserved or incompatible")
	case errors.Is(err, commands.ErrBusy):
		problem(w, 409, "agent_command_busy", "wait for the active command or resolve delivery uncertainty")
	case errors.Is(err, commands.ErrNotFound):
		problem(w, 404, "not_found", "command not found")
	default:
		a.fail(w, err)
	}
}
func (a *API) createCommand(w http.ResponseWriter, r *http.Request) {
	if !a.authEnabled(w) || !a.commandEnabled(w) {
		return
	}
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 || commands.ValidateKey(r.Header.Get("Idempotency-Key")) != nil {
		problem(w, 400, "invalid_request", "one valid Idempotency-Key header is required")
		return
	}
	var request commands.Request
	if !accessJSON(w, r, &request) {
		return
	}
	if err := commands.ValidateRequest(request); err != nil {
		problem(w, 400, "invalid_request", err.Error())
		return
	}
	record, replay, err := a.commands.CreateCommand(r.Context(), request, keys[0])
	if err != nil {
		a.commandFail(w, err)
		return
	}
	if replay {
		w.Header().Set("Idempotent-Replay", "true")
		write(w, 200, record)
	} else {
		write(w, 202, record)
	}
}

var commandIDPattern = regexp.MustCompile(`^cmd_[a-f0-9]{32}$`)

func (a *API) command(w http.ResponseWriter, r *http.Request) {
	if !a.commandEnabled(w) {
		return
	}
	id := r.PathValue("id")
	if !commandIDPattern.MatchString(id) {
		problem(w, 400, "invalid_request", "invalid command ID")
		return
	}
	record, err := a.commands.Command(r.Context(), id)
	if err != nil {
		a.commandFail(w, err)
		return
	}
	if r.Pattern == "GET /api/commands/{id}/events" {
		events, err := a.commands.CommandEvents(r.Context(), id)
		if err != nil {
			a.commandFail(w, err)
			return
		}
		write(w, 200, map[string]any{"items": events})
		return
	}
	write(w, 200, record)
}
func (a *API) commandList(w http.ResponseWriter, r *http.Request) {
	if !a.commandEnabled(w) {
		return
	}
	f := commands.Filter{AgentID: r.URL.Query().Get("agent_id"), DeviceID: r.URL.Query().Get("device_id"), State: r.URL.Query().Get("state")}
	if len(f.AgentID) > 256 || len(f.DeviceID) > 128 {
		problem(w, 400, "invalid_query", "identity filter too long")
		return
	}
	if f.State != "" {
		switch f.State {
		case "queued", "dispatching", "sent", "accepted", "running", "success", "failed", "rejected", "unsupported", "timeout", "expired", "uncertain":
		default:
			problem(w, 400, "invalid_query", "unknown command state")
			return
		}
	}
	var err error
	f.Limit, err = number(r, "limit", 100, 1, 500)
	if err != nil {
		problem(w, 400, "invalid_query", "invalid limit")
		return
	}
	f.Offset, err = number(r, "offset", 0, 0, 1000000)
	if err != nil {
		problem(w, 400, "invalid_query", "invalid offset")
		return
	}
	items, err := a.commands.Commands(r.Context(), f)
	if err != nil {
		a.commandFail(w, err)
		return
	}
	if items == nil {
		items = []commands.Record{}
	}
	write(w, 200, map[string]any{"items": items, "limit": f.Limit, "offset": f.Offset})
}
func (a *API) commandPolicy(w http.ResponseWriter, r *http.Request) {
	if !a.commandEnabled(w) {
		return
	}
	write(w, 200, a.commands.CommandPolicy())
}
