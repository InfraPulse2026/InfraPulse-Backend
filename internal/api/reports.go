package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/reports"
)

type ReportRepository interface {
	ReportSummary(context.Context, reports.Filter) (reports.Summary, error)
	CreateReportSnapshot(context.Context, reports.Filter, string) (reports.Snapshot, bool, error)
	ReportSnapshot(context.Context, string) (reports.Snapshot, error)
	ReportSnapshots(context.Context, int, int) ([]reports.Snapshot, error)
}

func WithReports(repo ReportRepository) Option { return func(a *API) { a.reports = repo } }
func reportFilter(r *http.Request, now time.Time) (reports.Filter, error) {
	f := reports.Filter{From: now.Add(-24 * time.Hour), To: now, AgentID: r.URL.Query().Get("agent_id"), Source: r.URL.Query().Get("source"), ResourceKind: r.URL.Query().Get("resource_kind")}
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if (from == "") != (to == "") {
		return f, reports.ErrInvalid
	}
	if from != "" {
		var err error
		f.From, err = time.Parse(time.RFC3339Nano, from)
		if err != nil {
			return f, reports.ErrInvalid
		}
		f.To, err = time.Parse(time.RFC3339Nano, to)
		if err != nil {
			return f, reports.ErrInvalid
		}
	}
	limit, err := number(r, "top_limit", 20, 1, 100)
	if err != nil {
		return f, reports.ErrInvalid
	}
	f.TopLimit = limit
	return f.Normalize(now)
}
func (a *API) reportFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, reports.ErrInvalid):
		problem(w, 400, "invalid_report", "period must be 1s..31d, end must not be future; use RFC3339 and valid filters")
	case errors.Is(err, reports.ErrConflict):
		problem(w, 409, "idempotency_conflict", "key belongs to different report request")
	case errors.Is(err, reports.ErrNotFound):
		problem(w, 404, "not_found", "report snapshot not found")
	default:
		a.fail(w, err)
	}
}
func (a *API) reportRoutes(w http.ResponseWriter, r *http.Request) {
	if a.reports == nil {
		problem(w, 503, "reports_unavailable", "reports are not configured")
		return
	}
	download := false
	if value := r.URL.Query().Get("download"); value != "" {
		var err error
		download, err = strconv.ParseBool(value)
		if err != nil {
			problem(w, 400, "invalid_query", "download must be true or false")
			return
		}
	}
	if r.Method == "POST" {
		var f reports.Filter
		if !accessJSON(w, r, &f) {
			return
		}
		normalized, err := f.Normalize(time.Now())
		if err != nil {
			a.reportFail(w, err)
			return
		}
		f = normalized
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 || commands.ValidateKey(keys[0]) != nil {
			problem(w, 400, "invalid_request", "one valid Idempotency-Key required")
			return
		}
		snapshot, replay, err := a.reports.CreateReportSnapshot(r.Context(), f, keys[0])
		if err != nil {
			a.reportFail(w, err)
			return
		}
		code := 201
		if replay {
			code = 200
			w.Header().Set("Idempotent-Replay", "true")
		}
		write(w, code, snapshot)
		return
	}
	if r.URL.Path == "/api/reports/summary" {
		f, err := reportFilter(r, time.Now().UTC())
		if err != nil {
			a.reportFail(w, err)
			return
		}
		summary, err := a.reports.ReportSummary(r.Context(), f)
		if err != nil {
			a.reportFail(w, err)
			return
		}
		if download {
			w.Header().Set("Content-Disposition", `attachment; filename="infrapulse-report.json"`)
		}
		write(w, 200, summary)
		return
	}
	if id := r.PathValue("id"); id != "" {
		snapshot, err := a.reports.ReportSnapshot(r.Context(), id)
		if err != nil {
			a.reportFail(w, err)
			return
		}
		if download {
			w.Header().Set("Content-Disposition", `attachment; filename="infrapulse-report-snapshot.json"`)
		}
		write(w, 200, snapshot)
		return
	}
	limit, e1 := number(r, "limit", 20, 1, 100)
	offset, e2 := number(r, "offset", 0, 0, 100000)
	if e1 != nil || e2 != nil {
		problem(w, 400, "invalid_query", "invalid pagination")
		return
	}
	items, err := a.reports.ReportSnapshots(r.Context(), limit, offset)
	if err != nil {
		a.reportFail(w, err)
		return
	}
	write(w, 200, map[string]any{"items": items, "limit": limit, "offset": offset})
}
