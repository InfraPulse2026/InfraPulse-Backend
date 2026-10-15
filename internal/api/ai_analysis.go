package api

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/analysisai"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
)

func WithAI(repo analysisai.Repository, cfg analysisai.Config) Option {
	return func(a *API) { a.ai = repo; a.aiConfig = cfg }
}
func (a *API) aiFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, analysisai.ErrExternalUnsupported):
		problem(w, 409, "external_analysis_unavailable", "external-source evidence export and correlation are planned in B12/B13")
	case errors.Is(err, analysisai.ErrDisabled):
		problem(w, 503, "ai_disabled", "external AI is disabled; preview is available")
	case errors.Is(err, analysisai.ErrInvalid):
		problem(w, 400, "invalid_ai_request", "invalid preview/hash/confirmation/key")
	case errors.Is(err, analysisai.ErrConflict):
		problem(w, 409, "ai_conflict", "preview expired/consumed, config changed, active job or idempotency conflict")
	case errors.Is(err, analysisai.ErrNotFound):
		problem(w, 404, "not_found", "AI analysis not found")
	default:
		a.fail(w, err)
	}
}
func (a *API) aiPreview(w http.ResponseWriter, r *http.Request) {
	if a.ai == nil {
		problem(w, 503, "ai_unavailable", "AI context service not configured")
		return
	}
	var body struct{}
	if !accessJSON(w, r, &body) {
		return
	}
	var preview analysisai.Preview
	var err error
	if strings.HasPrefix(r.URL.Path, "/api/correlation/") {
		if repo, ok := a.ai.(CorrelatedAIRepository); ok {
			preview, err = repo.CreateCorrelatedPreview(r.Context(), r.PathValue("id"), a.aiConfig)
		} else {
			err = analysisai.ErrDisabled
		}
	} else {
		preview, err = a.ai.CreatePreview(r.Context(), r.PathValue("id"), a.aiConfig)
	}
	if err != nil {
		a.aiFail(w, err)
		return
	}
	write(w, 201, preview)
}
func (a *API) aiRoutes(w http.ResponseWriter, r *http.Request) {
	if a.ai == nil {
		problem(w, 503, "ai_unavailable", "AI service not configured")
		return
	}
	if r.Method == "POST" {
		var body analysisai.Create
		if !accessJSON(w, r, &body) {
			return
		}
		hashBytes, hashErr := hex.DecodeString(body.RequestHash)
		if !body.Confirmed || body.PreviewID == "" || len(body.PreviewID) > 128 || hashErr != nil || len(hashBytes) != 32 {
			problem(w, 400, "invalid_ai_request", "valid preview_id/hash and confirmed=true required")
			return
		}
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 || commands.ValidateKey(keys[0]) != nil {
			problem(w, 400, "invalid_ai_request", "one valid Idempotency-Key required")
			return
		}
		var job analysisai.Job
		var replay bool
		var err error
		if strings.HasPrefix(r.URL.Path, "/api/correlation/") {
			if repo, ok := a.ai.(CorrelatedAIRepository); ok {
				job, replay, err = repo.CreateCorrelatedJob(r.Context(), r.PathValue("id"), body, keys[0], a.aiConfig)
			} else {
				err = analysisai.ErrDisabled
			}
		} else {
			job, replay, err = a.ai.CreateJob(r.Context(), r.PathValue("id"), body, keys[0], a.aiConfig)
		}
		if err != nil {
			a.aiFail(w, err)
			return
		}
		code := 202
		if replay {
			code = 200
			w.Header().Set("Idempotent-Replay", "true")
		}
		write(w, code, job)
		return
	}
	if analysis := r.PathValue("analysis_id"); analysis != "" {
		job, err := a.ai.Job(r.Context(), analysis)
		if err != nil {
			a.aiFail(w, err)
			return
		}
		write(w, 200, job)
		return
	}
	limit, err := number(r, "limit", 20, 1, 100)
	if err != nil {
		problem(w, 400, "invalid_query", "limit must be 1..100")
		return
	}
	items, err := a.ai.Jobs(r.Context(), r.PathValue("id"), limit)
	if err != nil {
		a.aiFail(w, err)
		return
	}
	write(w, 200, map[string]any{"items": items})
}

type CorrelatedAIRepository interface {
	CreateCorrelatedPreview(context.Context, string, analysisai.Config) (analysisai.Preview, error)
	CreateCorrelatedJob(context.Context, string, analysisai.Create, string, analysisai.Config) (analysisai.Job, bool, error)
	CorrelatedAnalysisJobs(context.Context, string, int) ([]analysisai.Job, error)
}

func (a *API) correlatedAnalysisJobs(w http.ResponseWriter, r *http.Request) {
	repo, ok := a.ai.(CorrelatedAIRepository)
	if !ok {
		problem(w, 503, "ai_unavailable", "AI service unavailable")
		return
	}
	limit, err := number(r, "limit", 20, 1, 100)
	if err != nil {
		problem(w, 400, "invalid_query", "invalid pagination")
		return
	}
	jobs, err := repo.CorrelatedAnalysisJobs(r.Context(), r.PathValue("id"), limit)
	if err != nil {
		a.aiFail(w, err)
		return
	}
	write(w, 200, map[string]any{"items": jobs})
}
