package api

import (
	"net/http"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/availability"
)

func availabilityIDs(w http.ResponseWriter, r *http.Request) (agentID, deviceID string, ok bool) {
	agentID, deviceID = r.URL.Query().Get("agent_id"), r.PathValue("id")
	if len(agentID) > 256 || len(deviceID) > 256 {
		problem(w, 400, "invalid_query", "agent_id and device_id must be at most 256 bytes")
		return "", "", false
	}
	return agentID, deviceID, true
}

func (a *API) availabilitySummary(w http.ResponseWriter, r *http.Request) {
	agentID, _, ok := availabilityIDs(w, r)
	if !ok {
		return
	}
	summary, err := a.repo.AvailabilitySummary(r.Context(), agentID)
	if err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, summary)
}

func (a *API) deviceAvailability(w http.ResponseWriter, r *http.Request) {
	agentID, deviceID, ok := availabilityIDs(w, r)
	if !ok {
		return
	}
	d, err := a.repo.Device(r.Context(), agentID, deviceID)
	if err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, d.Availability)
}

func (a *API) availabilityHistory(w http.ResponseWriter, r *http.Request) {
	agentID, deviceID, ok := availabilityIDs(w, r)
	if !ok {
		return
	}
	limit, err := number(r, "limit", 100, 1, 1000)
	if err != nil {
		problem(w, 400, "invalid_query", "limit must be 1..1000")
		return
	}
	offset, err := number(r, "offset", 0, 0, 1000000)
	if err != nil {
		problem(w, 400, "invalid_query", "offset must be 0..1000000")
		return
	}
	d, err := a.repo.Device(r.Context(), agentID, deviceID)
	if err != nil {
		a.fail(w, err)
		return
	}
	items, err := a.repo.AvailabilityChanges(r.Context(), d.AgentID, d.DeviceID, limit, offset)
	if err != nil {
		a.fail(w, err)
		return
	}
	if items == nil {
		items = []availability.Change{}
	}
	write(w, 200, map[string]any{"items": items, "limit": limit, "offset": offset})
}
