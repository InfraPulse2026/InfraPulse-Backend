package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/storage"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/telephony"
)

func sipIDs(w http.ResponseWriter, r *http.Request) (agent, target string, ok bool) {
	agent, target = r.URL.Query().Get("agent_id"), r.PathValue("id")
	if len(agent) > 256 || len(target) > 1024 {
		problem(w, 400, "invalid_query", "agent_id exceeds 256 bytes or target_id exceeds 1024 bytes")
		return "", "", false
	}
	return agent, target, true
}
func (a *API) failSIP(w http.ResponseWriter, err error) {
	if errors.Is(err, storage.ErrAmbiguous) {
		problem(w, 409, "ambiguous_target", "specify agent_id query parameter")
		return
	}
	a.fail(w, err)
}
func (a *API) sipTargets(w http.ResponseWriter, r *http.Request) {
	agent, _, ok := sipIDs(w, r)
	if !ok {
		return
	}
	limit, err := number(r, "limit", 100, 1, 500)
	if err != nil {
		problem(w, 400, "invalid_query", "limit must be 1..500")
		return
	}
	offset, err := number(r, "offset", 0, 0, 1000000)
	if err != nil {
		problem(w, 400, "invalid_query", "offset must be 0..1000000")
		return
	}
	f := telephony.Filter{AgentID: agent, TargetID: r.URL.Query().Get("target_id"), Source: r.URL.Query().Get("source"), Transport: r.URL.Query().Get("transport"), Limit: limit, Offset: offset}
	if len(f.TargetID) > 1024 || len(f.Source) > 1024 || f.Transport != "" && f.Transport != "udp" && f.Transport != "tcp" && f.Transport != "tls" {
		problem(w, 400, "invalid_query", "invalid target, source or transport filter")
		return
	}
	items, err := a.repo.TelephonyTargets(r.Context(), f)
	if err != nil {
		a.failSIP(w, err)
		return
	}
	if items == nil {
		items = []telephony.Target{}
	}
	write(w, 200, map[string]any{"items": items, "limit": limit, "offset": offset})
}
func (a *API) sipTarget(w http.ResponseWriter, r *http.Request) {
	agent, target, ok := sipIDs(w, r)
	if !ok {
		return
	}
	t, err := a.repo.TelephonyTarget(r.Context(), agent, target)
	if err != nil {
		a.failSIP(w, err)
		return
	}
	write(w, 200, t)
}
func (a *API) sipSummary(w http.ResponseWriter, r *http.Request) {
	agent, _, ok := sipIDs(w, r)
	if !ok {
		return
	}
	s, err := a.repo.TelephonySummary(r.Context(), agent)
	if err != nil {
		a.failSIP(w, err)
		return
	}
	write(w, 200, s)
}
func (a *API) sipHistory(w http.ResponseWriter, r *http.Request) {
	agent, target, ok := sipIDs(w, r)
	if !ok {
		return
	}
	f := telephony.HistoryFilter{}
	var err error
	f.Limit, err = number(r, "limit", 100, 1, 500)
	if err != nil {
		problem(w, 400, "invalid_query", "limit must be 1..500")
		return
	}
	if value := r.URL.Query().Get("before_id"); value != "" {
		f.BeforeID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || f.BeforeID < 1 {
			problem(w, 400, "invalid_query", "before_id must be a positive int64")
			return
		}
	}
	for name, dest := range map[string]**time.Time{"from": &f.From, "to": &f.To} {
		if value := r.URL.Query().Get(name); value != "" {
			v, err := time.Parse(time.RFC3339Nano, value)
			v = v.UTC()
			if err != nil || v.Year() < 1 || v.Year() > 9999 {
				problem(w, 400, "invalid_query", name+" must be RFC3339 with timezone")
				return
			}
			*dest = &v
		}
	}
	if f.From != nil && f.To != nil && !f.From.Before(*f.To) {
		problem(w, 400, "invalid_query", "from must be earlier than to")
		return
	}
	t, err := a.repo.TelephonyTarget(r.Context(), agent, target)
	if err != nil {
		a.failSIP(w, err)
		return
	}
	f.AgentID, f.TargetID = t.AgentID, t.TargetID
	page, err := a.repo.TelephonyHistory(r.Context(), f)
	if err != nil {
		a.failSIP(w, err)
		return
	}
	if page.Items == nil {
		page.Items = []telephony.Sample{}
	}
	write(w, 200, page)
}
func (a *API) sipMonitoring(w http.ResponseWriter, r *http.Request) {
	agent, target, ok := sipIDs(w, r)
	if !ok {
		return
	}
	t, err := a.repo.TelephonyTarget(r.Context(), agent, target)
	if err != nil {
		a.failSIP(w, err)
		return
	}
	if r.Method == http.MethodGet {
		write(w, 200, t.Monitoring)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var p struct {
		Seconds int `json:"expected_interval_seconds"`
	}
	if err = decoder.Decode(&p); err != nil {
		problem(w, 400, "invalid_policy", "expected one integer expected_interval_seconds")
		return
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		problem(w, 400, "invalid_policy", "expected exactly one JSON object")
		return
	}
	if err = telephony.ValidateInterval(p.Seconds); err != nil {
		problem(w, 400, "invalid_policy", err.Error())
		return
	}
	if err = a.repo.UpdateTelephonyMonitoring(r.Context(), t.AgentID, t.TargetID, p.Seconds); err != nil {
		a.failSIP(w, err)
		return
	}
	write(w, 200, telephony.DescribeInterval(t.TargetID, p.Seconds, "target_override"))
}
