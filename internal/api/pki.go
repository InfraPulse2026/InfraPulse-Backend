package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/access"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/pki"
)

func WithPKI(service *pki.Service) Option { return func(a *API) { a.pki = service } }

func (a *API) pkiFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, pki.ErrConflict):
		problem(w, 409, "pki_conflict", "credential already used/revoked or active credential limit reached")
	case errors.Is(err, pki.ErrNotFound):
		problem(w, 404, "not_found", "PKI resource not found")
	case errors.Is(err, pki.ErrDenied):
		problem(w, 401, "pki_rejected", "PKI credential rejected")
	default:
		a.fail(w, err)
	}
}
func (a *API) pkiManagement(w http.ResponseWriter, r *http.Request) {
	if a.pki == nil {
		problem(w, 503, "pki_disabled", "PKI is not enabled")
		return
	}
	if _, ok := access.Current(r.Context()); !ok {
		a.accessFail(w, access.ErrForbidden)
		return
	}
	repo := a.pki.Repository
	if r.Method == http.MethodPost {
		if id := r.PathValue("id"); id != "" {
			var body struct {
				Confirmed bool `json:"confirmed"`
			}
			if !accessJSON(w, r, &body) {
				return
			}
			if !body.Confirmed {
				problem(w, 400, "confirmation_required", "confirmed=true is required")
				return
			}
			if r.URL.Path == "/api/pki/bootstrap-tokens/"+id+"/revoke" {
				if err := repo.PKIRevokeToken(r.Context(), id); err != nil {
					a.pkiFail(w, err)
					return
				}
			} else {
				agent, err := repo.PKIRevokeCertificate(r.Context(), id)
				if err != nil {
					a.pkiFail(w, err)
					return
				}
				if a.pki.OnRevoke != nil {
					a.pki.OnRevoke(agent)
				}
			}
			write(w, 200, map[string]string{"status": "revoked"})
			return
		}
		var body struct {
			AgentID    string `json:"agent_id"`
			TTLSeconds int    `json:"ttl_seconds"`
		}
		if !accessJSON(w, r, &body) {
			return
		}
		if body.TTLSeconds == 0 {
			body.TTLSeconds = 900
		}
		if body.TTLSeconds < 60 || body.TTLSeconds > 86400 {
			problem(w, 400, "invalid_ttl", "ttl_seconds must be 60..86400")
			return
		}
		token, err := pki.NewToken(body.AgentID, time.Duration(body.TTLSeconds)*time.Second)
		if err != nil {
			problem(w, 400, "invalid_agent_id", "canonical non-nil UUID required")
			return
		}
		if err = repo.PKICreateToken(r.Context(), token); err != nil {
			a.pkiFail(w, err)
			return
		}
		write(w, 201, token)
		return
	}
	id := r.URL.Query().Get("agent_id")
	if id != "" {
		if _, err := pki.AgentURI(id); err != nil {
			problem(w, 400, "invalid_agent_id", "canonical UUID required")
			return
		}
	}
	limit, err := number(r, "limit", 100, 1, 200)
	if err != nil {
		problem(w, 400, "invalid_limit", err.Error())
		return
	}
	var items any
	switch r.URL.Path {
	case "/api/pki/bootstrap-tokens":
		items, err = repo.PKITokens(r.Context(), id, limit)
	case "/api/pki/certificates":
		items, err = repo.PKICertificates(r.Context(), id, limit)
	default:
		items, err = repo.PKIAudit(r.Context(), id, limit)
	}
	if err != nil {
		a.pkiFail(w, err)
		return
	}
	write(w, 200, map[string]any{"items": items})
}
