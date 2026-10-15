package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/health"
)

type IncidentRepository interface {
	HealthPolicy() health.Policy
	DeviceHealth(context.Context, string, string) ([]health.Observation, error)
	Incidents(context.Context, health.Filter) ([]health.Incident, error)
	Incident(context.Context, string) (health.Incident, error)
	IncidentTransitions(context.Context, string) ([]health.Transition, error)
	IncidentEvidence(context.Context, string) ([]health.Evidence, error)
	AcknowledgeIncident(context.Context, string) (health.Incident, error)
}

func WithIncidents(repo IncidentRepository) Option { return func(a *API) { a.incidents = repo } }
func (a *API) healthRoutes(w http.ResponseWriter, r *http.Request) {
	if a.incidents == nil {
		problem(w, 503, "health_unavailable", "health service is not configured")
		return
	}
	repo := a.incidents
	if r.URL.Path == "/api/health/rules" {
		write(w, 200, map[string]any{"policy": repo.HealthPolicy(), "resource_policy": health.ResourceDefaults(), "rule_ids": []string{"cpu.sustained", "ram.sustained", "cpu.trend", "interface.oper_down", "interface.utilization", "interface.errors", "interface.drops", "interface.flap", "sip.availability", "sip.latency", "collector.unavailable", "hardware.vendor_health", "log.reported_severity", "audit.reported_failure", "configuration.approved_drift", "topology.peer_changed", "log.kernel_oom_kill", "log.ssh_password_failure"}, "minimum_sustain_samples": 3, "minimum_trend_samples": 6, "minimum_trend_window_seconds": 30, "minimum_trend_r_squared": 0.9, "flap_window_seconds": 300, "flap_warning_changes": 4, "flap_critical_changes": 8, "flap_recovery_seconds": 300, "collector_confirmation_seconds": 15})
		return
	}
	if r.URL.Path == "/api/devices/"+r.PathValue("id")+"/health" {
		items, err := repo.DeviceHealth(r.Context(), r.URL.Query().Get("agent_id"), r.PathValue("id"))
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
			case "unknown", "stale", "pending":
				if status == "ok" {
					status = "unknown"
				}
			}
		}
		write(w, 200, map[string]any{"status": status, "items": items, "coverage": "observed_components_only", "truncated": len(items) >= 600})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		limit, e1 := number(r, "limit", 100, 1, 200)
		offset, e2 := number(r, "offset", 0, 0, 100000)
		state := r.URL.Query().Get("state")
		if e1 != nil || e2 != nil || state != "" && state != "open" && state != "acknowledged" && state != "resolved" {
			problem(w, 400, "invalid_query", "invalid state or pagination")
			return
		}
		kind := r.URL.Query().Get("resource_kind")
		if kind != "" && kind != "device" && kind != "interface" && kind != "sip" && kind != "agent" && kind != "external" && kind != "hardware" && kind != "log" && kind != "configuration" && kind != "topology" {
			problem(w, 400, "invalid_query", "invalid resource_kind")
			return
		}
		f := health.Filter{AgentID: r.URL.Query().Get("agent_id"), DeviceID: r.URL.Query().Get("device_id"), State: state, RuleID: r.URL.Query().Get("rule_id"), Source: r.URL.Query().Get("source"), ResourceKind: kind, ResourceID: r.URL.Query().Get("resource_id"), Limit: limit, Offset: offset}
		items, err := repo.Incidents(r.Context(), f)
		if err != nil {
			a.fail(w, err)
			return
		}
		write(w, 200, map[string]any{"items": items, "limit": limit, "offset": offset})
		return
	}
	if r.Method == "POST" {
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
		item, err := repo.AcknowledgeIncident(r.Context(), id)
		if errors.Is(err, health.ErrConflict) {
			problem(w, 409, "incident_resolved", "resolved incident cannot be acknowledged")
			return
		}
		if err != nil {
			a.fail(w, err)
			return
		}
		write(w, 200, item)
		return
	}
	item, err := repo.Incident(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	if r.URL.Path == "/api/incidents/"+id {
		write(w, 200, item)
		return
	}
	var items any
	if r.URL.Path == "/api/incidents/"+id+"/evidence" {
		items, err = repo.IncidentEvidence(r.Context(), id)
	} else {
		items, err = repo.IncidentTransitions(r.Context(), id)
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, map[string]any{"items": items})
}
