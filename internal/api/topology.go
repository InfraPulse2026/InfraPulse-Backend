package api

import "net/http"

func (a *API) topology(w http.ResponseWriter, r *http.Request) {
	if view := r.URL.Query().Get("view"); view != "" {
		if view != "global" {
			problem(w, 400, "invalid_query", "view must be global")
			return
		}
		a.globalTopology(w, r)
		return
	}
	agentID, deviceID := r.URL.Query().Get("agent_id"), r.URL.Query().Get("device_id")
	if id := r.PathValue("id"); id != "" {
		deviceID = id
	}
	if len(agentID) > 256 || len(deviceID) > 256 {
		problem(w, 400, "invalid_query", "agent_id and device_id must be at most 256 bytes")
		return
	}
	includeExpired := false
	switch r.URL.Query().Get("include_expired") {
	case "", "false":
	case "true":
		includeExpired = true
	default:
		problem(w, 400, "invalid_query", "include_expired must be true or false")
		return
	}
	limit, err := number(r, "limit", 500, 1, 2000)
	if err != nil {
		problem(w, 400, "invalid_query", "limit must be 1..2000")
		return
	}
	offset, err := number(r, "offset", 0, 0, 1000000)
	if err != nil {
		problem(w, 400, "invalid_query", "offset must be 0..1000000")
		return
	}
	if deviceID != "" {
		d, err := a.repo.Device(r.Context(), agentID, deviceID)
		if err != nil {
			a.fail(w, err)
			return
		}
		agentID = d.AgentID
	}
	g, err := a.repo.Topology(r.Context(), agentID, deviceID, includeExpired, limit, offset)
	if err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, g)
}
