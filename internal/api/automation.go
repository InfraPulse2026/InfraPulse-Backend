package api

import (
	"context"
	"errors"
	au "github.com/InfraPulse2026/InfraPulse-Backend/internal/automation"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"net/http"
	"strings"
)

type AutomationRepository interface {
	AutomationTarget(context.Context, string) (au.Target, error)
	PutAutomationTarget(context.Context, string, au.TargetInput) (au.Target, error)
	AutomationWorkers(context.Context) ([]au.WorkerStatus, error)
	CreateAutomation(context.Context, string, au.Input, string) (au.Job, bool, error)
	AutomationJob(context.Context, string) (au.Job, error)
	AutomationJobs(context.Context, string, int, int) ([]au.Job, error)
	AutomationEvents(context.Context, string, int, int) ([]au.Event, error)
	CancelAutomation(context.Context, string) (au.Job, error)
	AutomationSnapshots(context.Context, string, int, int) ([]au.Snapshot, error)
	AutomationSnapshotContent(context.Context, string) (au.Result, error)
}

func WithAutomation(repo AutomationRepository) Option { return func(a *API) { a.automation = repo } }
func (a *API) automationFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, au.ErrInvalid):
		problem(w, 400, "invalid_automation_request", "use a reviewed template, exact Asset/target revisions, valid SSH host key and bounded timeout; arbitrary commands/playbooks are forbidden")
	case errors.Is(err, au.ErrConflict):
		problem(w, 409, "automation_conflict", "reload the Asset, target revision and job idempotency key")
	case errors.Is(err, au.ErrUnavailable):
		problem(w, 503, "automation_unavailable", "enable automation with encryption and a live compatible Linux worker")
	default:
		a.fail(w, err)
	}
}
func (a *API) automationRoute(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.automation == nil {
		a.automationFail(w, au.ErrUnavailable)
		return
	}
	path := r.URL.Path
	paged := (strings.HasSuffix(path, "/jobs") && r.Method == "GET") || strings.HasSuffix(path, "/events") || strings.HasSuffix(path, "/snapshots")
	allowed := map[string]bool{}
	if paged {
		allowed = map[string]bool{"limit": true, "offset": true}
	}
	if !strictB8Query(w, r, allowed) {
		return
	}
	for _, param := range []string{"id", "job_id", "snapshot_id"} {
		if value := r.PathValue(param); value != "" && !au.Identifier(value) {
			a.automationFail(w, au.ErrInvalid)
			return
		}
	}
	if r.Method == "GET" && strings.HasSuffix(path, "/templates") {
		write(w, 200, map[string]any{"items": au.Templates()})
		return
	}
	if r.Method == "GET" && strings.HasSuffix(path, "/workers") {
		out, err := a.automation.AutomationWorkers(r.Context())
		if err != nil {
			a.automationFail(w, err)
			return
		}
		write(w, 200, map[string]any{"items": out})
		return
	}
	asset := r.PathValue("id")
	job := r.PathValue("job_id")
	var out any
	var err error
	status := 200
	switch {
	case strings.HasSuffix(path, "/automation-target"):
		if r.Method == "PUT" {
			var in au.TargetInput
			if !boundedJSON(w, r, &in, 8192) {
				return
			}
			if in.Validate() != nil {
				a.automationFail(w, au.ErrInvalid)
				return
			}
			out, err = a.automation.PutAutomationTarget(r.Context(), asset, in)
		} else {
			out, err = a.automation.AutomationTarget(r.Context(), asset)
		}
	case r.Method == "POST" && strings.HasSuffix(path, "/automation/jobs"):
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 || commands.ValidateKey(keys[0]) != nil {
			a.automationFail(w, au.ErrInvalid)
			return
		}
		var in au.Input
		if !boundedJSON(w, r, &in, 2048) {
			return
		}
		if in.Validate() != nil {
			a.automationFail(w, au.ErrInvalid)
			return
		}
		var replay bool
		out, replay, err = a.automation.CreateAutomation(r.Context(), asset, in, keys[0])
		status = 202
		if replay {
			w.Header().Set("Idempotent-Replay", "true")
		}
	case r.Method == "POST" && strings.HasSuffix(path, "/cancel"):
		if r.ContentLength != 0 {
			a.automationFail(w, au.ErrInvalid)
			return
		}
		out, err = a.automation.CancelAutomation(r.Context(), job)
		status = 202
	case strings.HasSuffix(path, "/content"):
		out, err = a.automation.AutomationSnapshotContent(r.Context(), r.PathValue("snapshot_id"))
	case paged:
		limit, e := number(r, "limit", 50, 1, 200)
		if e != nil {
			a.automationFail(w, au.ErrInvalid)
			return
		}
		offset, e := number(r, "offset", 0, 0, 100000)
		if e != nil {
			a.automationFail(w, au.ErrInvalid)
			return
		}
		if strings.HasSuffix(path, "/events") {
			out, err = a.automation.AutomationEvents(r.Context(), job, limit, offset)
		} else if strings.HasSuffix(path, "/snapshots") {
			out, err = a.automation.AutomationSnapshots(r.Context(), asset, limit, offset)
		} else {
			out, err = a.automation.AutomationJobs(r.Context(), asset, limit, offset)
		}
		if err == nil {
			out = map[string]any{"items": out, "limit": limit, "offset": offset}
		}
	default:
		out, err = a.automation.AutomationJob(r.Context(), job)
	}
	if err != nil {
		a.automationFail(w, err)
		return
	}
	write(w, status, out)
}
