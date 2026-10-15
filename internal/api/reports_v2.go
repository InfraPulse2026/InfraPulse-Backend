package api

import (
	"context"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/commands"
	"github.com/InfraPulse2026/InfraPulse-Backend/internal/reports"
	"net/http"
	"time"
)

type ReportV2Repository interface {
	ReportV2Summary(context.Context, reports.V2Filter) (reports.V2Summary, error)
	CreateReportV2Snapshot(context.Context, reports.V2Filter, string) (reports.V2Snapshot, bool, error)
	ReportV2Snapshot(context.Context, string) (reports.V2Snapshot, error)
	ReportV2Snapshots(context.Context, int, int) ([]reports.V2Snapshot, error)
	SaveReportSchedule(context.Context, string, reports.ScheduleInput) (reports.Schedule, error)
	ReportSchedules(context.Context, int, int) ([]reports.Schedule, error)
}

func (a *API) reportV2Routes(w http.ResponseWriter, r *http.Request) {
	repo, ok := a.reports.(ReportV2Repository)
	if !ok {
		problem(w, 503, "reports_unavailable", "unified report service unavailable")
		return
	}
	if r.URL.Path == "/api/reports/v2/summary" {
		now := time.Now().UTC()
		f := reports.V2Filter{From: now.Add(-24 * time.Hour), To: now, AssetID: r.URL.Query().Get("asset_id"), SiteID: r.URL.Query().Get("site_id"), Scope: r.URL.Query().Get("scope")}
		from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
		if (from == "") != (to == "") {
			a.reportFail(w, reports.ErrInvalid)
			return
		}
		var err error
		if from != "" {
			f.From, err = time.Parse(time.RFC3339Nano, from)
			if err != nil {
				a.reportFail(w, reports.ErrInvalid)
				return
			}
			f.To, err = time.Parse(time.RFC3339Nano, to)
			if err != nil {
				a.reportFail(w, reports.ErrInvalid)
				return
			}
		}
		f.Limit, err = number(r, "limit", 50, 1, 200)
		if err != nil {
			a.reportFail(w, reports.ErrInvalid)
			return
		}
		f, err = f.Normalize(now)
		if err != nil {
			a.reportFail(w, err)
			return
		}
		out, err := repo.ReportV2Summary(r.Context(), f)
		if err != nil {
			a.reportFail(w, err)
			return
		}
		write(w, 200, out)
		return
	}
	if r.Method == "POST" {
		var f reports.V2Filter
		if !accessJSON(w, r, &f) {
			return
		}
		f, err := f.Normalize(time.Now().UTC())
		if err != nil {
			a.reportFail(w, err)
			return
		}
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 || commands.ValidateKey(keys[0]) != nil {
			a.reportFail(w, reports.ErrInvalid)
			return
		}
		out, replay, err := repo.CreateReportV2Snapshot(r.Context(), f, keys[0])
		if err != nil {
			a.reportFail(w, err)
			return
		}
		code := 201
		if replay {
			code = 200
			w.Header().Set("Idempotent-Replay", "true")
		}
		write(w, code, out)
		return
	}
	if id := r.PathValue("id"); id != "" {
		out, err := repo.ReportV2Snapshot(r.Context(), id)
		if err != nil {
			a.reportFail(w, err)
			return
		}
		write(w, 200, out)
		return
	}
	limit, e1 := number(r, "limit", 20, 1, 100)
	offset, e2 := number(r, "offset", 0, 0, 100000)
	if e1 != nil || e2 != nil {
		a.reportFail(w, reports.ErrInvalid)
		return
	}
	out, err := repo.ReportV2Snapshots(r.Context(), limit, offset)
	if err != nil {
		a.reportFail(w, err)
		return
	}
	write(w, 200, map[string]any{"items": out, "limit": limit, "offset": offset})
}
func (a *API) reportScheduleRoutes(w http.ResponseWriter, r *http.Request) {
	repo, ok := a.reports.(ReportV2Repository)
	if !ok {
		problem(w, 503, "reports_unavailable", "report scheduler unavailable")
		return
	}
	if r.Method == "POST" || r.Method == "PUT" {
		var input reports.ScheduleInput
		if !accessJSON(w, r, &input) {
			return
		}
		if input.Validate() != nil {
			a.reportFail(w, reports.ErrInvalid)
			return
		}
		out, err := repo.SaveReportSchedule(r.Context(), r.PathValue("id"), input)
		if err != nil {
			a.reportFail(w, err)
			return
		}
		code := 200
		if r.Method == "POST" {
			code = 201
		}
		write(w, code, out)
		return
	}
	limit, e1 := number(r, "limit", 20, 1, 100)
	offset, e2 := number(r, "offset", 0, 0, 100000)
	if e1 != nil || e2 != nil {
		a.reportFail(w, reports.ErrInvalid)
		return
	}
	out, err := repo.ReportSchedules(r.Context(), limit, offset)
	if err != nil {
		a.reportFail(w, err)
		return
	}
	write(w, 200, map[string]any{"items": out, "limit": limit, "offset": offset})
}
