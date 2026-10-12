package api

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/assetlogs"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/logbook"
)

var auditKeyPattern = regexp.MustCompile(`^audit:[a-f0-9]{64}$`)

func logFilter(r *http.Request) (logbook.Filter, error) {
	q := r.URL.Query()
	f := logbook.Filter{AgentID: q.Get("agent_id"), DeviceID: q.Get("device_id"), Source: q.Get("source"), SourceID: q.Get("source_id"), Host: q.Get("host"), Process: q.Get("process"), RecordType: q.Get("record_type"), EventID: q.Get("event_id"), Executable: q.Get("executable"), Q: strings.TrimSpace(q.Get("q")), Contains: q.Get("contains")}
	for _, v := range []string{f.AgentID, f.DeviceID, f.Source, f.SourceID, f.Host, f.Process, f.RecordType, f.EventID, f.Executable, f.Q, f.Contains} {
		if len(v) > 256 {
			return f, errors.New("text filters must be at most 256 bytes")
		}
	}
	if f.Q != "" && f.Contains != "" {
		return f, errors.New("use q or contains, not both")
	}
	var err error
	f.Limit, err = number(r, "limit", 100, 1, 500)
	if err != nil {
		return f, errors.New("limit must be 1..500")
	}
	if value := q.Get("before_id"); value != "" {
		f.BeforeID, err = strconv.ParseInt(value, 10, 64)
		if err != nil || f.BeforeID < 1 {
			return f, errors.New("before_id must be a positive int64")
		}
	}
	parseUint := func(name string) (*uint32, error) {
		v := q.Get(name)
		if v == "" {
			return nil, nil
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return nil, errors.New(name + " must be uint32")
		}
		u := uint32(n)
		return &u, nil
	}
	if q.Get("severity") == "unknown" {
		f.SeverityUnknown = true
	} else {
		f.Severity, err = parseUint("severity")
		if err != nil {
			return f, err
		}
		if f.Severity != nil && *f.Severity > 7 {
			return f, errors.New("severity must be 0..7 or unknown")
		}
	}
	for name, dest := range map[string]**uint32{"facility": &f.Facility, "uid": &f.UID, "auid": &f.AUID, "pid": &f.PID} {
		*dest, err = parseUint(name)
		if err != nil {
			return f, err
		}
	}
	switch q.Get("success") {
	case "":
	case "true":
		v := true
		f.Success = &v
	case "false":
		v := false
		f.Success = &v
	case "unknown":
		f.SuccessUnknown = true
	default:
		return f, errors.New("success must be true, false or unknown")
	}
	for name, dest := range map[string]**time.Time{"from": &f.From, "to": &f.To} {
		if value := q.Get(name); value != "" {
			v, err := time.Parse(time.RFC3339Nano, value)
			v = v.UTC()
			if err != nil || v.Year() < 1 || v.Year() > 9999 {
				return f, errors.New(name + " must be RFC3339 with timezone")
			}
			*dest = &v
		}
	}
	if f.From != nil && f.To != nil && !f.From.Before(*f.To) {
		return f, errors.New("from must be earlier than to")
	}
	if f.Contains != "" && (f.From == nil || f.To == nil || f.To.Sub(*f.From) > 7*24*time.Hour) {
		return f, errors.New("contains requires a from/to window no longer than 7 days")
	}
	return f, nil
}

func (a *API) scopedLogs(w http.ResponseWriter, r *http.Request) (logbook.Filter, bool) {
	f, err := logFilter(r)
	if err != nil {
		problem(w, 400, "invalid_query", err.Error())
		return f, false
	}
	if strings.HasPrefix(r.Pattern, "GET /api/devices/") {
		f.DeviceID = r.PathValue("id")
	}
	if f.DeviceID != "" {
		d, err := a.repo.Device(r.Context(), f.AgentID, f.DeviceID)
		if err != nil {
			a.fail(w, err)
			return f, false
		}
		f.AgentID = d.AgentID
	}
	return f, true
}

func (a *API) logs(w http.ResponseWriter, r *http.Request) {
	f, ok := a.scopedLogs(w, r)
	if !ok {
		return
	}
	f.AuditOnly = strings.Contains(r.Pattern, "/audit")
	page, err := a.repo.Logs(r.Context(), f)
	if err != nil {
		a.fail(w, err)
		return
	}
	if page.Items == nil {
		page.Items = []logbook.Record{}
	}
	if a.assetLogs != nil {
		for i := range page.Items {
			page.Items[i] = assetlogs.SafeLegacy(page.Items[i])
		}
	}
	write(w, 200, page)
}

func (a *API) logRecord(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		problem(w, 400, "invalid_query", "log id must be positive int64")
		return
	}
	agent := r.URL.Query().Get("agent_id")
	if len(agent) > 256 {
		problem(w, 400, "invalid_query", "agent_id too long")
		return
	}
	record, err := a.repo.Log(r.Context(), id, agent)
	if err != nil {
		a.fail(w, err)
		return
	}
	if a.assetLogs != nil {
		record = assetlogs.SafeLegacy(record)
	}
	write(w, 200, record)
}

func (a *API) logsSummary(w http.ResponseWriter, r *http.Request) {
	f, ok := a.scopedLogs(w, r)
	if !ok {
		return
	}
	out, err := a.repo.LogsSummary(r.Context(), f)
	if err != nil {
		a.fail(w, err)
		return
	}
	write(w, 200, out)
}

func (a *API) auditEvents(w http.ResponseWriter, r *http.Request) {
	f, ok := a.scopedLogs(w, r)
	if !ok {
		return
	}
	out, err := a.repo.AuditEvents(r.Context(), f)
	if err != nil {
		a.fail(w, err)
		return
	}
	if out.Items == nil {
		out.Items = []logbook.AuditEvent{}
	}
	write(w, 200, out)
}

func (a *API) auditEvent(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !auditKeyPattern.MatchString(key) {
		problem(w, 400, "invalid_query", "invalid audit event key")
		return
	}
	f, ok := a.scopedLogs(w, r)
	if !ok {
		return
	}
	f.EventKey = key
	f.AuditOnly = true
	page, err := a.repo.Logs(r.Context(), f)
	if err != nil {
		a.fail(w, err)
		return
	}
	if len(page.Items) == 0 && f.BeforeID == 0 {
		problem(w, 404, "not_found", "audit group not found")
		return
	}
	if page.Items == nil {
		page.Items = []logbook.Record{}
	}
	if a.assetLogs != nil {
		for i := range page.Items {
			page.Items[i] = assetlogs.SafeLegacy(page.Items[i])
		}
	}
	write(w, 200, page)
}
