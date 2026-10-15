package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
)

type resourceHealthRepository interface {
	ResourceHealth(context.Context, string, string, string, string) ([]health.Observation, error)
}

func (a *API) resourceHealthRoutes(w http.ResponseWriter, r *http.Request) {
	repo, ok := a.incidents.(resourceHealthRepository)
	if !ok {
		problem(w, 503, "health_unavailable", "resource health not configured")
		return
	}
	kind, device, resource := "interface", r.PathValue("id"), r.PathValue("interface_id")
	if strings.HasPrefix(r.URL.Path, "/api/telephony/") {
		kind = "sip"
		device = ""
		resource = r.PathValue("id")
	}
	if strings.HasPrefix(r.URL.Path, "/api/agents/") {
		kind = "agent"
		device = ""
		resource = r.PathValue("id")
	}
	items, err := repo.ResourceHealth(r.Context(), r.URL.Query().Get("agent_id"), device, kind, resource)
	if err != nil {
		a.fail(w, err)
		return
	}
	status := "unknown"
	active := false
	for _, v := range items {
		if !v.Current {
			continue
		}
		if !active {
			status = "ok"
			active = true
		}
		switch v.Assessment.Status {
		case "critical":
			status = "critical"
		case "warning":
			if status != "critical" {
				status = "warning"
			}
		case "unknown", "pending", "stale":
			if status == "ok" {
				status = "unknown"
			}
		}
	}
	write(w, 200, map[string]any{"status": status, "items": items, "coverage": "observed_components_only", "truncated": len(items) >= 600})
}
