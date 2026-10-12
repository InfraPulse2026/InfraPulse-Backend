package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	au "github.com/InfraPulse2026/InfraPulse-Backend/internal/automation"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	rm "github.com/InfraPulse2026/InfraPulse-Backend/internal/remediation"
)

type RemediationRepository interface {
	CreateRemediation(context.Context, string, rm.CreateInput, string) (rm.Proposal, bool, error)
	Remediation(context.Context, string) (rm.Proposal, error)
	ApproveRemediation(context.Context, string, rm.ApproveInput) (rm.Proposal, bool, error)
	RejectRemediation(context.Context, string, rm.RejectInput) (rm.Proposal, error)
	ReconcileRemediation(context.Context, string, rm.ReconcileInput) (rm.Proposal, error)
	RemediationHistory(context.Context, string, int, int) ([]rm.HistoryEvent, error)
}

func WithRemediation(repo RemediationRepository) Option { return func(a *API) { a.remediation = repo } }

func (a *API) remediationFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, rm.ErrInvalid):
		problem(w, 422, "invalid_remediation_action", "use a saved drift, reviewed reason and exact proposal digest; action parameters are immutable")
	case errors.Is(err, rm.ErrConflict), errors.Is(err, rm.ErrExpired):
		problem(w, 409, "remediation_conflict", "reload the proposal; stale, expired or changed actions require a new proposal and approval")
	case errors.Is(err, rm.ErrPolicy):
		problem(w, 403, "remediation_policy_denied", "remediation is not permitted for this user, Asset, interface, VLAN or template")
	case errors.Is(err, rm.ErrUnavailable), errors.Is(err, au.ErrUnavailable):
		problem(w, 503, "remediation_unavailable", "remediation policy, coordinated schema and a compatible worker must be available")
	default:
		a.fail(w, err)
	}
}

func (a *API) remediationRoute(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.remediation == nil {
		a.remediationFail(w, rm.ErrUnavailable)
		return
	}
	id := r.PathValue("id")
	if !au.Identifier(id) {
		a.remediationFail(w, rm.ErrInvalid)
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
	status := http.StatusOK
	switch {
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/configuration/remediations"):
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 || commands.ValidateKey(keys[0]) != nil {
			a.remediationFail(w, rm.ErrInvalid)
			return
		}
		var in rm.CreateInput
		if !boundedJSON(w, r, &in, 2048) {
			return
		}
		if in.Validate() != nil {
			a.remediationFail(w, rm.ErrInvalid)
			return
		}
		var proposal rm.Proposal
		var replay bool
		proposal, replay, err = a.remediation.CreateRemediation(r.Context(), id, in, keys[0])
		out = proposal
		status = http.StatusCreated
		if err == nil {
			w.Header().Set("Location", "/api/remediations/"+proposal.ID)
		}
		if replay {
			w.Header().Set("Idempotent-Replay", "true")
		}
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/approve"):
		var in rm.ApproveInput
		if !boundedJSON(w, r, &in, 2048) {
			return
		}
		if in.Validate() != nil {
			a.remediationFail(w, rm.ErrInvalid)
			return
		}
		var replay bool
		out, replay, err = a.remediation.ApproveRemediation(r.Context(), id, in)
		status = http.StatusAccepted
		if err == nil {
			w.Header().Set("Location", "/api/remediations/"+id)
		}
		if replay {
			w.Header().Set("Idempotent-Replay", "true")
		}
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/reject"):
		var in rm.RejectInput
		if !boundedJSON(w, r, &in, 2048) {
			return
		}
		if in.Validate() != nil {
			a.remediationFail(w, rm.ErrInvalid)
			return
		}
		out, err = a.remediation.RejectRemediation(r.Context(), id, in)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/reconcile"):
		var in rm.ReconcileInput
		if !boundedJSON(w, r, &in, 2048) {
			return
		}
		if in.Validate() != nil {
			a.remediationFail(w, rm.ErrInvalid)
			return
		}
		out, err = a.remediation.ReconcileRemediation(r.Context(), id, in)
	case history:
		limit, e := number(r, "limit", 50, 1, 200)
		if e != nil {
			a.remediationFail(w, rm.ErrInvalid)
			return
		}
		offset, e := number(r, "offset", 0, 0, 100000)
		if e != nil {
			a.remediationFail(w, rm.ErrInvalid)
			return
		}
		var events []rm.HistoryEvent
		events, err = a.remediation.RemediationHistory(r.Context(), id, limit, offset)
		if events == nil {
			events = []rm.HistoryEvent{}
		}
		out = map[string]any{"items": events, "limit": limit, "offset": offset}
	default:
		out, err = a.remediation.Remediation(r.Context(), id)
	}
	if err != nil {
		a.remediationFail(w, err)
		return
	}
	write(w, status, out)
}
